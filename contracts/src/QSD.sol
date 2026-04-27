// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";
import {Math} from "@openzeppelin/contracts/utils/math/Math.sol";

import {IPriceOracle} from "./interfaces/IPriceOracle.sol";

/// @title  Quantum Stable Dollar (QSD)
/// @notice USD-denominated stablecoin backed by a symmetric pool of
///         native QRL and its inverse (iQRL).
///
/// @dev    Design summary
///         --------------
///         Pool reserves: A units of QRL (native), B units of iQRL.
///         At oracle price p (USD per QRL):
///             1 QRL  = p     USD
///             1 iQRL = 1/p   USD       (canonical, by construction)
///             V      = A*p + B/p       (pool USD value)
///
///         Solvency invariant (Cauchy-Schwarz over per-deposit
///         contributions): V >= total QSD supply at all times.
///
///         Deposit
///             User brings (qrlAmount, iqrlAmount) — qrlAmount via
///             msg.value, iqrlAmount via ERC-20 transferFrom — such
///             that their USD values are equal at the current oracle
///             price p:
///                 qrlAmount * p == iqrlAmount / p
///             i.e. iqrlAmount == qrlAmount * p^2.
///             QSD minted = total deposit USD value = 2 * qrlAmount * p.
///
///         Redeem
///             Burn `qsdAmount` QSD, receive a 1/V slice of the pool:
///                 qrlOut  = poolQRL  * qsdAmount / V    (native send)
///                 iqrlOut = poolIQRL * qsdAmount / V    (ERC-20 send)
///             USD value returned = qsdAmount exactly.
///
///         Properties
///             - No governance token, no liquidations, no peg defense.
///             - No mint fee, no redemption fee, no stability fee, no
///               interest. The protocol extracts no value from QSD
///               holders.
///             - Each redemption preserves the pool's composition ratio
///               A:B exactly, so the pool's straddle shape survives any
///               sequence of redemptions.
///             - Solvency is a closed-form arithmetic property
///               (Cauchy-Schwarz), not an actively maintained
///               invariant.
contract QSD is ERC20, ReentrancyGuard {
    using SafeERC20 for IERC20;

    /// @notice Fixed-point scale used by all amounts and the oracle.
    uint256 internal constant SCALE = 1e18;

    /// @notice The inverse token (iQRL).
    IERC20 public immutable iqrl;

    /// @notice The QRL/USD price oracle.
    IPriceOracle public immutable oracle;

    /// @notice QRL held by the pool. Tracked explicitly rather than
    ///         read from address(this).balance, to immunize against
    ///         force-credited balance (e.g. SELFDESTRUCT victims).
    uint256 public poolQRL;

    /// @notice iQRL held by the pool. Tracked for the same reason as
    ///         poolQRL — protects against unsolicited token transfers.
    uint256 public poolIQRL;

    /// @notice Cumulative time-weighted spot price for TWAP queries.
    ///         Units: 1e18-scaled `sqrt(poolIQRL / poolQRL)` integrated
    ///         over elapsed seconds. Updated lazily on every state-
    ///         mutating operation that affects the pool ratio (i.e.
    ///         swaps; symmetric deposits/redeems leave the ratio
    ///         unchanged but still update the timestamp anchor).
    ///         Consumers (currently only InverseQRL.mint) maintain
    ///         their own snapshots to compute TWAP over a window.
    uint256 public priceCumulativeLast;

    /// @notice Last block.timestamp at which `priceCumulativeLast`
    ///         was rolled forward. Set on bootstrap deposit; updated
    ///         by `_updatePriceCumulative` on every subsequent
    ///         state-mutating operation.
    uint64 public lastPriceUpdate;

    error EmptyPool();
    error OracleUnhealthy();
    error ZeroAmount();
    error UnexpectedValue(uint256 expected, uint256 received);
    error SlippageExceeded(uint256 amountOut, uint256 minAmountOut);
    error NativeTransferFailed();

    event Deposited(
        address indexed user,
        uint256 qrlAmount,
        uint256 iqrlAmount,
        uint256 qsdMinted
    );

    event Redeemed(
        address indexed user,
        uint256 qsdBurned,
        uint256 qrlReturned,
        uint256 iqrlReturned
    );

    event Swapped(
        address indexed user,
        bool qrlIn,
        uint256 amountIn,
        uint256 amountOut
    );

    constructor(IERC20 _iqrl, IPriceOracle _oracle) ERC20("Quantum Stable Dollar", "QSD") {
        iqrl = _iqrl;
        oracle = _oracle;
    }

    // ------------------------------------------------------------------
    // Deposit / Redeem
    // ------------------------------------------------------------------

    /// @notice Deposit native QRL (via msg.value) and/or iQRL and
    ///         receive QSD equal to the increase in the pool's
    ///         AM-GM-derived USD floor:
    ///
    ///             qsdMinted = totalSupply * qrlIn / poolQRL
    ///
    ///         which makes both reserves and supply scale by the
    ///         same factor (1 + qrlIn/poolQRL). This exactly
    ///         preserves the invariant
    ///
    ///             2 * sqrt(k) == totalSupply()
    ///
    ///         after every deposit (modulo floor-rounding wei).
    ///
    ///         Deposits MUST be symmetric at the pool's current
    ///         marginal ratio. The caller sends QRL via msg.value;
    ///         the contract pulls the corresponding iQRL via
    ///         transferFrom. There is no path to deposit at an
    ///         asymmetric ratio: it would mint less QSD than the
    ///         depositor's contribution and silently donate the
    ///         shortfall to existing holders. Users with imbalanced
    ///         inventory (e.g. extra iQRL) must swap through the
    ///         pool to balance before depositing.
    /// @param  maxIqrlIn  Slippage cap on the iQRL leg. The contract
    ///                    pulls (msg.value * poolIQRL / poolQRL)
    ///                    iQRL; if that exceeds maxIqrlIn the tx
    ///                    reverts. Protects against pool-ratio
    ///                    shifts between submission and execution.
    /// @param  minQsdOut  Minimum QSD the caller is willing to
    ///                    accept; revert if computed amount is
    ///                    below this.
    /// @return qsdMinted  Amount of QSD minted to the caller.
    /// @return iqrlIn     Amount of iQRL pulled from the caller.
    function deposit(uint256 maxIqrlIn, uint256 minQsdOut)
        external
        payable
        nonReentrant
        returns (uint256 qsdMinted, uint256 iqrlIn)
    {
        uint256 qrlIn = msg.value;
        if (qrlIn == 0) revert ZeroAmount();

        // Roll the price accumulator forward at the OLD spot price
        // before reserves change. Symmetric deposits don't move the
        // ratio, but they do anchor the timestamp.
        _updatePriceCumulative();

        uint256 supply = totalSupply();
        if (supply == 0) {
            // Bootstrap: first depositor sets the initial pool
            // ratio. Use both legs at face value, mint QSD via the
            // CPMM-LP formula so 2*sqrt(k) == totalSupply at the
            // end. maxIqrlIn doubles as the iQRL contribution
            // amount in this branch.
            iqrlIn = maxIqrlIn;
            if (iqrlIn == 0) revert ZeroAmount();
            qsdMinted = 2 * Math.sqrt(qrlIn * iqrlIn);
            // Initialize the TWAP timestamp anchor; the spot price
            // implied by these reserves applies from now forward.
            lastPriceUpdate = uint64(block.timestamp);
        } else {
            // Symmetric deposit at pool's marginal ratio.
            iqrlIn = (qrlIn * poolIQRL) / poolQRL;
            if (iqrlIn > maxIqrlIn) revert SlippageExceeded(iqrlIn, maxIqrlIn);
            // Pool grows by factor (qrlIn / poolQRL); supply grows
            // by the same factor.
            qsdMinted = (supply * qrlIn) / poolQRL;
        }

        if (qsdMinted == 0) revert ZeroAmount();
        if (qsdMinted < minQsdOut) revert SlippageExceeded(qsdMinted, minQsdOut);

        iqrl.safeTransferFrom(msg.sender, address(this), iqrlIn);

        poolQRL += qrlIn;
        poolIQRL += iqrlIn;

        _mint(msg.sender, qsdMinted);
        _assertSolvent();

        emit Deposited(msg.sender, qrlIn, iqrlIn, qsdMinted);
    }

    /// @notice Burn `qsdAmount` QSD and receive a pro-rata slice of
    ///         the pool reserves, sized strictly by `qsdAmount /
    ///         totalSupply`. No oracle dependency: the pool itself
    ///         is the source of truth, and the slice is a verifiable
    ///         on-chain claim that no external feed can corrupt.
    /// @param  qsdAmount   Amount of QSD to burn.
    /// @return qrlReturned QRL returned to the caller (native send).
    /// @return iqrlReturned iQRL returned to the caller.
    /// @dev    Pays >= $1 of USD value per QSD burned, with equality
    ///         only when the pool sits at its symmetric-balanced
    ///         marginal-price state. Any swap or asymmetric deposit
    ///         pushes V above totalSupply (call this "slack") and
    ///         redemptions distribute that slack pro-rata to
    ///         redeemers, rather than letting it accumulate
    ///         indefinitely in the pool. QSD is therefore a yield-
    ///         bearing share of the pool with a strict $1 floor, not
    ///         a strict $1-pegged stablecoin.
    ///
    ///         Redemption has no liveness dependency on the oracle:
    ///         even if every validator goes silent, holders can
    ///         exit the system at any time.
    function redeem(uint256 qsdAmount)
        external
        nonReentrant
        returns (uint256 qrlReturned, uint256 iqrlReturned)
    {
        if (qsdAmount == 0) revert ZeroAmount();

        uint256 supply = totalSupply();
        if (supply == 0) revert EmptyPool();

        // Roll the price accumulator forward at the OLD spot price
        // before reserves change. Pro-rata redemption doesn't move
        // the ratio, but anchors the timestamp.
        _updatePriceCumulative();

        // Pro-rata-by-supply slice. Integer division rounds payouts
        // DOWN, which favors the pool and tightens solvency.
        qrlReturned = (poolQRL * qsdAmount) / supply;
        iqrlReturned = (poolIQRL * qsdAmount) / supply;

        // Sanity: slice can never exceed reserves. By construction
        // qsdAmount <= supply, so qrlReturned <= poolQRL and
        // iqrlReturned <= poolIQRL.
        assert(qrlReturned <= poolQRL);
        assert(iqrlReturned <= poolIQRL);

        _burn(msg.sender, qsdAmount);

        poolQRL -= qrlReturned;
        poolIQRL -= iqrlReturned;

        // Pay iQRL first (deterministic ERC-20 transfer), then native
        // last (CEI: all state mutations done). _payNative will revert
        // if the recipient rejects the transfer.
        if (iqrlReturned > 0) iqrl.safeTransfer(msg.sender, iqrlReturned);
        if (qrlReturned > 0) _payNative(msg.sender, qrlReturned);

        // Pro-rata redemption preserves the solvency invariant
        // 2*sqrt(k) >= totalSupply: both reserves and supply scale
        // by the same factor (supply - q) / supply, so the
        // inequality is invariant under redemption modulo floor-
        // rounding (which only adds slack, never subtracts).
        _assertSolvent();

        emit Redeemed(msg.sender, qsdAmount, qrlReturned, iqrlReturned);
    }

    // ------------------------------------------------------------------
    // Swap (constant-product AMM over the same pool)
    // ------------------------------------------------------------------
    //
    // The pool's reserves (poolQRL, poolIQRL) double as a CPMM with
    // invariant x * y = k. Swaps maintain k by construction; only
    // deposits and redemptions change it.
    //
    // Inverse-priced asset pairs have ZERO impermanent loss in a CPMM:
    // for any p, the LP value at fair pricing is 2 * sqrt(k), which is
    // independent of p (since the price product p_QRL * p_iQRL = 1).
    // Because there is no LP to compensate for IL, no swap fee is
    // charged. Arbitrageurs extract value only from misalignment
    // between the pool's marginal price and the external market;
    // realigned pools sit at the AM-GM minimum 2 * sqrt(k), which is
    // the same floor that backs QSD solvency.
    //
    // Solvency invariant under swaps: k is unchanged, so the AM-GM
    // bound 2 * sqrt(k) >= total QSD supply continues to hold.
    //
    // The two-direction split (swapQrlForIqrl payable + swapIqrlForQrl
    // ERC-20-pull) is forced by native-QRL semantics — the input asset
    // determines whether msg.value or transferFrom is the right pull
    // mechanism.

    /// @notice Swap an exact amount of native QRL (msg.value) for iQRL
    ///         using the constant-product invariant x * y = k.
    /// @param  minAmountOut  Slippage protection: revert if output < this.
    /// @return amountOut     iQRL delivered to the caller.
    /// @dev    No swap fee. No oracle dependency.
    function swapQrlForIqrl(uint256 minAmountOut)
        external
        payable
        nonReentrant
        returns (uint256 amountOut)
    {
        uint256 amountIn = msg.value;
        if (amountIn == 0) revert ZeroAmount();
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();

        // Roll the price accumulator forward at the PRE-swap spot
        // before mutating reserves. This is the only path that
        // changes the pool ratio, so this is where the TWAP signal
        // accumulates against time.
        _updatePriceCumulative();

        uint256 kBefore = poolQRL * poolIQRL;

        // x * y = k:
        //   poolQRL * poolIQRL = k
        //   (poolQRL + amountIn) * (poolIQRL - amountOut) = k
        // => amountOut = poolIQRL * amountIn / (poolQRL + amountIn)
        amountOut = (poolIQRL * amountIn) / (poolQRL + amountIn);
        assert(amountOut < poolIQRL);
        if (amountOut < minAmountOut) revert SlippageExceeded(amountOut, minAmountOut);

        poolQRL += amountIn;
        poolIQRL -= amountOut;

        iqrl.safeTransfer(msg.sender, amountOut);

        assert(poolQRL * poolIQRL >= kBefore);
        _assertSolvent();

        emit Swapped(msg.sender, true, amountIn, amountOut);
    }

    /// @notice Swap an exact amount of iQRL for native QRL using the
    ///         constant-product invariant x * y = k.
    /// @param  amountIn      Exact iQRL input. Caller must have first
    ///                       approved this contract for amountIn iQRL.
    /// @param  minAmountOut  Slippage protection: revert if output < this.
    /// @return amountOut     Native QRL delivered to the caller.
    /// @dev    No swap fee. No oracle dependency.
    function swapIqrlForQrl(uint256 amountIn, uint256 minAmountOut)
        external
        nonReentrant
        returns (uint256 amountOut)
    {
        if (amountIn == 0) revert ZeroAmount();
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();

        // Roll the price accumulator forward at the PRE-swap spot.
        _updatePriceCumulative();

        uint256 kBefore = poolQRL * poolIQRL;

        amountOut = (poolQRL * amountIn) / (poolIQRL + amountIn);
        assert(amountOut < poolQRL);
        if (amountOut < minAmountOut) revert SlippageExceeded(amountOut, minAmountOut);

        // Pull input first; update reserves; then pay out native last.
        iqrl.safeTransferFrom(msg.sender, address(this), amountIn);

        poolIQRL += amountIn;
        poolQRL -= amountOut;

        _payNative(msg.sender, amountOut);

        assert(poolQRL * poolIQRL >= kBefore);
        _assertSolvent();

        emit Swapped(msg.sender, false, amountIn, amountOut);
    }

    /// @notice Quote a swap in either direction without executing it.
    /// @param  qrlIn      True if quoting QRL -> iQRL; false if iQRL -> QRL.
    /// @param  amountIn   Proposed input amount.
    /// @return amountOut  Output amount that the caller would receive.
    function quoteSwap(bool qrlIn, uint256 amountIn)
        external
        view
        returns (uint256 amountOut)
    {
        if (qrlIn) {
            amountOut = (poolIQRL * amountIn) / (poolQRL + amountIn);
        } else {
            amountOut = (poolQRL * amountIn) / (poolIQRL + amountIn);
        }
    }

    // ------------------------------------------------------------------
    // View helpers
    // ------------------------------------------------------------------

    /// @notice USD value of the pool at the current oracle price,
    ///         scaled by 1e18.
    function poolValueUsd() external view returns (uint256) {
        uint256 p = oracle.price();
        return (poolQRL * p) / SCALE + (poolIQRL * SCALE) / p;
    }

    /// @notice Current collateralization ratio, scaled by 1e18 (1e18
    ///         == 100%). Returns max(uint256) when there is no QSD
    ///         outstanding. By Cauchy-Schwarz the value is always
    ///         >= 1e18 in steady state.
    function collateralRatio() external view returns (uint256) {
        uint256 supply = totalSupply();
        if (supply == 0) return type(uint256).max;
        uint256 p = oracle.price();
        uint256 V = (poolQRL * p) / SCALE + (poolIQRL * SCALE) / p;
        return (V * SCALE) / supply;
    }

    /// @notice Quote the symmetric deposit for a given QRL
    ///         contribution: returns the iQRL the contract would
    ///         pull and the QSD that would be minted at the current
    ///         pool state. Mirrors `deposit`'s post-bootstrap path
    ///         exactly, so callers can size `maxIqrlIn` against the
    ///         returned `iqrlIn` plus their tolerance for ratio
    ///         drift.
    /// @dev    Reverts when the pool is empty (totalSupply == 0):
    ///         the marginal ratio is undefined during bootstrap, so
    ///         no quote is meaningful.
    function quoteDeposit(uint256 qrlAmount)
        external
        view
        returns (uint256 iqrlIn, uint256 qsdMinted)
    {
        uint256 supply = totalSupply();
        if (supply == 0) revert EmptyPool();
        iqrlIn = (qrlAmount * poolIQRL) / poolQRL;
        qsdMinted = (supply * qrlAmount) / poolQRL;
    }

    // ------------------------------------------------------------------
    // Invariants
    // ------------------------------------------------------------------
    //
    // Core invariant (always holds):
    //
    //     2 * sqrt(poolQRL * poolIQRL) >= totalSupply()
    //
    // After deposits this is exactly equal (modulo floor-sqrt rounding,
    // which can leave totalSupply slightly below 2*floor(sqrt(k))). Swaps
    // preserve k exactly (or grow it via integer rounding favoring the
    // pool). Redemptions shrink k proportionally to qsdAmount/V <= 1,
    // which is at most as fast as supply shrinks, so slack only grows.
    //
    // The invariant is asserted after every state-changing entrypoint
    // and exposed via checkInvariants() for tests and external monitors.

    /// @notice External view: returns true iff the core solvency
    ///         invariant holds at the current state.
    function checkInvariants() external view returns (bool) {
        return 2 * Math.sqrt(poolQRL * poolIQRL) >= totalSupply();
    }

    /// @dev    Internal solvency check; uses Solidity's `assert` so a
    ///         failure produces a Panic and signals a critical bug
    ///         rather than a recoverable user error.
    function _assertSolvent() internal view {
        assert(2 * Math.sqrt(poolQRL * poolIQRL) >= totalSupply());
    }

    /// @dev    Send `amount` native QRL to `to`. Reverts on failure;
    ///         must be the last side-effect in the caller (CEI).
    function _payNative(address to, uint256 amount) internal {
        (bool ok, ) = to.call{value: amount}("");
        if (!ok) revert NativeTransferFailed();
    }

    // ------------------------------------------------------------------
    // Price oracle (TWAP)
    // ------------------------------------------------------------------

    /// @dev Roll `priceCumulativeLast` forward to `block.timestamp`
    ///      using the spot price implied by the current reserves
    ///      (BEFORE any state mutation in the calling function).
    ///      Spot is `sqrt(poolIQRL / poolQRL)` in 1e18 fixed point,
    ///      derived from the inverse-priced-pair USD equivalence
    ///      `B / A = p^2`.
    ///
    ///      Idempotent within a block: if `block.timestamp ==
    ///      lastPriceUpdate` no contribution is added, which makes
    ///      the cumulative flash-loan resistant (intra-block
    ///      manipulation cannot move the integrated value).
    function _updatePriceCumulative() internal {
        uint64 nowTs = uint64(block.timestamp);
        if (lastPriceUpdate == 0) {
            // Pool not yet bootstrapped.
            return;
        }
        if (nowTs > lastPriceUpdate && poolQRL > 0 && poolIQRL > 0) {
            uint256 elapsed = uint256(nowTs - lastPriceUpdate);
            uint256 spot = Math.sqrt((poolIQRL * SCALE * SCALE) / poolQRL);
            priceCumulativeLast += spot * elapsed;
        }
        lastPriceUpdate = nowTs;
    }

    /// @notice Read the cumulative price extrapolated to the current
    ///         block. External readers store the returned tuple as a
    ///         snapshot and compute TWAP across two snapshots as
    ///         `(c2 - c1) / (t2 - t1)`. Returns zeros when the pool
    ///         has never been bootstrapped.
    /// @return cumulative Cumulative spot price scaled by 1e18,
    ///                    integrated over seconds.
    /// @return timestamp  block.timestamp at the read point.
    function priceCumulativeNow() external view returns (uint256 cumulative, uint64 timestamp) {
        timestamp = uint64(block.timestamp);
        cumulative = priceCumulativeLast;
        if (lastPriceUpdate != 0 && lastPriceUpdate < timestamp && poolQRL > 0 && poolIQRL > 0) {
            uint256 elapsed = uint256(timestamp - lastPriceUpdate);
            uint256 spot = Math.sqrt((poolIQRL * SCALE * SCALE) / poolQRL);
            cumulative += spot * elapsed;
        }
    }
}
