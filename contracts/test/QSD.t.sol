// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test, console2} from "forge-std/Test.sol";
import {Math} from "@openzeppelin/contracts/utils/math/Math.sol";

import {QSD} from "../src/QSD.sol";
import {IPriceOracle} from "../src/interfaces/IPriceOracle.sol";
import {MockERC20} from "./mocks/MockERC20.sol";
import {MockPriceOracle} from "./mocks/MockPriceOracle.sol";

/// @notice Unit + fuzz tests for the QSD contract.
contract QSDTest is Test {
    QSD internal qsd;
    MockERC20 internal iqrl;
    MockPriceOracle internal oracle;

    address internal alice = address(0xA11CE);
    address internal bob = address(0xB0B);

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        iqrl = new MockERC20("iQRL", "iQRL");
        oracle = new MockPriceOracle(ONE); // p = $1.00
        qsd = new QSD(iqrl, oracle);

        _fund(alice);
        _fund(bob);
    }

    function _fund(address user) internal {
        vm.deal(user, 1e30);
        iqrl.mint(user, 1e30);
        vm.prank(user);
        iqrl.approve(address(qsd), type(uint256).max);
    }

    // ------------------------------------------------------------------
    // deposit
    // ------------------------------------------------------------------

    function test_FirstDeposit_Symmetric_AtPeg() public {
        // Bootstrap: first depositor sets the initial ratio. (1, 1)
        // mints 2*sqrt(1*1) = 2 QSD.
        vm.prank(alice);
        (uint256 minted, uint256 iqrlIn) = qsd.deposit{value: ONE}(ONE, 0);

        assertEq(minted, 2 * ONE, "qsd minted");
        assertEq(iqrlIn, ONE);
        assertEq(qsd.balanceOf(alice), 2 * ONE);
        assertEq(qsd.poolQRL(), ONE);
        assertEq(qsd.poolIQRL(), ONE);
        assertTrue(qsd.checkInvariants());
        assertEq(2 * Math.sqrt(qsd.poolQRL() * qsd.poolIQRL()), qsd.totalSupply());
    }

    function test_Deposit_PullsIqrlAtPoolRatio() public {
        // Bootstrap pool at 2:1.
        vm.prank(alice);
        qsd.deposit{value: 2 * ONE}(ONE, 0); // pool (2, 1)

        // Bob's subsequent deposit must match the 2:1 ratio. Sending
        // 1 QRL pulls 0.5 iQRL.
        vm.prank(bob);
        (uint256 minted, uint256 iqrlIn) = qsd.deposit{value: ONE}(ONE, 0);

        assertEq(iqrlIn, ONE / 2, "iqrl pulled at pool ratio");
        // Pool grew by factor (1/2 = qrlIn/poolQRL), so supply grows
        // by the same factor: minted = totalSupplyBefore * 1/2.
        // Pool was at (2, 1) → totalSupply ≈ 2*sqrt(2). Half of that
        // is sqrt(2) ≈ 1.414e18.
        assertApproxEqRel(minted, 1414213562373095049, 1e15);
        assertEq(qsd.poolQRL(), 3 * ONE);
        assertEq(qsd.poolIQRL(), ONE + ONE / 2);
        assertTrue(qsd.checkInvariants());
    }

    function test_Deposit_AsymmetricRejected_PostBootstrap() public {
        // Bootstrap to (1, 1).
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0);

        // Bob tries to deposit 1 QRL but caps iQRL at 0. The contract
        // tries to pull 1 iQRL (matching pool ratio); slippage cap
        // exceeded, revert. There is no path to deposit a single-
        // sided QRL contribution.
        vm.prank(bob);
        vm.expectRevert(); // SlippageExceeded(1e18, 0)
        qsd.deposit{value: ONE}(0, 0);
    }

