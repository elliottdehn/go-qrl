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
        // Symmetric at p=$1: deposit (1, 1), expect 2 QSD minted.
        vm.prank(alice);
        uint256 minted = qsd.deposit{value: ONE}(ONE, 0);

        assertEq(minted, 2 * ONE, "qsd minted");
        assertEq(qsd.balanceOf(alice), 2 * ONE);
        assertEq(qsd.poolQRL(), ONE);
        assertEq(qsd.poolIQRL(), ONE);
        assertTrue(qsd.checkInvariants());
        assertEq(2 * Math.sqrt(qsd.poolQRL() * qsd.poolIQRL()), qsd.totalSupply());
    }

    function test_Deposit_Asymmetric_MintsLessThanUsdValue() public {
        // Empty pool. Single-sided deposit: stays at k=0, qsdMinted == 0
        // → ZeroAmount revert.
        vm.prank(alice);
        vm.expectRevert(QSD.ZeroAmount.selector);
        qsd.deposit{value: ONE}(0, 0);

        // After a balanced first deposit, asymmetric deposits succeed
        // but mint less than the asymmetric leg's USD value.
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0);

        // Deposit only QRL: at the pool's marginal price the user is
        // pushing the pool away from balance, so they get less QSD
        // than the deposit's USD value at oracle price.
        uint256 supplyBefore = qsd.totalSupply();
        vm.prank(bob);
        uint256 minted = qsd.deposit{value: ONE}(0, 0);
        uint256 oracleUsdValue = ONE; // 1 QRL at p=$1 = $1

        assertGt(minted, 0);
        assertLt(minted, oracleUsdValue, "asymmetric mints less than USD");
        assertEq(qsd.totalSupply(), supplyBefore + minted);
        assertTrue(qsd.checkInvariants());
    }

    function test_Deposit_SlippageRevert() public {
        vm.prank(alice);
        vm.expectRevert(); // SlippageExceeded
        qsd.deposit{value: ONE}(ONE, 3 * ONE); // expect 2, demand 3
    }

    function test_Deposit_RevertsOnEmptyEmpty() public {
        vm.prank(alice);
        vm.expectRevert(QSD.ZeroAmount.selector);
        qsd.deposit{value: 0}(0, 0);
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

    function test_Redeem_OffPeg_StillReturns_DollarValue() public {
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0); // pool (1, 1), supply 2

        // Move oracle to $2. Pool USD value at $2:
        //   V = 1*2 + 1/2 = 2.5
        // Redeem 1 QSD: slice = 1/V of pool. USD value of slice = 1.
        oracle.setPrice(2 * ONE);

        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(ONE);

        // USD value at p=$2:
        uint256 sliceValue = (qrlOut * 2 * ONE) / ONE + (iqrlOut * ONE) / (2 * ONE);
        assertApproxEqAbs(sliceValue, ONE, 1);
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

    function test_OracleUnhealthy_BlocksRedeem() public {
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0);

        oracle.setHealthy(false);

        vm.prank(alice);
        vm.expectRevert(QSD.OracleUnhealthy.selector);
        qsd.redeem(ONE);
    }

    function test_OracleUnhealthy_BlocksQuoteSymmetricDeposit() public {
        oracle.setHealthy(false);

        vm.expectRevert(QSD.OracleUnhealthy.selector);
        qsd.quoteSymmetricDeposit(ONE);
    }

    function test_OracleUnhealthy_DepositStillWorks() public {
        // Deposits use the sqrt-k LP formula — fully oracle-independent.
        oracle.setHealthy(false);

        vm.prank(alice);
        uint256 minted = qsd.deposit{value: ONE}(ONE, 0);
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

        uint256 quote = qsd.quoteDeposit(ONE, ONE);
        assertGt(quote, 0);
    }

    function test_OracleRecovers_RedeemAgainSucceeds() public {
        vm.prank(alice);
        qsd.deposit{value: ONE}(ONE, 0);

        oracle.setHealthy(false);
        vm.prank(alice);
        vm.expectRevert(QSD.OracleUnhealthy.selector);
        qsd.redeem(ONE / 2);

        oracle.setHealthy(true);
        vm.prank(alice);
        (uint256 qrlOut,) = qsd.redeem(ONE / 2);
        assertGt(qrlOut, 0);
    }

    // ------------------------------------------------------------------
    // fuzz
    // ------------------------------------------------------------------

    function testFuzz_DepositRoundTrip(uint96 qrlIn, uint96 iqrlIn) public {
        uint256 qIn = bound(uint256(qrlIn), 1e6, 1e24);
        uint256 iIn = bound(uint256(iqrlIn), 1e6, 1e24);

        vm.prank(alice);
        uint256 minted = qsd.deposit{value: qIn}(iIn, 0);

        assertGt(minted, 0);
        assertTrue(qsd.checkInvariants());

        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(minted);

        uint256 valueAtPeg = (qrlOut * ONE) / ONE + (iqrlOut * ONE) / ONE;
        assertApproxEqRel(valueAtPeg, minted, 1e15);
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

    function testFuzz_OraclePriceMoves_PreservesRedemptionDollarValue(
        uint96 priceMultiplier
    ) public {
        vm.prank(alice);
        qsd.deposit{value: 100 * ONE}(100 * ONE, 0);
        uint256 minted = qsd.balanceOf(alice);

        uint256 newPrice = bound(uint256(priceMultiplier), ONE / 100, 100 * ONE);
        oracle.setPrice(newPrice);

        vm.prank(alice);
        (uint256 qrlOut, uint256 iqrlOut) = qsd.redeem(minted);

        uint256 sliceValue = (qrlOut * newPrice) / ONE + (iqrlOut * ONE) / newPrice;

        assertApproxEqRel(sliceValue, minted, 1e15); // 0.1%
        assertTrue(qsd.checkInvariants());
    }
}
