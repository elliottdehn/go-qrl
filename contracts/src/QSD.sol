// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";
import {Math} from "@openzeppelin/contracts/utils/math/Math.sol";

import {IPriceOracle} from "./interfaces/IPriceOracle.sol";

/// @dev Subset of YieldQSD that this contract calls. Defined inline
///      to avoid a deploy-time circular dependency: YieldQSD takes
///      QSD's address in its constructor, so QSD cannot directly
///      import the contract type.
interface IYieldQSD {
    function distribute(uint256 amount) external;
}

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

    /// @notice The yQSD staking contract that receives leverage-facility
    ///         interest as iQRL. Stakers of QSD into yQSD claim this
    ///         flow pro-rata. Replacing the previous burn-on-receipt
    ///         design: leverage interest is no longer destroyed but
    ///         routed to a yield primitive that bootstraps QSD-pool
    ///         liquidity by giving holders a real yield.
    IYieldQSD public immutable yieldQsd;

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

    // ------------------------------------------------------------------
    // Leverage facility
    // ------------------------------------------------------------------

    /// @notice Maximum loan duration. Loans must be settled (voluntarily
    ///         by the owner or via forceClose by anyone after expiry)
    ///         within this window.
    uint256 public constant LEVERAGE_MAX_DURATION = 1 days;

    /// @notice Interest rate floor in basis points / year (12% APR).
    ///         Charged at zero pool utilization.
    uint256 public constant LEVERAGE_MIN_RATE_BPS = 1200;

    /// @notice Interest rate ceiling in basis points / year (24% APR).
    ///         Charged when 50% of the pool (the cap) is loaned out.
    uint256 public constant LEVERAGE_MAX_RATE_BPS = 2400;

    /// @notice Maximum fraction of the pool's total token holdings that
    ///         may be loaned out at any time, expressed in basis points.
    ///         5000 bps = 50%. New loans that would exceed this cap revert.
    uint256 public constant LEVERAGE_POOL_CAP_BPS = 5000;

    uint256 internal constant LEVERAGE_BPS = 10_000;
    uint256 internal constant LEVERAGE_SECONDS_PER_YEAR = 365 days;

    /// @notice State of an open leveraged long-vol position.
    /// @dev    Sandbox tokens are held by the QSD contract on the
    ///         borrower's behalf. They never leave the contract until
    ///         settlement and may only be moved by sandbox swaps that
    ///         monotonically converge toward the pool's current ratio.
    struct Position {
        address owner;          // borrower
        uint128 qrlOwed;        // initial QRL borrowed (== sandbox at open)
        uint128 iqrlOwed;       // initial iQRL borrowed
        uint128 qrlBalance;     // sandbox QRL (mutates via sandbox swap)
        uint128 iqrlBalance;    // sandbox iQRL
        uint64  deadline;       // open + LEVERAGE_MAX_DURATION
    }

    /// @notice positionId => Position. Closed positions are deleted.
    mapping(uint256 => Position) public positions;

    /// @notice Next position id. Monotone increasing; never zero.
    uint256 public nextPositionId = 1;

    /// @notice Sum of qrlBalance across all open positions. Used for
    ///         (a) the 50% pool cap and (b) the aggregate-solvency
    ///         invariant: 2*sqrt((poolQRL + lentQRL)*(poolIQRL +
    ///         lentIQRL)) >= totalSupply().
    uint256 public lentQRL;

    /// @notice Sum of iqrlBalance across all open positions.
    uint256 public lentIQRL;

    error EmptyPool();
    error OracleUnhealthy();
    error ZeroAmount();
    error UnexpectedValue(uint256 expected, uint256 received);
    error SlippageExceeded(uint256 amountOut, uint256 minAmountOut);
    error NativeTransferFailed();
    error PositionNotOwned();
    error PositionNotFound();
    error PositionExpired();
    error PositionStillActive();
    error LeverageCapExceeded(uint256 requested, uint256 available);
    error DurationOutOfRange();
    error InsufficientFee(uint256 required, uint256 supplied);
    error ConvergenceViolation();
    error InsufficientSandboxBalance();
    error SolvencyWouldBreak();
    error PoolNotAtParity();
    error PoolNotInDiscount();
    error PoolNotInPremium();

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

    event PositionOpened(
        uint256 indexed positionId,
        address indexed owner,
        uint256 qrlOwed,
        uint256 iqrlOwed,
        uint256 feeIqrlPaid,
        uint256 rateBps,
        uint64 deadline
    );

    event SandboxSwapped(
        uint256 indexed positionId,
        bool qrlIn,
        uint256 amountIn,
        uint256 amountOut
    );

    event PositionClosed(
        uint256 indexed positionId,
        address indexed owner,
        uint256 qrlReturnedToPool,
        uint256 iqrlReturnedToPool,
        uint256 qrlPaidToOwner,
        uint256 iqrlPaidToOwner,
        bool forced
    );

    constructor(IERC20 _iqrl, IPriceOracle _oracle, IYieldQSD _yieldQsd)
        ERC20("Quantum Stable Dollar", "QSD")
    {
        iqrl = _iqrl;
        oracle = _oracle;
        yieldQsd = _yieldQsd;
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

    /// @notice External view: returns true iff the core aggregate
    ///         solvency invariant holds at the current state. Aggregate
    ///         covers pool reserves + tokens currently lent to leverage
    ///         positions, since all sandbox swaps preserve aggregate k
    ///         exactly. The pool-only k may be lower than supply during
    ///         active loans, which is intentional.
    function checkInvariants() external view returns (bool) {
        return 2 * Math.sqrt((poolQRL + lentQRL) * (poolIQRL + lentIQRL)) >= totalSupply();
    }

    /// @dev    Internal solvency check on contract aggregate holdings
    ///         (pool + lent). Uses `assert` so a failure produces a
    ///         Panic and signals a critical bug. Sandbox swaps and
    ///         loan opens preserve aggregate exactly; only deposits,
    ///         redemptions, external swaps, and position closes can
    ///         change the aggregate, and all of those check this.
    function _assertSolvent() internal view {
        assert(2 * Math.sqrt((poolQRL + lentQRL) * (poolIQRL + lentIQRL)) >= totalSupply());
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

    // ------------------------------------------------------------------
    // Leverage facility
    // ------------------------------------------------------------------
    //
    // No-collateral leveraged long-vol positions backed by the pool's
    // own depth. A borrower:
    //
    //   1. Pays an iQRL fee upfront (linear utilization curve from 12%
    //      to 24% APR, scaled to chosen duration). The fee is routed
    //      to yQSD stakers as iQRL rewards (see YieldQSD.distribute)
    //      and is non-refundable on early settlement.
    //   2. Receives a sandbox account holding (qrlOwed, iqrlOwed) at
    //      the pool's current marginal ratio. Pool reserves drop by the
    //      same amounts; aggregate (pool + lent) is preserved.
    //   3. May swap their sandbox through the pool, but only in the
    //      direction that monotonically reduces the gap between
    //      sandbox.R and pool.R. Each rebalance captures realized
    //      variance (gamma scalping); k_sandbox grows weakly.
    //   4. Settles voluntarily anytime before deadline, or anyone may
    //      force-close after the deadline. Pool reclaims tokens with
    //      product k_loan at sandbox's current ratio; borrower keeps
    //      excess as gamma profit, modulo aggregate solvency.
    //
    // Aggregate solvency floor (poolQRL + lentQRL)*(poolIQRL +
    // lentIQRL) >= (totalSupply()/2)^2 is preserved by opens and
    // sandbox swaps exactly, and asserted post-close. External pool
    // activity (swaps, deposits) accumulates slack via slippage that
    // bounded gamma extraction at close.

    /// @notice Current interest rate in basis points / year at the
    ///         present utilization. Public callers see the rate that
    ///         would apply to a zero-size additional borrow. New
    ///         loans actually pay the rate at *post-borrow*
    ///         utilization (see openPosition), so a borrower who
    ///         takes utilization from 0% to 50% pays the 24% rate,
    ///         not 12%.
    function leverageRateBps() public view returns (uint256) {
        return _rateAt(lentQRL);
    }

    /// @dev Pool-must-be-at-parity guard for openPosition.
    ///      Allows the smallest tolerance compatible with integer
    ///      rounding on a parity-restoring swap (1 part per 1e9
    ///      relative deviation in the cross-product check).
    function _requireParity() internal view {
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();
        uint256 p = oracle.price();
        if (p == 0 || !oracle.healthy()) revert OracleUnhealthy();
        // poolIQRL/poolQRL == p^2/SCALE^2  ⟺  poolIQRL * SCALE^2 == poolQRL * p^2
        uint256 lhs = poolIQRL * SCALE * SCALE;
        uint256 rhs = poolQRL * p * p;
        uint256 diff = lhs > rhs ? lhs - rhs : rhs - lhs;
        // Tolerance: diff must be < max(lhs, rhs) / 1e9.
        uint256 ref = lhs > rhs ? lhs : rhs;
        if (diff * 1_000_000_000 > ref) revert PoolNotAtParity();
    }

    /// @dev Compute the rate at a specific lent-amount, using the
    ///      contract's current total token holdings as the
    ///      denominator. Linear interpolation between
    ///      LEVERAGE_MIN_RATE_BPS at u=0 and LEVERAGE_MAX_RATE_BPS at
    ///      u=0.5 (the cap).
    function _rateAt(uint256 lentTotal) internal view returns (uint256) {
        uint256 totalQ = poolQRL + lentQRL;
        if (totalQ == 0) return LEVERAGE_MIN_RATE_BPS;
        uint256 utilScaled = (lentTotal * SCALE) / totalQ;
        uint256 spread = LEVERAGE_MAX_RATE_BPS - LEVERAGE_MIN_RATE_BPS;
        uint256 rate = LEVERAGE_MIN_RATE_BPS + (spread * 2 * utilScaled) / SCALE;
        if (rate > LEVERAGE_MAX_RATE_BPS) rate = LEVERAGE_MAX_RATE_BPS;
        return rate;
    }

    /// @notice Quote the iQRL fee for a hypothetical loan, using the
    ///         post-borrow rate (the rate the borrower would pay).
    function quoteLeverageFee(uint128 qrlAmount, uint64 durationSeconds)
        external
        view
        returns (uint256 feeIqrl, uint256 iqrlAmount, uint256 rateBps)
    {
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();
        iqrlAmount = (uint256(qrlAmount) * poolIQRL) / poolQRL;
        rateBps = _rateAt(lentQRL + uint256(qrlAmount));
        uint256 p = oracle.price();
        if (p == 0) revert OracleUnhealthy();
        feeIqrl = _computeFeeIqrl(uint256(qrlAmount), iqrlAmount, p, rateBps, durationSeconds);
    }

    /// @notice Open a leveraged long-vol position. Pool must be at
    ///         canonical parity for the current oracle price; otherwise
    ///         reverts with PoolNotAtParity. Use the bundled
    ///         `openPositionAtParityFromDiscount` /
    ///         `openPositionAtParityFromPremium` helpers to atomically
    ///         restore parity and open in one call.
    /// @param  qrlAmount        QRL leg of the symmetric loan.
    /// @param  durationSeconds  Position lifetime; (0, LEVERAGE_MAX_DURATION].
    /// @param  maxFeeIqrl       Slippage cap on the iQRL fee. Caller
    ///                          must have approved this contract for
    ///                          at least the actual fee amount.
    /// @return positionId       Identifier for the new position.
    /// @return iqrlAmount       Amount of iQRL pulled from pool into
    ///                          the borrower's sandbox.
    /// @return feeIqrl          Actual iQRL fee paid to yQSD (≤ maxFeeIqrl).
    function openPosition(uint128 qrlAmount, uint64 durationSeconds, uint256 maxFeeIqrl)
        external
        nonReentrant
        returns (uint256 positionId, uint256 iqrlAmount, uint256 feeIqrl)
    {
        _requireParity();
        return _openPositionUnchecked(qrlAmount, durationSeconds, maxFeeIqrl);
    }

    /// @notice Atomically restore pool parity (in either direction)
    ///         and open a leveraged position. Pool state at landing
    ///         time may be discount, premium, or parity; the function
    ///         handles all three. Caller pre-funds both directions:
    ///         msg.value supplies QRL for closing a discount; the
    ///         contract pulls up to maxIqrlInForParity iQRL via
    ///         allowance for closing a premium. The unused side is
    ///         refunded at the end. The captured parity-swap output
    ///         (iQRL or QRL, depending on direction) goes to the
    ///         caller as their peg-restoration profit.
    function openPositionAtParity(
        uint128 qrlAmount,
        uint64 durationSeconds,
        uint256 maxFeeIqrl,
        uint256 maxIqrlInForParity,
        uint256 minOutForParity
    )
        external
        payable
        nonReentrant
        returns (
            uint256 positionId,
            uint256 iqrlAmount,
            uint256 feeIqrl,
            uint256 paritySwapIn,
            uint256 paritySwapOut,
            bool discountClosed
        )
    {
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();
        _updatePriceCumulative();

        uint256 p = oracle.price();
        if (p == 0 || !oracle.healthy()) revert OracleUnhealthy();

        // Compare current ratio to canonical via cross-product to avoid
        // intermediate division. lhs > rhs ⟺ B/A > p^2 ⟺ discount.
        uint256 lhs = poolIQRL * SCALE * SCALE;
        uint256 rhs = poolQRL * p * p;

        if (lhs > rhs) {
            // Discount: swap QRL→iQRL to bring pool to parity.
            // target_A = sqrt(k) * SCALE / p; spend = target_A - poolQRL.
            uint256 targetA = (Math.sqrt(poolQRL * poolIQRL) * SCALE) / p;
            paritySwapIn = targetA - poolQRL;
            if (msg.value < paritySwapIn) revert UnexpectedValue(paritySwapIn, msg.value);

            paritySwapOut = (poolIQRL * paritySwapIn) / (poolQRL + paritySwapIn);
            if (paritySwapOut < minOutForParity) revert SlippageExceeded(paritySwapOut, minOutForParity);

            poolQRL += paritySwapIn;
            poolIQRL -= paritySwapOut;
            emit Swapped(msg.sender, true, paritySwapIn, paritySwapOut);

            iqrl.safeTransfer(msg.sender, paritySwapOut);
            discountClosed = true;
        } else if (lhs < rhs) {
            // Premium: swap iQRL→QRL to bring pool to parity.
            uint256 targetB = (Math.sqrt(poolQRL * poolIQRL) * p) / SCALE;
            paritySwapIn = targetB - poolIQRL;
            if (paritySwapIn > maxIqrlInForParity) revert SlippageExceeded(paritySwapIn, maxIqrlInForParity);

            paritySwapOut = (poolQRL * paritySwapIn) / (poolIQRL + paritySwapIn);
            if (paritySwapOut < minOutForParity) revert SlippageExceeded(paritySwapOut, minOutForParity);

            iqrl.safeTransferFrom(msg.sender, address(this), paritySwapIn);

            poolIQRL += paritySwapIn;
            poolQRL -= paritySwapOut;
            emit Swapped(msg.sender, false, paritySwapIn, paritySwapOut);

            _payNative(msg.sender, paritySwapOut);
            discountClosed = false;
        }
        // else: pool already at parity (within rounding); no parity swap.

        // Pool is now at parity. Open the position.
        (positionId, iqrlAmount, feeIqrl) = _openPositionUnchecked(qrlAmount, durationSeconds, maxFeeIqrl);

        // Refund any unused QRL (always safe — covers both branches).
        uint256 qrlSpent = lhs > rhs ? paritySwapIn : 0;
        if (msg.value > qrlSpent) {
            _payNative(msg.sender, msg.value - qrlSpent);
        }
    }

    /// @dev Internal core of open-position. Caller is responsible for
    ///      ensuring the pool is at parity before invocation; this
    ///      function does no parity check.
    function _openPositionUnchecked(uint128 qrlAmount, uint64 durationSeconds, uint256 maxFeeIqrl)
        internal
        returns (uint256 positionId, uint256 iqrlAmount, uint256 feeIqrl)
    {
        if (qrlAmount == 0) revert ZeroAmount();
        if (durationSeconds == 0 || durationSeconds > LEVERAGE_MAX_DURATION) revert DurationOutOfRange();
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();

        _updatePriceCumulative();

        // 50%-of-pool cap on aggregate lent. The cap on QRL implies
        // the same cap on iQRL because all loans are symmetric at the
        // pool's marginal ratio at the moment of opening.
        uint256 totalQ = poolQRL + lentQRL;
        uint256 capQ = (totalQ * LEVERAGE_POOL_CAP_BPS) / LEVERAGE_BPS;
        if (lentQRL + qrlAmount > capQ) {
            revert LeverageCapExceeded(lentQRL + qrlAmount, capQ);
        }

        iqrlAmount = (uint256(qrlAmount) * poolIQRL) / poolQRL;
        if (iqrlAmount == 0 || iqrlAmount > type(uint128).max) revert ZeroAmount();

        // Borrower pays the rate at POST-borrow utilization. A
        // borrower who consumes the last bit of available depth pays
        // the cap rate; an earlier borrower against zero utilization
        // pays a lower rate. This mirrors the Compound/Aave pattern
        // where rate is a function of post-state utilization.
        uint256 rateBps = _rateAt(lentQRL + uint256(qrlAmount));
        uint256 p = oracle.price();
        if (p == 0 || !oracle.healthy()) revert OracleUnhealthy();

        feeIqrl = _computeFeeIqrl(uint256(qrlAmount), iqrlAmount, p, rateBps, durationSeconds);
        if (feeIqrl > maxFeeIqrl) revert InsufficientFee(feeIqrl, maxFeeIqrl);

        // Route the fee to yQSD stakers. Pull the borrower's iQRL into
        // this contract, then push it to yQSD via distribute(). yQSD
        // updates its accRewardPerShare and credits stakers pro-rata.
        // The fee transitorily enters this contract's accounting (one
        // block, one tx) but never affects pool reserves.
        iqrl.safeTransferFrom(msg.sender, address(this), feeIqrl);
        iqrl.safeIncreaseAllowance(address(yieldQsd), feeIqrl);
        yieldQsd.distribute(feeIqrl);

        // Move tokens from pool to sandbox. Aggregate (pool + lent) is
        // preserved exactly; pool's k drops, lent's k rises, but the
        // aggregate solvency invariant uses pool+lent which is
        // unchanged.
        poolQRL -= qrlAmount;
        poolIQRL -= iqrlAmount;
        lentQRL += qrlAmount;
        lentIQRL += iqrlAmount;

        positionId = nextPositionId++;
        positions[positionId] = Position({
            owner: msg.sender,
            qrlOwed: qrlAmount,
            iqrlOwed: uint128(iqrlAmount),
            qrlBalance: qrlAmount,
            iqrlBalance: uint128(iqrlAmount),
            deadline: uint64(block.timestamp) + durationSeconds
        });

        _assertSolvent();

        emit PositionOpened(
            positionId, msg.sender,
            qrlAmount, iqrlAmount,
            feeIqrl, rateBps,
            uint64(block.timestamp) + durationSeconds
        );
    }

    /// @notice Sandbox-swap QRL→iQRL for `positionId`. Only callable by
    ///         the position owner. The swap routes through the pool
    ///         and must monotonically reduce the gap between sandbox
    ///         and pool ratios (else reverts).
    function sandboxSwapQrlForIqrl(uint256 positionId, uint128 qrlIn, uint256 minIqrlOut)
        external
        nonReentrant
        returns (uint256 iqrlOut)
    {
        Position storage pos = positions[positionId];
        if (pos.owner != msg.sender) revert PositionNotOwned();
        if (block.timestamp >= pos.deadline) revert PositionExpired();
        if (qrlIn == 0) revert ZeroAmount();
        if (qrlIn > pos.qrlBalance) revert InsufficientSandboxBalance();
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();

        _updatePriceCumulative();

        // Snapshot before-state for convergence check.
        uint256 sQ_before = pos.qrlBalance;
        uint256 sI_before = pos.iqrlBalance;
        uint256 pQ_before = poolQRL;
        uint256 pI_before = poolIQRL;

        // Standard CPMM quote at current pool reserves.
        iqrlOut = (poolIQRL * uint256(qrlIn)) / (poolQRL + uint256(qrlIn));
        if (iqrlOut < minIqrlOut) revert SlippageExceeded(iqrlOut, minIqrlOut);

        // Apply the swap to both sandbox and pool. Aggregate preserved.
        pos.qrlBalance = uint128(uint256(pos.qrlBalance) - uint256(qrlIn));
        pos.iqrlBalance = uint128(uint256(pos.iqrlBalance) + iqrlOut);
        poolQRL += qrlIn;
        poolIQRL -= iqrlOut;
        lentQRL -= qrlIn;
        lentIQRL += iqrlOut;

        _assertConverges(
            sQ_before, sI_before, pos.qrlBalance, pos.iqrlBalance,
            pQ_before, pI_before, poolQRL, poolIQRL
        );

        _assertSolvent();

        emit SandboxSwapped(positionId, true, qrlIn, iqrlOut);
    }

    /// @notice Sandbox-swap iQRL→QRL for `positionId`. Mirror of the
    ///         QRL→iQRL path; same convergence requirement.
    function sandboxSwapIqrlForQrl(uint256 positionId, uint128 iqrlIn, uint256 minQrlOut)
        external
        nonReentrant
        returns (uint256 qrlOut)
    {
        Position storage pos = positions[positionId];
        if (pos.owner != msg.sender) revert PositionNotOwned();
        if (block.timestamp >= pos.deadline) revert PositionExpired();
        if (iqrlIn == 0) revert ZeroAmount();
        if (iqrlIn > pos.iqrlBalance) revert InsufficientSandboxBalance();
        if (poolQRL == 0 || poolIQRL == 0) revert EmptyPool();

        _updatePriceCumulative();

        uint256 sQ_before = pos.qrlBalance;
        uint256 sI_before = pos.iqrlBalance;
        uint256 pQ_before = poolQRL;
        uint256 pI_before = poolIQRL;

        qrlOut = (poolQRL * uint256(iqrlIn)) / (poolIQRL + uint256(iqrlIn));
        if (qrlOut < minQrlOut) revert SlippageExceeded(qrlOut, minQrlOut);

        pos.iqrlBalance = uint128(uint256(pos.iqrlBalance) - uint256(iqrlIn));
        pos.qrlBalance = uint128(uint256(pos.qrlBalance) + qrlOut);
        poolIQRL += iqrlIn;
        poolQRL -= qrlOut;
        lentIQRL -= iqrlIn;
        lentQRL += qrlOut;

        _assertConverges(
            sQ_before, sI_before, pos.qrlBalance, pos.iqrlBalance,
            pQ_before, pI_before, poolQRL, poolIQRL
        );

        _assertSolvent();

        emit SandboxSwapped(positionId, false, iqrlIn, qrlOut);
    }

    /// @notice Voluntarily close a position before its deadline.
    ///         Reverts if the resulting payout would break aggregate
    ///         solvency; in that case the borrower may wait for pool
    ///         depth to grow (deposits, external arb slippage) and
    ///         retry, or accept force-close after the deadline.
    function closePosition(uint256 positionId)
        external
        nonReentrant
        returns (uint256 qrlPaid, uint256 iqrlPaid)
    {
        Position storage pos = positions[positionId];
        if (pos.owner != msg.sender) revert PositionNotOwned();
        return _settle(positionId, pos, /*forced=*/false);
    }

    /// @notice Force-close an expired position. Anyone may call this
    ///         after the deadline. Returns full sandbox to the pool;
    ///         pays the borrower the excess only if aggregate
    ///         solvency permits, otherwise borrower receives zero
    ///         excess and pool retains the full sandbox.
    function forceClosePosition(uint256 positionId)
        external
        nonReentrant
        returns (uint256 qrlPaid, uint256 iqrlPaid)
    {
        Position storage pos = positions[positionId];
        if (pos.owner == address(0)) revert PositionNotFound();
        if (block.timestamp < pos.deadline) revert PositionStillActive();
        return _settle(positionId, pos, /*forced=*/true);
    }

    /// @dev Compute the iQRL fee for a hypothetical loan. Fee is the
    ///      annualized USD-canonical loan value times the rate over
    ///      duration, converted to iQRL units (1 USD = p iQRL canonical).
    function _computeFeeIqrl(
        uint256 qrlAmount,
        uint256 iqrlAmount,
        uint256 p,
        uint256 rateBps,
        uint64 durationSeconds
    ) internal pure returns (uint256) {
        // loanUsd = qrl*p + iqrl/p (1e18 scale)
        uint256 loanUsd = (qrlAmount * p) / SCALE + (iqrlAmount * SCALE) / p;
        // feeUsd = loanUsd * rate * dur / (BPS * year)
        uint256 feeUsd = (loanUsd * rateBps * uint256(durationSeconds)) / (LEVERAGE_BPS * LEVERAGE_SECONDS_PER_YEAR);
        // feeIqrl = feeUsd * p (since 1 USD = p iQRL canonical)
        return (feeUsd * p) / SCALE;
    }

    /// @dev Convergence check: each sandbox swap must monotonically
    ///      reduce |sandbox.R - pool.R|, where R = iqrl/qrl. Compares
    ///      ratios in 1e18 fixed-point (one division each side; precision
    ///      loss is acceptable for the gating decision).
    function _assertConverges(
        uint256 sQb, uint256 sIb,
        uint256 sQa, uint256 sIa,
        uint256 pQb, uint256 pIb,
        uint256 pQa, uint256 pIa
    ) internal pure {
        if (sQb == 0 || sQa == 0 || pQb == 0 || pQa == 0) revert ConvergenceViolation();
        uint256 Rs_before = (sIb * SCALE) / sQb;
        uint256 Rp_before = (pIb * SCALE) / pQb;
        uint256 Rs_after = (sIa * SCALE) / sQa;
        uint256 Rp_after = (pIa * SCALE) / pQa;
        uint256 gapBefore = Rs_before > Rp_before ? Rs_before - Rp_before : Rp_before - Rs_before;
        uint256 gapAfter  = Rs_after  > Rp_after  ? Rs_after  - Rp_after  : Rp_after  - Rs_after;
        if (gapAfter > gapBefore) revert ConvergenceViolation();
    }

    /// @dev Settlement implementation shared by close and forceClose.
    ///      Pool reclaims (takeQ, takeI) with product = k_loan at the
    ///      sandbox's current ratio. Borrower keeps the excess subject
    ///      to aggregate solvency.
    function _settle(uint256 positionId, Position storage pos, bool forced)
        internal
        returns (uint256 qrlPaid, uint256 iqrlPaid)
    {
        _updatePriceCumulative();

        address owner = pos.owner;
        (uint256 takeQ, uint256 takeI) = _computeTake(pos);
        uint256 sQ = pos.qrlBalance;
        uint256 sI = pos.iqrlBalance;

        // Apply settlement to pool/lent state.
        poolQRL += takeQ;
        poolIQRL += takeI;
        lentQRL -= sQ;
        lentIQRL -= sI;

        // If standard take breaks aggregate solvency: voluntary close
        // reverts, forced close eats borrower's excess into the pool.
        if (2 * Math.sqrt((poolQRL + lentQRL) * (poolIQRL + lentIQRL)) < totalSupply()) {
            if (!forced) revert SolvencyWouldBreak();
            poolQRL += (sQ - takeQ);
            poolIQRL += (sI - takeI);
            takeQ = sQ;
            takeI = sI;
        }

        qrlPaid = sQ - takeQ;
        iqrlPaid = sI - takeI;

        delete positions[positionId];

        if (iqrlPaid > 0) iqrl.safeTransfer(owner, iqrlPaid);
        if (qrlPaid > 0) _payNative(owner, qrlPaid);

        _assertSolvent();

        emit PositionClosed(positionId, owner, takeQ, takeI, qrlPaid, iqrlPaid, forced);
    }

    /// @dev Compute the pool's take from a sandbox at settlement.
    ///      Slice has canonical USD value == loan_USD_at_current_p, taken
    ///      at the sandbox's current ratio. Borrower keeps the residual,
    ///      which is the captured gamma in USD terms.
    ///
    ///      The factor `loan_USD / sandbox_USD` extracts exactly the
    ///      original loan principal in canonical USD; the borrower's
    ///      payout is the convexity premium their position accumulated
    ///      via convergence trades against off-canonical pool states.
    ///
    ///      Falls back to k-based slicing if the oracle is unhealthy
    ///      (canonical USD valuation requires a live price).
    function _computeTake(Position storage pos)
        internal
        view
        returns (uint256 takeQ, uint256 takeI)
    {
        uint256 sQ = pos.qrlBalance;
        uint256 sI = pos.iqrlBalance;
        if (sQ == 0 && sI == 0) return (0, 0);

        uint256 p = oracle.healthy() ? oracle.price() : 0;
        if (p > 0 && sQ > 0 && sI > 0) {
            // USD-based take: factor = loan_USD / sandbox_USD.
            uint256 loanUSD =
                (uint256(pos.qrlOwed) * p) / SCALE +
                (uint256(pos.iqrlOwed) * SCALE) / p;
            uint256 sandboxUSD = (sQ * p) / SCALE + (sI * SCALE) / p;
            if (sandboxUSD <= loanUSD) {
                // Sandbox failed to bank gamma (rare in zero-fee CPMM,
                // possible if oracle p moved unfavorably vs loan ratio).
                // Pool takes everything; borrower forfeits.
                takeQ = sQ;
                takeI = sI;
            } else {
                uint256 sliceFactor = (loanUSD * SCALE) / sandboxUSD;
                takeQ = (sQ * sliceFactor) / SCALE;
                takeI = (sI * sliceFactor) / SCALE;
                // Round up by one wei on each leg to ensure pool's claim
                // is satisfied exactly under integer arithmetic.
                if (takeQ < sQ) takeQ += 1;
                if (takeI < sI) takeI += 1;
            }
        } else {
            // Oracle unhealthy: fall back to k-based slice. This
            // under-rewards gamma but preserves solvency without
            // requiring oracle access at settle.
            uint256 kLoan = uint256(pos.qrlOwed) * uint256(pos.iqrlOwed);
            uint256 kSandbox = sQ * sI;
            if (kSandbox >= kLoan && sQ > 0 && sI > 0) {
                uint256 sliceFactor = Math.sqrt((kLoan * SCALE * SCALE) / kSandbox);
                takeQ = (sQ * sliceFactor) / SCALE;
                takeI = (sI * sliceFactor) / SCALE;
                if (takeQ < sQ) takeQ += 1;
                if (takeI < sI) takeI += 1;
            } else {
                takeQ = sQ;
                takeI = sI;
            }
        }
        if (takeQ > sQ) takeQ = sQ;
        if (takeI > sI) takeI = sI;
    }
}
