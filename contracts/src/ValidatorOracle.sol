// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IPriceOracle} from "./interfaces/IPriceOracle.sol";

/// @title  ValidatorOracle
/// @notice On-chain QRL/USD price oracle. Each active beacon-chain
///         validator submits a price vote as a regular EVM
///         transaction; the contract exposes the unweighted median
///         of fresh votes via the IPriceOracle interface.
///
/// @dev    Membership comes from consensus, not from the contract
///         owner. The chain's beacon engine system-calls
///         `setValidatorSet(addresses)` from the sentinel
///         SYSTEM_CALLER address on every block to mirror the
///         current PoS validator set into EVM storage. The contract
///         has no other admin entrypoints — there's nothing to
///         govern, and nothing to compromise.
contract ValidatorOracle is IPriceOracle {
    /// @notice Sentinel sender consensus uses when invoking system
    ///         calls. Mirrors the convention used by other consensus
    ///         hooks on this chain (see PayWithIQRL).
    address public constant SYSTEM_CALLER = 0xffffFFFfFFffffffffffffffFfFFFfffFFFfFFfE;

    /// @notice Upper bound on validator-set size. Bounds the median
    ///         computation cost (insertion sort on N elements).
    uint256 public constant MAX_VALIDATORS = 100;

    /// @notice A vote is considered stale if it was posted more than
    ///         `voteStalenessBlocks` blocks ago. Configured at
    ///         construction.
    uint256 public immutable voteStalenessBlocks;

    /// @notice Minimum fraction of validators that must have fresh
    ///         votes for `healthy()` to return true. Expressed as
    ///         numerator/denominator.
    uint256 public immutable minQuorumNumerator;
    uint256 public immutable minQuorumDenominator;

    struct Vote {
        uint128 price;
        uint64 blockNumber;
    }

    /// @notice Active validator set as of the most recent
    ///         setValidatorSet system call.
    address[] public validators;

    /// @notice 1-indexed position in `validators`. 0 means not a
    ///         validator. Using 1-index lets us distinguish
    ///         "missing" from "index 0".
    mapping(address validator => uint256 indexPlus1) public validatorIndex;

    /// @notice Most recent vote from each currently-active validator.
    ///         Cleared when a validator drops out of the set.
    mapping(address validator => Vote vote) public votes;

    /// @dev Per-block cache of the median price and health flag.
    ///      Refreshed by every state-changing entrypoint
    ///      (submitVote, setValidatorSet) and by pokeCache(). Reads
    ///      hit the cache when cachedAtBlock == block.number;
    ///      otherwise price() / healthy() recompute from raw votes.
    struct ViewCache {
        uint128 medianPrice;   // 16 bytes
        uint64 cachedAtBlock;  // 8 bytes
        uint8 healthyFlag;     // 1 byte (0 or 1)
        // 7 bytes implicit padding
    }

    ViewCache public cache;

    error NotValidator();
    error NotSystemCaller();
    error ValidatorLimitReached();
    error ZeroPrice();
    error PriceTooLarge();
    error BadQuorum();
    error WrongBlockNumber(uint256 forBlock, uint256 currentBlock);

    event ValidatorSetChanged(address[] added, address[] removed);
    event VoteSubmitted(address indexed validator, uint256 priceUsd1e18, uint256 blockNumber);

    constructor(
        uint256 _voteStalenessBlocks,
        uint256 _minQuorumNumerator,
        uint256 _minQuorumDenominator
    ) {
        if (_minQuorumDenominator == 0 || _minQuorumNumerator > _minQuorumDenominator) {
            revert BadQuorum();
        }
        voteStalenessBlocks = _voteStalenessBlocks;
        minQuorumNumerator = _minQuorumNumerator;
        minQuorumDenominator = _minQuorumDenominator;
    }

    // ------------------------------------------------------------------
    // Validator set (consensus-driven)
    // ------------------------------------------------------------------

    /// @notice Replace the active validator set with `newSet`.
    ///         Callable only by the consensus engine via system call.
    /// @dev    Diffs against the current set:
    ///           - addresses in `newSet` but not currently registered
    ///             are appended;
    ///           - currently-registered addresses not in `newSet` are
    ///             removed via swap-and-pop, and their stale vote is
    ///             deleted so it can't leak into a future tenure if
    ///             the same address is re-registered later;
    ///           - addresses already registered AND in `newSet` keep
    ///             their existing vote.
    ///
    ///         Duplicates within `newSet` are silently deduplicated
    ///         (a second occurrence of an already-registered address
    ///         is a no-op).
    function setValidatorSet(address[] calldata newSet) external {
        if (msg.sender != SYSTEM_CALLER) revert NotSystemCaller();
        if (newSet.length > MAX_VALIDATORS) revert ValidatorLimitReached();

        // Phase 1 — collect addresses to remove. A current validator
        // is removed iff it is not present in newSet. We snapshot
        // into memory so the array-mutating loop below is well-
        // defined.
        uint256 oldLen = validators.length;
        address[] memory toRemove = new address[](oldLen);
        uint256 nRemove = 0;
        for (uint256 i = 0; i < oldLen; i++) {
            address v = validators[i];
            bool stillActive = false;
            for (uint256 j = 0; j < newSet.length; j++) {
                if (newSet[j] == v) {
                    stillActive = true;
                    break;
                }
            }
            if (!stillActive) {
                toRemove[nRemove++] = v;
            }
        }

        // Phase 2 — remove. Swap-and-pop, clean up validatorIndex
        // and the now-orphaned vote.
        for (uint256 i = 0; i < nRemove; i++) {
            address v = toRemove[i];
            uint256 idx1 = validatorIndex[v];
            uint256 lastIdx = validators.length - 1;
            address lastAddr = validators[lastIdx];
            validators[idx1 - 1] = lastAddr;
            validatorIndex[lastAddr] = idx1;
            validators.pop();
            delete validatorIndex[v];
            delete votes[v];
        }

        // Phase 3 — add. Anyone in newSet with no current index gets
        // pushed; duplicates skipped via the index check.
        address[] memory added = new address[](newSet.length);
        uint256 nAdded = 0;
        for (uint256 j = 0; j < newSet.length; j++) {
            address v = newSet[j];
            if (validatorIndex[v] != 0) continue;
            validators.push(v);
            validatorIndex[v] = validators.length;
            added[nAdded++] = v;
        }

        // Tighten the added/removed slices for the event.
        assembly ("memory-safe") {
            mstore(added, nAdded)
            mstore(toRemove, nRemove)
        }

        _updateCache();
        emit ValidatorSetChanged(added, toRemove);
    }

    function validatorCount() external view returns (uint256) {
        return validators.length;
    }

    // ------------------------------------------------------------------
    // Vote submission
    // ------------------------------------------------------------------

    /// @notice Submit the caller's current QRL/USD vote, bound to a
    ///         specific block. Reverts unless `forBlockNumber` equals
    ///         the block in which the tx executes; this prevents a
    ///         late-mining tx from overwriting a fresher vote with a
    ///         stale price.
    /// @param  forBlockNumber Block in which the caller intends this
    ///                        vote to land. Must equal `block.number`.
    /// @param  priceUsd1e18   Price quoted in USD, scaled by 1e18
    ///                        (1e18 == $1.00).
    function submitVote(uint256 forBlockNumber, uint256 priceUsd1e18) external {
        if (forBlockNumber != block.number) {
            revert WrongBlockNumber(forBlockNumber, block.number);
        }
        if (validatorIndex[msg.sender] == 0) revert NotValidator();
        if (priceUsd1e18 == 0) revert ZeroPrice();
        if (priceUsd1e18 > type(uint128).max) revert PriceTooLarge();

        votes[msg.sender] = Vote({
            price: uint128(priceUsd1e18),
            blockNumber: uint64(block.number)
        });
        _updateCache();
        emit VoteSubmitted(msg.sender, priceUsd1e18, block.number);
    }

    // ------------------------------------------------------------------
    // IPriceOracle
    // ------------------------------------------------------------------

    function price() external view returns (uint256) {
        ViewCache memory c = cache;
        if (c.cachedAtBlock == uint64(block.number)) {
            return c.medianPrice;
        }
        (uint256[] memory fresh, uint256 n) = _freshPrices();
        if (n == 0) return 0;
        return _median(fresh, n);
    }

    function healthy() external view returns (bool) {
        uint256 total = validators.length;
        if (total == 0) return false;

        ViewCache memory c = cache;
        if (c.cachedAtBlock == uint64(block.number)) {
            return c.healthyFlag == 1;
        }
        (, uint256 n) = _freshPrices();
        return n * minQuorumDenominator >= minQuorumNumerator * total;
    }

    function pokeCache() external {
        _updateCache();
    }

    // ------------------------------------------------------------------
    // Internals
    // ------------------------------------------------------------------

    function _freshPrices() internal view returns (uint256[] memory prices, uint256 n) {
        uint256 total = validators.length;
        prices = new uint256[](total);

        uint256 threshold = block.number > voteStalenessBlocks
            ? block.number - voteStalenessBlocks
            : 0;

        for (uint256 i = 0; i < total; i++) {
            Vote memory v = votes[validators[i]];
            if (v.price > 0 && uint256(v.blockNumber) >= threshold) {
                prices[n++] = v.price;
            }
        }
    }

    function _updateCache() internal {
        (uint256[] memory fresh, uint256 n) = _freshPrices();
        uint256 med = n == 0 ? 0 : _median(fresh, n);

        uint256 total = validators.length;
        bool isHealthy = total > 0
            && n * minQuorumDenominator >= minQuorumNumerator * total;

        cache = ViewCache({
            medianPrice: uint128(med),
            cachedAtBlock: uint64(block.number),
            healthyFlag: isHealthy ? 1 : 0
        });
    }

    function _median(uint256[] memory prices, uint256 n) internal pure returns (uint256) {
        for (uint256 i = 1; i < n; i++) {
            uint256 key = prices[i];
            uint256 j = i;
            while (j > 0 && prices[j - 1] > key) {
                prices[j] = prices[j - 1];
                unchecked { j--; }
            }
            prices[j] = key;
        }
        if (n % 2 == 1) {
            return prices[n / 2];
        }
        return (prices[n / 2 - 1] + prices[n / 2]) / 2;
    }
}
