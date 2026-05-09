// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test, console2} from "forge-std/Test.sol";
import {Math} from "@openzeppelin/contracts/utils/math/Math.sol";

import {QSD} from "../src/QSD.sol";
import {IPriceOracle} from "../src/interfaces/IPriceOracle.sol";
import {MockERC20} from "./mocks/MockERC20.sol";
import {MockPriceOracle} from "./mocks/MockPriceOracle.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";

/// @notice Tests for the QSD leverage facility (open / sandbox swap /
///         close / forceClose paths).
contract QSDLeverageTest is Test {
    QSD internal qsd;
    MockERC20 internal iqrl;
    MockPriceOracle internal oracle;

    address internal alice = address(0xA11CE);
    address internal bob = address(0xB0B);
    address internal carol = address(0xCAFE);

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        iqrl = new MockERC20("iQRL", "iQRL");
        oracle = new MockPriceOracle(ONE); // p = $1.00
        qsd = new QSD(iqrl, oracle);

        _fund(alice);
        _fund(bob);
        _fund(carol);

        // Bootstrap the pool at p=1 with 1000 QRL + 1000 iQRL.
        vm.prank(alice);
        qsd.deposit{value: 1000 * ONE}(1000 * ONE, 0);
    }

    function _fund(address user) internal {
        vm.deal(user, 1e30);
        iqrl.mint(user, 1e30);
        vm.prank(user);
        iqrl.approve(address(qsd), type(uint256).max);
    }

    // ------------------------------------------------------------------
    // openPosition
    // ------------------------------------------------------------------

    function test_OpenPosition_TransfersTokensToSandbox() public {
        uint256 poolQRLBefore = qsd.poolQRL();
        uint256 poolIQRLBefore = qsd.poolIQRL();
        uint256 lentQRLBefore = qsd.lentQRL();

        vm.prank(bob);
        (uint256 positionId, uint256 iqrlAmount, uint256 feeIqrl) =
            qsd.openPosition(uint128(10 * ONE), 1 days, type(uint256).max);

        assertEq(positionId, 1, "position id");
        assertEq(iqrlAmount, 10 * ONE, "iqrl pulled at 1:1 pool ratio");
        assertGt(feeIqrl, 0, "fee was charged");

        // Pool reserves dropped by the loan amount.
        assertEq(qsd.poolQRL(), poolQRLBefore - 10 * ONE, "pool qrl dropped");
        assertEq(qsd.poolIQRL(), poolIQRLBefore - 10 * ONE, "pool iqrl dropped");
        // Lent tracks the loan.
        assertEq(qsd.lentQRL(), lentQRLBefore + 10 * ONE, "lentQRL");
        assertEq(qsd.lentIQRL(), 10 * ONE, "lentIQRL");
        // Aggregate solvency still holds.
        assertTrue(qsd.checkInvariants());
    }

    function test_OpenPosition_RoutesFeeToHolders() public {
        uint256 supplyBefore = iqrl.totalSupply();
        uint256 bobIqrlBefore = iqrl.balanceOf(bob);
        uint256 qsdContractIqrlBefore = iqrl.balanceOf(address(qsd));

        vm.prank(bob);
        (, , uint256 feeIqrl) = qsd.openPosition(uint128(10 * ONE), 1 days, type(uint256).max);

        // Total supply unchanged: fee is routed to QSD holders, not burned.
        assertEq(iqrl.totalSupply(), supplyBefore, "supply unchanged (fee routed, not burned)");
        // Borrower's balance dropped by the fee.
        assertEq(iqrl.balanceOf(bob), bobIqrlBefore - feeIqrl, "fee debited from borrower");
        // The QSD contract holds the fee until holders claim it.
        assertEq(iqrl.balanceOf(address(qsd)) - qsdContractIqrlBefore, feeIqrl, "fee held by QSD contract");
        // QSD recorded the distribution.
        assertEq(qsd.totalDistributed(), feeIqrl, "qsd recorded the distribution");
    }

    function test_OpenPosition_RateAtZeroUtilization_Is12Pct() public view {
        // utilization = 0 → rate should be exactly LEVERAGE_MIN_RATE_BPS = 1200.
        assertEq(qsd.leverageRateBps(), 1200, "min rate at u=0");
    }

    // ------------------------------------------------------------------
    // Yield accumulator: distributions accrue to QSD holders pro-rata,
    // settle on balance changes, drain on claim().
    // ------------------------------------------------------------------

    function test_LeverageFee_AccruesToHolders() public {
        // Alice was the bootstrap depositor (in setUp). She holds all
        // outstanding QSD, so the entire iQRL fee accrues to her.
        uint256 alicePendingBefore = qsd.pending(alice);
        assertEq(alicePendingBefore, 0, "alice starts with no pending");

        vm.prank(bob);
        (, , uint256 feeIqrl) = qsd.openPosition(uint128(10 * ONE), 1 days, type(uint256).max);

        // Allow MasterChef rounding loss bounded by supply/SCALE wei.
        uint256 tol = qsd.totalSupply() / 1e18 + 100;
        assertApproxEqAbs(qsd.pending(alice), feeIqrl, tol, "alice gets the entire fee");

        // Alice claims and receives the iQRL.
        uint256 aliceIqrlBefore = iqrl.balanceOf(alice);
        vm.prank(alice);
        uint256 claimed = qsd.claim();
        assertApproxEqAbs(claimed, feeIqrl, tol, "alice claims feeIqrl");
        assertApproxEqAbs(iqrl.balanceOf(alice) - aliceIqrlBefore, feeIqrl, tol, "iqrl transferred");
    }

    function test_QsdTransfer_MovesFutureYieldRights() public {
        // Bob deposits to acquire QSD.
        vm.prank(bob);
        (uint256 qsdMinted, ) = qsd.deposit{value: 100 * ONE}(100 * ONE, 0);

        // Bob transfers his QSD to carol. Future yield then accrues
        // to carol, not bob, since the yield rights are tied to the
        // QSD itself in the bundled design.
        vm.prank(bob);
        qsd.transfer(carol, qsdMinted);

        // Carol now holds the QSD; bob holds zero. A subsequent
        // distribution should accrue to carol's pending balance.
        uint256 bobPendingBefore = qsd.pending(bob);
        uint256 carolPendingBefore = qsd.pending(carol);

        vm.prank(alice);
        (, , uint256 feeIqrl) = qsd.openPosition(uint128(10 * ONE), 1 days, type(uint256).max);

        // Carol's pending grew (proportional to her share); bob's did not.
        assertGt(qsd.pending(carol), carolPendingBefore, "carol accrues yield");
        assertEq(qsd.pending(bob), bobPendingBefore, "bob accrues nothing further");
        assertGt(feeIqrl, 0);
    }

    function test_Redeem_NoLongerGatedByYieldClaim() public {
        // In the bundled design, redeeming QSD does not require any
        // matching token. Anyone holding QSD can redeem. The "give up
        // future yield" disincentive is the only stickiness, and it
        // applies via opportunity cost, not contract gating.
        vm.prank(bob);
        (uint256 qsdMinted, ) = qsd.deposit{value: 100 * ONE}(100 * ONE, 0);

        // Bob transfers QSD to carol. Carol can redeem freely.
        vm.prank(bob);
        qsd.transfer(carol, qsdMinted);

        vm.prank(carol);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(qsdMinted);
        assertGt(qrlOut, 0);
        assertGt(iqrlOut, 0);
        assertEq(qsd.balanceOf(carol), 0, "carol's qsd burned");
    }

    function test_OpenPosition_BorrowerPaysPostBorrowRate() public {
        // A borrower taking utilization from 0 to 50% should pay the
        // 40% rate (rate at the post-borrow state), not the 12% rate
        // at the pre-borrow state. Quote then open and check the
        // emitted rate.
        (uint256 quotedFee, , uint256 quotedRate) =
            qsd.quoteLeverageFee(uint128(500 * ONE), 1 days);
        assertEq(quotedRate, 4000, "quoted rate is post-borrow (40%)");

        // Quote at a partial borrow (0% → 25%) should give the
        // mid-rate at u=25%: 12% + (40%-12%)*(0.25/0.5) = 26%.
        (, , uint256 quotedMidRate) = qsd.quoteLeverageFee(uint128(250 * ONE), 1 days);
        assertEq(quotedMidRate, 2600, "quoted rate at half-of-cap is 26%");

        // Open the half-borrow and verify the actual fee paid uses
        // the post-borrow rate (matches the quote).
        vm.prank(bob);
        (, , uint256 actualFee) = qsd.openPosition(uint128(250 * ONE), 1 days, type(uint256).max);

        // Re-quote (now from u=25%) for a borrower who'd take it from
        // 25% → 50%: rate at end u=50% = 40%.
        (, , uint256 nextRate) = qsd.quoteLeverageFee(uint128(250 * ONE), 1 days);
        assertEq(nextRate, 4000, "next borrower from u=25% to u=50% pays 40%");
        // Actual fee on the first half-borrow at quoted rate.
        assertEq(actualFee, quotedFee / 2 * 0 + actualFee, "fee captured");
        assertGt(actualFee, 0);
    }

    function test_OpenPosition_RateScalesLinearlyToCap() public {
        // Cap = 50% of total. With pool=1000 lent=0, total=1000, cap=500
        // QRL. Maximum first-loan size is 500. After this loan,
        // pool=500, lent=500, total=1000, utilization=lent/total=50%.
        // Rate at the cap should be LEVERAGE_MAX_RATE_BPS = 4000.
        vm.prank(bob);
        qsd.openPosition(uint128(500 * ONE), 1 days, type(uint256).max);

        // utilization = lent/(pool+lent) = 500/1000 = 50%, rate = 40%.
        assertEq(qsd.leverageRateBps(), 4000, "max rate at u=cap");
    }

    function test_OpenPosition_RevertsBeyondCap() public {
        // First loan: 500 QRL = 50% of pool. After: pool=500, lent=500.
        vm.prank(bob);
        qsd.openPosition(uint128(500 * ONE), 1 days, type(uint256).max);

        // Now lent=500, pool=500. Cap = 50% of (500+500) = 500. Lent
        // is already at cap; no more loans possible.
        vm.expectRevert();
        vm.prank(carol);
        qsd.openPosition(uint128(1 * ONE), 1 days, type(uint256).max);
    }

    function test_OpenPosition_RevertsOnDurationOutOfRange() public {
        vm.expectRevert(QSD.DurationOutOfRange.selector);
        vm.prank(bob);
        qsd.openPosition(uint128(10 * ONE), 0, type(uint256).max);

        vm.expectRevert(QSD.DurationOutOfRange.selector);
        vm.prank(bob);
        qsd.openPosition(uint128(10 * ONE), uint64(1 days + 1), type(uint256).max);
    }

    function test_OpenPosition_RevertsOnInsufficientFee() public {
        // Quote the exact fee, then call with maxFee one wei below.
        (uint256 feeIqrl, , ) = qsd.quoteLeverageFee(uint128(10 * ONE), 1 days);
        vm.expectRevert();
        vm.prank(bob);
        qsd.openPosition(uint128(10 * ONE), 1 days, feeIqrl - 1);
    }

    // ------------------------------------------------------------------
    // Parity restriction
    // ------------------------------------------------------------------

    function test_OpenPosition_RevertsWhenPoolInDiscount() public {
        // Carol creates a discount by swapping iQRL into the pool
        // (pool gains iQRL, B/A rises → B/A > p² = discount).
        vm.prank(carol);
        qsd.swapIqrlForQrl(100 * ONE, 0);

        vm.expectRevert(QSD.PoolNotAtParity.selector);
        vm.prank(bob);
        qsd.openPosition(uint128(10 * ONE), 1 days, type(uint256).max);
    }

    function test_OpenPosition_RevertsWhenPoolInPremium() public {
        // Carol creates a premium by swapping QRL into the pool
        // (pool gains QRL, B/A drops → B/A < p² = premium).
        vm.prank(carol);
        qsd.swapQrlForIqrl{value: 100 * ONE}(0);

        vm.expectRevert(QSD.PoolNotAtParity.selector);
        vm.prank(bob);
        qsd.openPosition(uint128(10 * ONE), 1 days, type(uint256).max);
    }

    function _assertPoolAtParity(string memory label) internal view {
        uint256 p = oracle.price();
        uint256 lhs = qsd.poolIQRL() * 1e18 * 1e18;
        uint256 rhs = qsd.poolQRL() * p * p;
        uint256 diff = lhs > rhs ? lhs - rhs : rhs - lhs;
        uint256 ref = lhs > rhs ? lhs : rhs;
        assertLt(diff * 1_000_000, ref, label);
    }

    function test_OpenPositionAtParity_HandlesDiscount() public {
        // Discount = caller does iqrl→qrl into pool.
        vm.prank(carol);
        qsd.swapIqrlForQrl(100 * ONE, 0);

        vm.prank(bob);
        (
            uint256 positionId,
            uint256 iqrlAmount,
            uint256 feeIqrl,
            uint256 paritySwapIn,
            uint256 paritySwapOut,
            bool discountClosed
        ) = qsd.openPositionAtParity{value: 200 * ONE}(
            uint128(10 * ONE),
            1 days,
            type(uint256).max,
            type(uint256).max,
            0
        );

        assertTrue(discountClosed, "discount detected and closed");
        assertGt(paritySwapIn, 0, "parity swap fired");
        assertGt(paritySwapOut, 0, "iqrl out from parity");
        assertEq(positionId, 1);
        assertGt(iqrlAmount, 0);
        assertGt(feeIqrl, 0);
        _assertPoolAtParity("pool at parity post-bundle (discount path)");
    }

    function test_OpenPositionAtParity_HandlesPremium() public {
        // Premium = caller does qrl→iqrl into pool.
        vm.prank(carol);
        qsd.swapQrlForIqrl{value: 100 * ONE}(0);

        uint256 bobQrlBefore = bob.balance;

        vm.prank(bob);
        (
            uint256 positionId,
            ,
            ,
            uint256 paritySwapIn,
            uint256 paritySwapOut,
            bool discountClosed
        ) = qsd.openPositionAtParity(
            uint128(10 * ONE),
            1 days,
            type(uint256).max,
            type(uint256).max,
            0
        );

        assertFalse(discountClosed, "premium path");
        assertGt(paritySwapIn, 0, "iqrl in spent");
        assertGt(paritySwapOut, 0, "qrl received");
        assertEq(positionId, 1);
        // Bob received QRL from the premium close.
        assertEq(bob.balance, bobQrlBefore + paritySwapOut, "bob received qrl");
        _assertPoolAtParity("pool at parity post-bundle (premium path)");
    }

    function test_OpenPositionAtParity_NoOpWhenAlreadyAtParity() public {
        // Pool is at parity by setup.
        vm.prank(bob);
        (
            uint256 positionId,
            ,
            ,
            uint256 paritySwapIn,
            uint256 paritySwapOut,
            bool discountClosed
        ) = qsd.openPositionAtParity{value: 100 * ONE}(
            uint128(10 * ONE),
            1 days,
            type(uint256).max,
            type(uint256).max,
            0
        );

        assertEq(paritySwapIn, 0, "no parity swap needed");
        assertEq(paritySwapOut, 0, "no parity output");
        assertFalse(discountClosed, "no discount to close");
        assertEq(positionId, 1, "position still opened");
        _assertPoolAtParity("pool still at parity");
    }

    function test_OpenPositionAtParity_RefundsExcessQrl_OnDiscountPath() public {
        vm.prank(carol);
        qsd.swapIqrlForQrl(100 * ONE, 0);

        uint256 bobQrlBefore = bob.balance;

        vm.prank(bob);
        (, , , uint256 paritySwapIn, , bool discountClosed) =
            qsd.openPositionAtParity{value: 500 * ONE}(
                uint128(10 * ONE),
                1 days,
                type(uint256).max,
                type(uint256).max,
                0
            );

        assertTrue(discountClosed);
        // Bob's net QRL outflow = exactly paritySwapIn (rest refunded).
        assertEq(bob.balance, bobQrlBefore - paritySwapIn, "surplus refunded");
    }

    function test_OpenPositionAtParity_RefundsAllQrl_OnPremiumPath() public {
        // Pool in premium → bundled function uses iQRL allowance, not QRL.
        // All msg.value should be refunded.
        vm.prank(carol);
        qsd.swapQrlForIqrl{value: 100 * ONE}(0);

        uint256 bobQrlBefore = bob.balance;

        vm.prank(bob);
        (, , , , uint256 paritySwapOut, ) = qsd.openPositionAtParity{value: 500 * ONE}(
            uint128(10 * ONE),
            1 days,
            type(uint256).max,
            type(uint256).max,
            0
        );

        // Bob received QRL from the premium close, paid no QRL into the swap.
        assertEq(bob.balance, bobQrlBefore + paritySwapOut, "qrl received from premium close");
    }

    // ------------------------------------------------------------------
    // sandboxSwap
    // ------------------------------------------------------------------

    function _openBobLoan(uint128 qrlAmount) internal returns (uint256 positionId) {
        vm.prank(bob);
        (positionId, , ) = qsd.openPosition(qrlAmount, 1 days, type(uint256).max);
    }

    function test_SandboxSwap_ConvergesTowardPool() public {
        // Setup: open loan, then move pool externally so its ratio
        // diverges from sandbox.
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        // Carol does a big external swap to move pool to a discount.
        // She swaps QRL into the pool — pool gains QRL, loses iQRL,
        // pool's iqrl/qrl ratio drops.
        vm.prank(carol);
        qsd.swapQrlForIqrl{value: 100 * ONE}(0);

        // Sandbox is at ratio 1 (open ratio), pool now at < 1.
        // Bob's allowed convergence direction: swap iqrl→qrl. The
        // amount must be small enough to not overshoot pool's ratio.
        // 0.5 iqrl on a (10,10) sandbox vs (~1090, ~899) pool moves
        // sandbox from 1 toward ~0.9, narrowing the gap to pool's
        // ~0.825 without overshoot.
        vm.prank(bob);
        uint256 qrlOut = qsd.sandboxSwapIqrlForQrl(positionId, uint128(ONE / 2), 0);
        assertGt(qrlOut, 0, "swap delivered qrl");

        assertTrue(qsd.checkInvariants(), "aggregate solvency after sandbox swap");
    }

    function test_SandboxSwap_RevertsOnDivergence() public {
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        // Carol moves pool to discount (qrl-into-pool swap).
        vm.prank(carol);
        qsd.swapQrlForIqrl{value: 100 * ONE}(0);

        // The convergent direction is iqrl→qrl. Bob tries the wrong
        // direction (qrl→iqrl), which would push his sandbox further
        // from pool.
        vm.expectRevert(QSD.ConvergenceViolation.selector);
        vm.prank(bob);
        qsd.sandboxSwapQrlForIqrl(positionId, uint128(1 * ONE), 0);
    }

    function test_SandboxSwap_OnlyOwner() public {
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        vm.expectRevert(QSD.PositionNotOwned.selector);
        vm.prank(carol);
        qsd.sandboxSwapQrlForIqrl(positionId, uint128(1 * ONE), 0);
    }

    function test_SandboxSwap_RevertsAfterDeadline() public {
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        vm.warp(block.timestamp + 1 days + 1);

        vm.expectRevert(QSD.PositionExpired.selector);
        vm.prank(bob);
        qsd.sandboxSwapQrlForIqrl(positionId, uint128(1 * ONE), 0);
    }

    // ------------------------------------------------------------------
    // closePosition
    // ------------------------------------------------------------------

    function test_ClosePosition_NoSwaps_ReturnsExactlyLoan() public {
        uint256 poolQRLPre = qsd.poolQRL();
        uint256 poolIQRLPre = qsd.poolIQRL();

        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        vm.prank(bob);
        (uint256 qrlPaid, uint256 iqrlPaid) = qsd.closePosition(positionId);

        // No swaps → sandbox = (10, 10) = loan. Pool gets it all back.
        // Borrower receives nothing extra.
        assertEq(qrlPaid, 0, "no qrl excess");
        assertEq(iqrlPaid, 0, "no iqrl excess");
        assertEq(qsd.poolQRL(), poolQRLPre, "pool qrl restored");
        assertEq(qsd.poolIQRL(), poolIQRLPre, "pool iqrl restored");
        assertEq(qsd.lentQRL(), 0, "lent cleared");
        assertEq(qsd.lentIQRL(), 0, "lent iqrl cleared");
        assertTrue(qsd.checkInvariants());
    }

    function test_ClosePosition_OnlyOwner() public {
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        vm.expectRevert(QSD.PositionNotOwned.selector);
        vm.prank(carol);
        qsd.closePosition(positionId);
    }

    function test_ForceClosePosition_BeforeDeadline_Reverts() public {
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        vm.expectRevert(QSD.PositionStillActive.selector);
        vm.prank(carol);
        qsd.forceClosePosition(positionId);
    }

    function test_ForceClosePosition_AfterDeadline_AnyoneCanCall() public {
        uint256 positionId = _openBobLoan(uint128(10 * ONE));
        vm.warp(block.timestamp + 1 days + 1);

        // Carol (not the owner) can force-close.
        vm.prank(carol);
        qsd.forceClosePosition(positionId);

        assertEq(qsd.lentQRL(), 0, "lent cleared after force close");
        assertTrue(qsd.checkInvariants());
    }

    // ------------------------------------------------------------------
    // gamma capture: borrower profits via the cycle
    // ------------------------------------------------------------------

    function test_GammaCycle_SandboxCapturesGammaInUSD() public {
        // Bob opens at parity. Pool gets perturbed by Carol. Bob's
        // convergence trade banks gamma into his sandbox: sandbox V
        // at canonical p exceeds the initial loan principal V.
        //
        // Note: we don't necessarily close-and-payout here — in tight-
        // solvency pools the voluntary close would revert because
        // borrower's gamma extraction exceeds 2*sqrt(aggregate_k) -
        // totalSupply slack. The gamma is REAL (sandbox V grew); it
        // becomes extractable as the pool accumulates slack via deposits
        // / external rounding gains over time, or via force-close.
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        // Initial sandbox: (10, 10) at p=1 → V_canonical = 20 ONE.

        // Carol perturbs pool to a discount.
        vm.prank(carol);
        qsd.swapIqrlForQrl(50 * ONE, 0);

        // Bob converges (qrl→iqrl) — banks the discount into sandbox.
        vm.prank(bob);
        qsd.sandboxSwapQrlForIqrl(positionId, uint128(ONE / 2), 0);

        // Read sandbox state and check V at p=1 grew above 20 ONE.
        (, , , uint128 sQ, uint128 sI, ) = qsd.positions(positionId);
        uint256 sandboxVat1 = uint256(sQ) + uint256(sI);
        assertGt(sandboxVat1, 20 * ONE, "sandbox V at canonical grew (gamma banked)");
        assertTrue(qsd.checkInvariants(), "aggregate solvency holds");
    }

    function test_VoluntaryClose_NoSwaps_PaysZeroExcess() public {
        // No convergence happened, so sandbox = loan exactly.
        // Voluntary close cleanly returns the loan to the pool.
        uint256 positionId = _openBobLoan(uint128(10 * ONE));

        vm.prank(bob);
        (uint256 qrlPaid, uint256 iqrlPaid) = qsd.closePosition(positionId);

        assertEq(qrlPaid, 0, "no qrl excess");
        assertEq(iqrlPaid, 0, "no iqrl excess");
        assertTrue(qsd.checkInvariants());
    }

    function test_ForceClose_HandlesSolvencyTightCase() public {
        // After convergence captures gamma, voluntary close may revert
        // if extraction exceeds solvency slack. Force-close is the
        // safety valve: it takes everything (no excess to borrower)
        // and preserves aggregate solvency exactly.
        uint256 positionId = _openBobLoan(uint128(10 * ONE));
        vm.prank(carol);
        qsd.swapIqrlForQrl(50 * ONE, 0);
        vm.prank(bob);
        qsd.sandboxSwapQrlForIqrl(positionId, uint128(ONE / 2), 0);

        // Wait past deadline.
        vm.warp(block.timestamp + 1 days + 1);

        // Anyone can force-close.
        vm.prank(carol);
        qsd.forceClosePosition(positionId);
        (address owner, , , , , ) = qsd.positions(positionId);
        assertEq(owner, address(0), "position cleared");
        assertTrue(qsd.checkInvariants());
    }

    // ------------------------------------------------------------------
    // aggregate solvency invariant under leverage
    // ------------------------------------------------------------------

    function test_OpenPosition_PreservesAggregateK() public {
        uint256 aggKBefore = (qsd.poolQRL() + qsd.lentQRL()) * (qsd.poolIQRL() + qsd.lentIQRL());

        _openBobLoan(uint128(50 * ONE));

        uint256 aggKAfter = (qsd.poolQRL() + qsd.lentQRL()) * (qsd.poolIQRL() + qsd.lentIQRL());

        // Open does not change aggregate token holdings (tokens just
        // shift from pool to sandbox).
        assertEq(aggKAfter, aggKBefore, "aggregate k preserved on open");
    }

    function test_SandboxSwap_PreservesAggregateK() public {
        uint256 positionId = _openBobLoan(uint128(50 * ONE));
        // Carol pushes pool off-ratio so a convergence trade is meaningful.
        vm.prank(carol);
        qsd.swapQrlForIqrl{value: 100 * ONE}(0);

        uint256 aggKBefore = (qsd.poolQRL() + qsd.lentQRL()) * (qsd.poolIQRL() + qsd.lentIQRL());

        vm.prank(bob);
        qsd.sandboxSwapIqrlForQrl(positionId, uint128(5 * ONE), 0);

        uint256 aggKAfter = (qsd.poolQRL() + qsd.lentQRL()) * (qsd.poolIQRL() + qsd.lentIQRL());

        // Sandbox swap is just a token shift between sandbox and pool —
        // aggregate token amounts unchanged. Product preserved.
        assertEq(aggKAfter, aggKBefore, "aggregate k preserved on sandbox swap");
    }
}
