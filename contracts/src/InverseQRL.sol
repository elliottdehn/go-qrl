// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {ERC20Burnable} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Burnable.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

import {IPriceOracle} from "./interfaces/IPriceOracle.sol";

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

    /// @notice Aggregate iQRL minted per block, for cap enforcement.
    mapping(uint256 blockNumber => uint256 minted) public mintedInBlock;

    error OracleUnhealthy();
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

    constructor(IPriceOracle _oracle) ERC20("Inverse QRL", "iQRL") {
        oracle = _oracle;
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
        if (!oracle.healthy()) revert OracleUnhealthy();

        // Per-block cap check.
        uint256 cap = _mintCapForBlock();
        uint256 already = mintedInBlock[block.number];
        if (already + iqrlAmount > cap) {
            revert MintCapExceeded(already + iqrlAmount, cap);
        }

        // Compute QRL cost: (iqrlAmount / p) * (1/p) = iqrlAmount / p^2.
        // Evaluated as two sequential mul/div to keep intermediate
        // values bounded and match the USD-denominated mental model.
        uint256 p = oracle.price();
        uint256 baseCost = (iqrlAmount * SCALE / p) * SCALE / p;
        uint256 fee = baseCost * MINT_FEE_BPS / 10_000;
        qrlDestroyed = baseCost + fee;

        if (msg.value < qrlDestroyed) {
            revert InsufficientPayment(qrlDestroyed, msg.value);
        }

        mintedInBlock[block.number] = already + iqrlAmount;
        _mint(msg.sender, iqrlAmount);

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

    /// @notice Quote the QRL cost for minting `iqrlAmount` iQRL.
    /// @return qrlTotal   Total QRL required (base cost + fee).
    /// @return feePortion Portion of the total that is fee.
    /// @dev    Reverts if the oracle is unhealthy; a stale or zero
    ///         price would produce misleading quotes.
    function quoteMint(uint256 iqrlAmount)
        external
        view
        returns (uint256 qrlTotal, uint256 feePortion)
    {
        if (!oracle.healthy()) revert OracleUnhealthy();
        uint256 p = oracle.price();
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
