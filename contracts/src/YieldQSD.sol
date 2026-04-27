// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @title  yQSD — QSD Yield Claim
/// @notice ERC-20 token co-minted 1:1 with QSD at the moment of QSD
///         issuance and burned 1:1 at redemption. Holders receive
///         iQRL rewards (paid by the QSD pool's leverage facility)
///         pro-rata via a standard MasterChef accumulator.
///
/// @dev    Design: principal/yield separation at issuance.
///         ----------------------------------------------
///         - When a depositor mints QSD via the pool, they receive
///           QSD AND yQSD in equal amounts. To redeem QSD, the same
///           wallet must hold (and burn) the matching yQSD. Secondary
///           buyers of QSD do not have yield rights and cannot
///           redeem unless they also acquire yQSD on the open market.
///         - yQSD is freely transferable. In practice most holders
///           will keep it inert in their wallet, accumulating iQRL.
///           A market in yQSD may develop for sophisticated
///           yield-trading; the design accommodates either.
///         - The yield primitive is fully decoupled from QSD's role
///           as a stablecoin: QSD can be LP'd, used as collateral,
///           or held in cold storage without forfeiting yield.
///         - Mint / burn are gated to the QSD contract via an
///           onlyQsd modifier. This is the only privileged surface;
///           distribute() and claim() are permissionless.
contract YieldQSD is ERC20, ReentrancyGuard {
    using SafeERC20 for IERC20;

    /// @notice 1e18 fixed-point scale.
    uint256 internal constant SCALE = 1e18;

    /// @notice The QSD token. Authorized to call issueTo / redeemFrom.
    IERC20 public immutable qsd;

    /// @notice The reward token (iQRL).
    IERC20 public immutable iqrl;

    /// @notice Cumulative iQRL rewards per yQSD share, 1e18-scaled.
    ///         Monotone non-decreasing.
    uint256 public accRewardPerShare;

    /// @notice Per-user snapshot of accRewardPerShare * balance / SCALE
    ///         at last balance change. The standard MasterChef
    ///         "rewardDebt" — represents iQRL already credited.
    mapping(address => uint256) public rewardDebt;

    /// @notice Settled-but-not-yet-claimed iQRL rewards per user.
    ///         Drained by claim().
    mapping(address => uint256) public pendingReward;

    /// @notice Cumulative iQRL distributed (audit / view convenience).
    uint256 public totalDistributed;

    error ZeroAmount();
    error OnlyQsd();
    error InsufficientYieldClaim(address holder, uint256 required, uint256 actual);

    event Issued(address indexed to, uint256 amount);
    event Redeemed(address indexed from, uint256 amount);
    event Claimed(address indexed user, uint256 amount);
    event Distributed(uint256 amount, uint256 newAccRewardPerShare);

    modifier onlyQsd() {
        if (msg.sender != address(qsd)) revert OnlyQsd();
        _;
    }

    constructor(IERC20 _qsd, IERC20 _iqrl) ERC20("Quantum Stable Dollar Yield Claim", "yQSD") {
        qsd = _qsd;
        iqrl = _iqrl;
    }

    // ------------------------------------------------------------------
    // Issue / Redeem (QSD-gated; called from QSD.deposit / QSD.redeem)
    // ------------------------------------------------------------------

    /// @notice Mint `amount` yQSD to `to`. Called by QSD when QSD is
    ///         minted to a depositor.
    /// @dev    Restricted to the QSD contract.
    function issueTo(address to, uint256 amount) external onlyQsd {
        if (amount == 0) revert ZeroAmount();
        _mint(to, amount);
        emit Issued(to, amount);
    }

    /// @notice Burn `amount` yQSD from `from`. Called by QSD when QSD
    ///         is redeemed; the redeemer must have `amount` yQSD or
    ///         the call reverts. This enforces the 1:1 invariant
    ///         between yQSD and QSD outstanding.
    /// @dev    Restricted to the QSD contract.
    function redeemFrom(address from, uint256 amount) external onlyQsd {
        if (amount == 0) revert ZeroAmount();
        uint256 bal = balanceOf(from);
        if (bal < amount) revert InsufficientYieldClaim(from, amount, bal);
        _burn(from, amount);
        emit Redeemed(from, amount);
    }

    // ------------------------------------------------------------------
    // Claim
    // ------------------------------------------------------------------

    /// @notice Transfer all settled iQRL rewards to the caller.
    /// @return amount Amount of iQRL transferred.
    function claim() external nonReentrant returns (uint256 amount) {
        _settle(msg.sender);
        amount = pendingReward[msg.sender];
        if (amount == 0) return 0;
        pendingReward[msg.sender] = 0;
        iqrl.safeTransfer(msg.sender, amount);
        emit Claimed(msg.sender, amount);
    }

    /// @notice View: pending iQRL rewards for `user`, including any
    ///         unsettled accruals since their last balance change.
    function pending(address user) external view returns (uint256) {
        uint256 credited = balanceOf(user) * accRewardPerShare / SCALE;
        uint256 unsettled = credited >= rewardDebt[user]
            ? credited - rewardDebt[user]
            : 0;
        return pendingReward[user] + unsettled;
    }

    // ------------------------------------------------------------------
    // Distribute (called by QSD leverage facility, but permissionless)
    // ------------------------------------------------------------------

    /// @notice Pull `amount` iQRL from caller and distribute to current
    ///         yQSD holders pro-rata.
    /// @dev    Permissionless: anyone may donate iQRL. In practice the
    ///         QSD leverage facility is the sole caller. If totalSupply
    ///         is zero at distribution time, accRewardPerShare cannot
    ///         advance and the iQRL is held by the contract; in
    ///         practice the leverage facility cannot be used until QSD
    ///         is minted (which co-mints yQSD), so supply > 0 whenever
    ///         distribute() is called.
    function distribute(uint256 amount) external nonReentrant {
        if (amount == 0) revert ZeroAmount();
        iqrl.safeTransferFrom(msg.sender, address(this), amount);
        uint256 supply = totalSupply();
        if (supply > 0) {
            accRewardPerShare += (amount * SCALE) / supply;
        }
        totalDistributed += amount;
        emit Distributed(amount, accRewardPerShare);
    }

    // ------------------------------------------------------------------
    // Internal: settle on every balance change
    // ------------------------------------------------------------------

    /// @dev Move newly-accrued rewards from the unsettled bucket
    ///      (acc * balance - rewardDebt) into the settled bucket
    ///      (pendingReward), and update rewardDebt at the current
    ///      balance.
    function _settle(address user) internal {
        if (user == address(0)) return;
        uint256 credited = balanceOf(user) * accRewardPerShare / SCALE;
        uint256 debt = rewardDebt[user];
        if (credited > debt) {
            pendingReward[user] += credited - debt;
        }
        rewardDebt[user] = credited;
    }

    /// @dev OZ ERC20 v5 hook. Runs on every balance change including
    ///      mint (from = 0), burn (to = 0), and user transfers. Settle
    ///      both sides at the pre-change balance, then snapshot
    ///      rewardDebt at the post-change balance.
    function _update(address from, address to, uint256 value) internal override {
        _settle(from);
        _settle(to);

        super._update(from, to, value);

        if (from != address(0)) {
            rewardDebt[from] = balanceOf(from) * accRewardPerShare / SCALE;
        }
        if (to != address(0)) {
            rewardDebt[to] = balanceOf(to) * accRewardPerShare / SCALE;
        }
    }
}
