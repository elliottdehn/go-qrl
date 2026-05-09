// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {ERC20Burnable} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Burnable.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";

/// @title  PayWithIQRL
/// @notice Paymaster that lets a transaction declare iQRL as the
///         fee asset. Consensus calls this contract before and after
///         executing a paymaster-flagged tx; the contract handles
///         the iQRL escrow + settlement.
///
/// @dev    Lifecycle of one iQRL-paid tx:
///
///           1. Consensus checks that:
///                - sender has iqrl.balanceOf(sender) >= maxFee, and
///                - sender has approved this contract for >= maxFee.
///           2. Consensus issues a system call `escrow(sender,
///              maxFee)`. The contract pulls maxFee iQRL from sender
///              into its own balance.
///           3. The tx executes normally. Any internal calls work
///              against the sender's *remaining* iQRL balance.
///           4. Consensus issues a system call `settle(sender,
///              maxFee, burnAmount, tipAmount)`. The contract burns
///              burnAmount iQRL (the EIP-1559-equivalent base fee
///              quoted in iQRL terms), forwards tipAmount to
///              `block.coinbase`, and refunds the unused
///              `maxFee - burnAmount - tipAmount` to the sender.
///
///         Burning the base-fee portion mirrors the QRL-side burn on
///         native EIP-1559 txs, so iQRL supply responds to chain
///         activity the same way QRL does. Without this split,
///         paymaster txs would inflate iQRL relative to the native
///         path.
///
///         Both entrypoints check that `msg.sender == SYSTEM_CALLER`.
///         Without that guard, anyone could call `settle` with bogus
///         arguments and drain the contract's transient balance.
///         Conversely, sender's iQRL is only ever moved when sender
///         has explicitly approved this contract — standard ERC-20
///         allowance semantics protect against unauthorized debits.
contract PayWithIQRL {
    using SafeERC20 for IERC20;

    /// @notice Sentinel sender address used by the consensus engine
    ///         when invoking system calls. Hard-coded; mirrors the
    ///         pattern used by other on-chain consensus hooks (e.g.
    ///         the EIP-4788 beacon-root contract).
    address public constant SYSTEM_CALLER = 0xffffFFFfFFffffffffffffffFfFFFfffFFFfFFfE;

    /// @notice The iQRL token used to settle fees.
    IERC20 public immutable iqrl;

    error NotSystemCaller();
    error InsufficientEscrow(uint256 actualFee, uint256 escrowed);

    event FeeEscrowed(address indexed payer, uint256 maxFee);
    event FeeSettled(
        address indexed payer,
        uint256 burnAmount,
        uint256 tipAmount,
        uint256 refund,
        address coinbase
    );

    constructor(IERC20 _iqrl) {
        iqrl = _iqrl;
    }

    /// @notice Pull `maxFee` iQRL from `payer` and hold it in this
    ///         contract until `settle` is called. Restricted to the
    ///         consensus engine.
    function escrow(address payer, uint256 maxFee) external {
        if (msg.sender != SYSTEM_CALLER) revert NotSystemCaller();
        iqrl.safeTransferFrom(payer, address(this), maxFee);
        emit FeeEscrowed(payer, maxFee);
    }

    /// @notice Settle a previously-escrowed fee. `burnAmount` iQRL
    ///         is destroyed via iqrl.burn (mirroring the EIP-1559
    ///         base-fee burn on native txs); `tipAmount` is forwarded
    ///         to `block.coinbase`; the leftover
    ///         `maxFee - burnAmount - tipAmount` is refunded to
    ///         `payer`. Restricted to the consensus engine.
    /// @dev    The (burn, tip) split is computed by consensus from
    ///         the chain's QRL base fee converted to iQRL terms via
    ///         the oracle, plus the tx's signed gasFeeCap/gasTipCap.
    ///         The contract just trusts the values (it has to —
    ///         only the system caller can invoke this).
    function settle(
        address payer,
        uint256 maxFee,
        uint256 burnAmount,
        uint256 tipAmount
    ) external {
        if (msg.sender != SYSTEM_CALLER) revert NotSystemCaller();
        uint256 actualFee = burnAmount + tipAmount;
        if (actualFee > maxFee) revert InsufficientEscrow(actualFee, maxFee);

        if (burnAmount > 0) {
            // Burn from this contract's own iQRL balance. Reduces
            // iqrl.totalSupply by exactly burnAmount.
            ERC20Burnable(address(iqrl)).burn(burnAmount);
        }

        address coinbase = block.coinbase;
        if (tipAmount > 0) iqrl.safeTransfer(coinbase, tipAmount);
        uint256 refund = maxFee - actualFee;
        if (refund > 0) iqrl.safeTransfer(payer, refund);

        emit FeeSettled(payer, burnAmount, tipAmount, refund, coinbase);
    }
}
