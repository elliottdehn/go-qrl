// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {ERC20Burnable} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Burnable.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

import {IPriceOracle} from "./interfaces/IPriceOracle.sol";

/// @dev Subset of the QSD pool that this contract reads. Matches the
///      external view surface of `QSD.priceCumulativeNow()`. Defining
///      it here as an interface (rather than importing the full
///      contract) avoids a deploy-time circular dependency: QSD also
///      references the iQRL token by address.
interface IQsdPool {
    function priceCumulativeNow() external view returns (uint256 cumulative, uint64 timestamp);
}

/// @title  Inverse Quantum-Resistant Ledger Token (iQRL)
/// @notice ERC-20 token whose canonical USD value tracks 1/p, where p
///         is the QRL/USD oracle price. Minted by permanently
///         destroying QRL at the rate 1/p^2 QRL per iQRL (plus a small
///         fee that is also destroyed). Non-redeemable: the only way
///         to destroy iQRL is via burn() / burnFrom() — typically
///         called to pay network fees or settle off-chain accounts.
///
/// @dev    Properties
///         ----------
///         - Mint is payable and consumes native QRL via msg.value.
///           QRL paid is permanently held by this contract: there is
///           no withdrawal path, no admin, no upgrade, no escape
///           hatch. The held balance is effectively burned from QRL's
///           circulating supply.
///         - There is no reverse operation (no iQRL -> QRL). This
///           eliminates the bank-run vector that a redemption path
///           would create for a 1/p-priced liability.
///         - Mint is rate-limited per-block to bound oracle-lag
///           arbitrage extraction.
///         - 0.5% upfront mint fee (also burned) compensates QRL
///           holders for the minter's oracle-lag option value.
contract InverseQRL is ERC20, ERC20Burnable, ReentrancyGuard {
    /// @notice 1e18 fixed-point scale used throughout.
    uint256 internal constant SCALE = 1e18;

    /// @notice Mint fee in basis points of the pre-fee QRL cost.
    ///         50 bps = 0.5%. Fee is destroyed along with the base cost.
    uint256 public constant MINT_FEE_BPS = 50;

    /// @notice Per-block aggregate mint cap, in parts-per-million of
    ///         total iQRL supply. 25 ppm = 0.0025%. Limits arbitrage
    ///         extraction per oracle update.
    uint256 public constant MINT_CAP_PPM = 25;

    /// @notice Absolute floor on the per-block mint cap. Until iQRL
    ///         supply grows large enough for MINT_CAP_PPM to dominate,
    ///         at most MIN_CAP_ABSOLUTE iQRL may be minted per block.
    uint256 public constant MIN_CAP_ABSOLUTE = 1 ether;

    /// @notice The QRL/USD oracle.
    IPriceOracle public immutable oracle;

    /// @notice The QSD pool, used as a second price source for mint
    ///         pricing (TWAP via priceCumulativeNow). Mint takes
    ///         `min(oracle, pool TWAP)` so an attacker who corrupts
    ///         only one of the two cannot drive the mint price up
    ///         and extract cheap iQRL.
    IQsdPool public immutable qsdPool;

    /// @notice Minimum elapsed time between a mint's pool snapshots
    ///         for the TWAP to be considered valid. Below this
    ///         window we fall back to the oracle alone (or revert
    ///         if the oracle is also unhealthy). 5 minutes is short
    ///         enough to keep mint usable in normal operation,
    ///         long enough that intra-block flash manipulation
    ///         can't move the integrated TWAP meaningfully.
    uint64 public constant MIN_TWAP_WINDOW = 5 minutes;

    /// @notice Aggregate iQRL minted per block, for cap enforcement.
    mapping(uint256 blockNumber => uint256 minted) public mintedInBlock;

    /// @notice Pool-cumulative price snapshot from the most recent
    ///         successful mint. Used together with the current
    ///         priceCumulativeNow() reading to compute a TWAP over
    ///         the inter-mint interval.
    uint256 public twapSnapshotCumulative;

    /// @notice block.timestamp of the last successful mint's pool
    ///         snapshot. Zero means "no snapshot yet": the next mint
    ///         falls back to oracle-only pricing.
    uint64 public twapSnapshotTime;

    error OracleUnhealthy();
    error PriceUnavailable();
    error ZeroAmount();
    error MintCapExceeded(uint256 requested, uint256 available);
    error InsufficientPayment(uint256 required, uint256 supplied);
    error RefundFailed();

    event Minted(
        address indexed minter,
        uint256 iqrlAmount,
        uint256 qrlBaseDestroyed,
        uint256 qrlFeeDestroyed,
        uint256 priceAtMint
    );

    constructor(IPriceOracle _oracle, IQsdPool _qsdPool) ERC20("Inverse QRL", "iQRL") {
        oracle = _oracle;
        qsdPool = _qsdPool;
    }

    // ------------------------------------------------------------------
    // Mint
    // ------------------------------------------------------------------

    /// @notice Mint `iqrlAmount` new iQRL by destroying QRL at the
    ///         rate 1/p^2 QRL per iQRL, plus a MINT_FEE_BPS fee.
    ///         The caller supplies QRL via msg.value; any surplus over
    ///         the computed cost is refunded.
    /// @param  iqrlAmount Desired iQRL output.
    /// @return qrlDestroyed Total QRL destroyed (base cost + fee).
    /// @dev    Reverts if the oracle is unhealthy, if msg.value is
    ///         insufficient, or if the per-block cap would be exceeded.
    ///         msg.value functions as the slippage cap: pass exactly
    ///         the quoted cost to disallow any movement, or more to
    ///         tolerate unfavorable price drift.
    function mint(uint256 iqrlAmount)
        external
        payable
        nonReentrant
        returns (uint256 qrlDestroyed)
    {
        if (iqrlAmount == 0) revert ZeroAmount();

        // Per-block cap check (independent of price source).
        uint256 cap = _mintCapForBlock();
        uint256 already = mintedInBlock[block.number];
        if (already + iqrlAmount > cap) {
            revert MintCapExceeded(already + iqrlAmount, cap);
        }

        // Establish the mint price `p` as min(oracle, pool TWAP).
        // Lower p means HIGHER mint cost (cost = iqrlAmount / p^2),
        // so taking the min charges the higher of the two cost
        // estimates and removes the up-manipulation surface from
        // either source individually. Either source can be missing
        // (oracle unhealthy / TWAP not yet established); we revert
        // only if BOTH are unavailable.
        uint256 p = _mintPrice();

        // Compute QRL cost: (iqrlAmount / p) * (1/p) = iqrlAmount / p^2.
        // Evaluated as two sequential mul/div to keep intermediate
        // values bounded and match the USD-denominated mental model.
        uint256 baseCost = (iqrlAmount * SCALE / p) * SCALE / p;
        uint256 fee = baseCost * MINT_FEE_BPS / 10_000;
        qrlDestroyed = baseCost + fee;

        if (msg.value < qrlDestroyed) {
            revert InsufficientPayment(qrlDestroyed, msg.value);
        }

        mintedInBlock[block.number] = already + iqrlAmount;
        _mint(msg.sender, iqrlAmount);

        // Update the TWAP snapshot anchor to the current pool state.
        // The next mint's TWAP window is measured from here.
        (twapSnapshotCumulative, twapSnapshotTime) = qsdPool.priceCumulativeNow();

        // Refund excess payment last, after all state mutations
        // (CEI). The contract has no other path to release native
        // QRL, so the post-call invariant address(this).balance ==
        // qrlDestroyed-since-genesis still holds.
        uint256 surplus = msg.value - qrlDestroyed;
        if (surplus > 0) {
            (bool ok, ) = msg.sender.call{value: surplus}("");
            if (!ok) revert RefundFailed();
        }

        emit Minted(msg.sender, iqrlAmount, baseCost, fee, p);
    }

    /// @dev Resolve the mint price `p` (1e18 scale) as the minimum
    ///      of the two available sources, with a hard revert when
    ///      neither is available.
    ///
    ///        - Oracle source: oracle.price() if oracle.healthy(),
    ///          else "infinity" (i.e. excluded from the min).
    ///        - Pool source: TWAP across the inter-mint window if
    ///          a snapshot exists AND the window is at least
    ///          MIN_TWAP_WINDOW seconds. Below the window we treat
    ///          the pool source as unavailable (flash-loan
    ///          resistance). The bootstrap mint, before any
    ///          snapshot has been recorded, also treats it as
    ///          unavailable.
    function _mintPrice() internal view returns (uint256 p) {
        uint256 pOracle = oracle.healthy() ? oracle.price() : type(uint256).max;
        uint256 pTwap = _twapPrice();

        // min(pOracle, pTwap), with type(uint256).max acting as the
        // "unavailable" sentinel for either side.
        p = pOracle < pTwap ? pOracle : pTwap;
        if (p == type(uint256).max || p == 0) revert PriceUnavailable();
    }

    /// @dev Returns the pool's TWAP from the previous mint's
    ///      snapshot to the current block, or `type(uint256).max`
    ///      when the TWAP is not yet usable (no snapshot, or
    ///      window < MIN_TWAP_WINDOW). Pure-view; does not mutate.
    function _twapPrice() internal view returns (uint256) {
        uint64 prevTime = twapSnapshotTime;
        if (prevTime == 0) return type(uint256).max;
        (uint256 curCum, uint64 curTime) = qsdPool.priceCumulativeNow();
        if (curTime <= prevTime) return type(uint256).max;
        uint64 window = curTime - prevTime;
        if (window < MIN_TWAP_WINDOW) return type(uint256).max;
        return (curCum - twapSnapshotCumulative) / uint256(window);
    }

    /// @notice Quote the QRL cost for minting `iqrlAmount` iQRL at
    ///         the current mint price (`min(oracle, pool TWAP)`).
    /// @return qrlTotal   Total QRL required (base cost + fee).
    /// @return feePortion Portion of the total that is fee.
    /// @dev    Reverts if neither price source is available.
    function quoteMint(uint256 iqrlAmount)
        external
        view
        returns (uint256 qrlTotal, uint256 feePortion)
    {
        uint256 p = _mintPrice();
        uint256 baseCost = (iqrlAmount * SCALE / p) * SCALE / p;
        feePortion = baseCost * MINT_FEE_BPS / 10_000;
        qrlTotal = baseCost + feePortion;
    }

    // ------------------------------------------------------------------
    // View helpers
    // ------------------------------------------------------------------

    /// @notice Current per-block aggregate mint cap in iQRL.
    function mintCapForBlock() external view returns (uint256) {
        return _mintCapForBlock();
    }

    /// @notice Amount of iQRL still mintable in the current block.
    function remainingMintBudget() external view returns (uint256) {
        uint256 cap = _mintCapForBlock();
        uint256 already = mintedInBlock[block.number];
        return already >= cap ? 0 : cap - already;
    }

    /// @notice Cumulative QRL destroyed through this mint facility.
    /// @dev    Reads the contract's native balance. The contract has
    ///         no withdrawal path, so this strictly grows over time
    ///         under normal operation. (A SELFDESTRUCT victim could
    ///         force-credit additional balance; that path is a
    ///         protocol-wide concern, not specific to this contract.)
    function qrlPermanentlyDestroyed() external view returns (uint256) {
        return address(this).balance;
    }

    function _mintCapForBlock() internal view returns (uint256) {
        uint256 supplyCap = totalSupply() * MINT_CAP_PPM / 1_000_000;
        return supplyCap > MIN_CAP_ABSOLUTE ? supplyCap : MIN_CAP_ABSOLUTE;
    }
}
