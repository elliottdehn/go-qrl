// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @title  Yielding Quantum Stable Dollar (yQSD)
/// @notice ERC-20 wrapper around QSD that distributes incoming iQRL
///         rewards (paid by the QSD pool's leverage facility) to
///         stakers proportionally. Standard MasterChef-style accumulator.
///
/// @dev    Properties
///         ----------
///         - 1:1 wrap of QSD. deposit(amount) → mint amount yQSD;
///           withdraw(amount) → burn amount yQSD, return amount QSD.
///         - Rewards in raw iQRL. Stakers can hold (long-vol exposure)
///           or burn for fees themselves.
///         - Instant unstake. No cooldown, no slashing.
///         - distribute(amount) is permissionless; in practice the
///           QSD leverage facility is the sole caller. Anyone may
///           voluntarily route iQRL into the pool.
///         - accRewardPerShare math, settled on every transfer
///           (mints / burns / user-to-user transfers all flow through
///           the standard ERC20 _update hook).
contract YieldQSD is ERC20, ReentrancyGuard {
    using SafeERC20 for IERC20;

    /// @notice 1e18 fixed-point scale.
    uint256 internal constant SCALE = 1e18;

    /// @notice The underlying QSD token (the asset wrapped 1:1).
    IERC20 public immutable qsd;

    /// @notice The reward token (iQRL).
    IERC20 public immutable iqrl;

    /// @notice Cumulative iQRL rewards per yQSD share, 1e18-scaled.
    ///         Monotone non-decreasing.
    uint256 public accRewardPerShare;

    /// @notice Per-user snapshot of accRewardPerShare * balance / SCALE
    ///         at the user's last balance change. The standard
    ///         MasterChef "rewardDebt" — represents iQRL already
    ///         credited to the user.
    mapping(address => uint256) public rewardDebt;

    /// @notice Settled-but-not-yet-claimed iQRL rewards per user.
    ///         Accumulated on every balance change; drained by claim().
    mapping(address => uint256) public pendingReward;

    /// @notice Cumulative iQRL distributed (audit / view convenience).
    uint256 public totalDistributed;

    error ZeroAmount();

    event Deposited(address indexed user, uint256 amount);
    event Withdrawn(address indexed user, uint256 amount);
    event Claimed(address indexed user, uint256 amount);
    event Distributed(uint256 amount, uint256 newAccRewardPerShare);

    constructor(IERC20 _qsd, IERC20 _iqrl) ERC20("Yielding Quantum Stable Dollar", "yQSD") {
        qsd = _qsd;
        iqrl = _iqrl;
    }

    // ------------------------------------------------------------------
    // Deposit / Withdraw (1:1 wrap of QSD)
    // ------------------------------------------------------------------

    /// @notice Stake QSD; mint yQSD 1:1. Caller must have approved
    ///         this contract to spend `amount` QSD.
    function deposit(uint256 amount) external nonReentrant {
        if (amount == 0) revert ZeroAmount();
        qsd.safeTransferFrom(msg.sender, address(this), amount);
        _mint(msg.sender, amount);
        emit Deposited(msg.sender, amount);
    }

    /// @notice Burn yQSD; return QSD 1:1. Pending iQRL rewards remain
    ///         claimable separately via claim() — they are not
    ///         forfeited on withdrawal.
    function withdraw(uint256 amount) external nonReentrant {
        if (amount == 0) revert ZeroAmount();
        _burn(msg.sender, amount);
        qsd.safeTransfer(msg.sender, amount);
        emit Withdrawn(msg.sender, amount);
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
    ///         accruals since their last settled balance change.
    function pending(address user) external view returns (uint256) {
        uint256 credited = balanceOf(user) * accRewardPerShare / SCALE;
        uint256 unsettled = credited >= rewardDebt[user]
            ? credited - rewardDebt[user]
            : 0;
        return pendingReward[user] + unsettled;
    }

    // ------------------------------------------------------------------
    // Distribute (called by QSD leverage facility)
    // ------------------------------------------------------------------

    /// @notice Pull `amount` iQRL from caller and distribute to current
    ///         stakers pro-rata. Permissionless: anyone may donate.
    /// @dev    If totalSupply is zero at the time of distribution, the
    ///         iQRL is held by the contract but never distributed
    ///         (accRewardPerShare can't be advanced against zero
    ///         supply). In practice the leverage facility cannot
    ///         operate before QSD is minted (which requires a pool
    ///         deposit), and anyone who deposits QSD into yQSD before
    ///         a single distribution is a bootstrapper accepting that
    ///         risk. The orphan funds, if any, sit forever as a
    ///         protocol donation.
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
    ///      (pendingReward), and update rewardDebt to reflect the
    ///      now-credited amount at the *current* balance.
    ///      Caller responsibility: invoke before any balance change so
    ///      the pre-change balance is used; _update then refreshes
    ///      rewardDebt against the new balance.
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
    ///      mint (from = 0), burn (to = 0), and user transfers. Settles
    ///      both sides at the pre-change balance, applies the transfer,
    ///      then snapshots rewardDebt at the post-change balance.
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