function test_Deposit_SlippageRevert_OnQsdOut() public {
        vm.prank(alice);
        vm.expectRevert(); // SlippageExceeded on minQsdOut
        qsd.deposit{value: ONE}(ONE, 3 * ONE); // bootstrap mints 2, demand 3
    }

    function test_Deposit_RevertsOnZeroQrl() public {
        vm.prank(alice);
        vm.expectRevert(QSD.ZeroAmount.selector);
        qsd.deposit{value: 0}(0, 0);
    }

    function test_Deposit_RevertsOnZeroIqrlAtBootstrap() public {
        vm.prank(alice);
        vm.expectRevert(QSD.ZeroAmount.selector);
        qsd.deposit{value: ONE}(0, 0); // bootstrap with iqrl=0 disallowed
    }

    // ------------------------------------------------------------------
    // redeem
    // ------------------------------------------------------------------

    function test_Redeem_AtPeg_Returns_DollarValue() public {
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0); // 2 QSD, pool (1, 1)

        // Redeem 1 QSD at p=$1: V = 1*1 + 1/1 = 2. slice = 1/2 of pool.
        // Returns 0.5 QRL + 0.5 iQRL = $1.
        uint256 balBefore = alice.balance;
        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(ONE);
        assertEq(qrlOut, ONE / 2);
        assertEq(iqrlOut, ONE / 2);
        // Native QRL was actually delivered.
        assertEq(alice.balance, balBefore + ONE / 2);
        assertTrue(qsd.checkInvariants());
    }

    // Redemption is pro-rata over totalSupply, oracle-independent.
    // For a fresh symmetric pool (1 QRL, 1 iQRL, 2 QSD supply),
    // burning 1 QSD yields exactly half of each reserve. The oracle
    // price doesn't enter the math; we set one anyway to confirm
    // it has no effect.
    function test_Redeem_PaysProRataSlice_OracleIndependent() public {
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0); // pool (1, 1), supply 2

        oracle.setPrice(2 * ONE); // arbitrary; should not affect payout

        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(ONE);

        // Pro-rata: 1 / 2 of each reserve.
        assertEq(qrlOut, ONE / 2);
        assertEq(iqrlOut, ONE / 2);
        assertTrue(qsd.checkInvariants());
    }

    function test_Redeem_PreservesPoolRatio() public {
        vm.prank(alice);
        qsd.deposit{value: 2 * ONE}(ONE, 0); // asymmetric to break ratio

        uint256 ratioBefore = (qsd.poolQRL() * 1e18) / qsd.poolIQRL();
        uint256 redeemAmount = qsd.balanceOf(alice) / 4;

        vm.prank(alice);
        qsd.redeem(redeemAmount);

        uint256 ratioAfter = (qsd.poolQRL() * 1e18) / qsd.poolIQRL();
        assertApproxEqRel(ratioAfter, ratioBefore, 1e15); // 0.1% tolerance
        assertTrue(qsd.checkInvariants());
    }

    // ------------------------------------------------------------------
    // swap
    // ------------------------------------------------------------------

    function test_Swap_KMonotone_QrlIn() public {
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);

        uint256 kBefore = qsd.poolQRL() * qsd.poolIQRL();

        vm.prank(bob);
        qsd.swapQrlForIqrl{value: 10 * ONE}(0);

        uint256 kAfter = qsd.poolQRL() * qsd.poolIQRL();
        assertGe(kAfter, kBefore, "k should not decrease");
        assertTrue(qsd.checkInvariants());
    }

    function test_Swap_KMonotone_IqrlIn() public {
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);

        uint256 kBefore = qsd.poolQRL() * qsd.poolIQRL();

        vm.prank(bob);
        qsd.swapIqrlForQrl(10 * ONE, 0);

        uint256 kAfter = qsd.poolQRL() * qsd.poolIQRL();
        assertGe(kAfter, kBefore);
        assertTrue(qsd.checkInvariants());
    }

    function test_Swap_NoOracleDependency() public {
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);

        oracle.setHealthy(false);

        vm.prank(bob);
        qsd.swapQrlForIqrl{value: 10 * ONE}(0);
        assertTrue(qsd.checkInvariants());
    }

    function test_Swap_IqrlIn_DeliversNativeQrl() public {
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);

        uint256 balBefore = bob.balance;
        vm.prank(bob);
        uint256 amountOut = qsd.swapIqrlForQrl(10 * ONE, 0);

        assertGt(amountOut, 0);
        assertEq(bob.balance, balBefore + amountOut);
    }

    // ------------------------------------------------------------------
    // oracle health
    // ------------------------------------------------------------------

    function test_Redeem_OracleUnhealthy_StillSucceeds() public {
        // Redemption is a pure pool-share claim; it has no oracle
        // dependency. Even if every validator goes silent, holders
        // can still exit at any time. This is the "no liveness
        // dependency on the oracle" property.
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0);

        oracle.setHealthy(false);

        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(ONE);
        assertEq(qrlOut, ONE / 2);
        assertEq(iqrlOut, ONE / 2);
        assertTrue(qsd.checkInvariants());
    }

    function test_OracleUnhealthy_DepositStillWorks() public {
        // Deposits derive iqrlIn and qsdMinted from pool state only;
        // fully oracle-independent.
        oracle.setHealthy(false);

        vm.prank(alice);
        (uint256 minted,) = qsd.deposit{value: ONE}(ONE, 0);
        assertEq(minted, 2 * ONE);
        assertTrue(qsd.checkInvariants());
    }

    function test_OracleUnhealthy_SwapStillWorks() public {
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);

        oracle.setHealthy(false);

        vm.prank(bob);
        uint256 out = qsd.swapQrlForIqrl{value: 10 * ONE}(0);
        assertGt(out, 0);
        assertTrue(qsd.checkInvariants());
    }

    function test_OracleUnhealthy_QuoteDepositStillWorks() public {
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0);

        oracle.setHealthy(false);

        (uint256 iqrlIn, uint256 qsdMinted) = qsd.quoteDeposit(ONE);
        assertGt(qsdMinted, 0);
        assertEq(iqrlIn, ONE); // 1:1 pool ratio
    }

    function test_Redeem_FloorAtDollarValue() public {
        // Pro-rata redemption pays >= $1 of USD value per QSD by the
        // solvency invariant V >= totalSupply (AM-GM). Equality only
        // at a balanced symmetric pool; any swap or asymmetric
        // deposit tilts the pool and pushes V > T, so redeemers
        // start receiving > $1 of value.
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);
        uint256 minted = qsd.balanceOf(alice);

        // Tilt the pool with a swap. swapQrlForIqrl preserves k and
        // grows V at p=$1 because the pool moves off marginal.
        vm.prank(bob);
        qsd.swapQrlForIqrl{value: 10 * ONE}(0);

        // Redeem half of alice's QSD and check the slice's USD value
        // at the peg (oracle still reports $1).
        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(minted / 2);

        // At p=$1, slice value = qrlOut + iqrlOut. Should be >= half
        // of minted (which represents the $1-per-QSD floor) and
        // strictly greater since the pool is now tilted.
        uint256 sliceValueAtPeg = qrlOut + iqrlOut;
        assertGe(sliceValueAtPeg, minted / 2, "below $1 floor per QSD");
        assertTrue(qsd.checkInvariants());
    }

    // ------------------------------------------------------------------
    // fuzz
    // ------------------------------------------------------------------

    function testFuzz_DepositRoundTrip_FloorsAtDollarValue(uint96 qrlIn, uint96 iqrlIn) public {
        uint256 qIn = bound(uint256(qrlIn), 1e6, 1e24);
        uint256 iIn = bound(uint256(iqrlIn), 1e6, 1e24);

        vm.prank(alice);
        (uint256 minted,) = qsd.deposit{value: qIn}(iIn, 0);

        assertGt(minted, 0);
        assertTrue(qsd.checkInvariants());

        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(minted);

        // Pro-rata redemption pays >= minted USD at any non-tilted
        // exit price. At p=$1 the slice value equals the full pool
        // value, which is >= totalSupply by solvency. So redeemers
        // never fall below the $1-per-QSD floor.
        uint256 valueAtPeg = qrlOut + iqrlOut;
        assertGe(valueAtPeg + 1, minted, "below $1 floor"); // +1 for floor-rounding
        assertTrue(qsd.checkInvariants());
    }

    function testFuzz_SwapMonotone(uint96 qrlSeed, uint96 iqrlSeed, uint96 swapAmount, bool qrlIn)
        public
    {
        uint256 qIn = bound(uint256(qrlSeed), 1e10, 1e24);
        uint256 iIn = bound(uint256(iqrlSeed), 1e10, 1e24);
        vm.prank(alice);
        qsd.deposit{value: qIn}(iIn, 0);

        uint256 reserveIn = qrlIn ? qsd.poolQRL() : qsd.poolIQRL();
        uint256 amt = bound(uint256(swapAmount), 1, reserveIn / 2);

        uint256 kBefore = qsd.poolQRL() * qsd.poolIQRL();

        vm.prank(bob);
        if (qrlIn) {
            qsd.swapQrlForIqrl{value: amt}(0);
        } else {
            qsd.swapIqrlForQrl(amt, 0);
        }

        uint256 kAfter = qsd.poolQRL() * qsd.poolIQRL();
        assertGe(kAfter, kBefore);
        assertTrue(qsd.checkInvariants());
    }

    function testFuzz_OraclePriceIrrelevantToRedeem(
        uint96 priceMultiplier
    ) public {
        // Two side-by-side QSD deployments redeeming the same supply
        // share against the same pool ratio should pay out identical
        // slices regardless of where the oracle says price is. This
        // is the "redeem has no oracle dependency" property.
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);
        uint256 minted = qsd.balanceOf(alice);

        uint256 newPrice = bound(uint256(priceMultiplier), ONE / 100, 100 * ONE);
        oracle.setPrice(newPrice);

        // Slice should equal exactly half of each reserve regardless
        // of newPrice (pool was symmetric, redeeming half of supply).
        uint256 expectedQrl = qsd.poolQRL() / 2;
        uint256 expectedIqrl = qsd.poolIQRL() / 2;

        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(minted / 2);

        assertEq(qrlOut, expectedQrl);
        assertEq(iqrlOut, expectedIqrl);
        assertTrue(qsd.checkInvariants());
    }
}
