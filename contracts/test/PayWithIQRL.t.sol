// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";

import {PayWithIQRL} from "../src/PayWithIQRL.sol";
import {MockERC20} from "./mocks/MockERC20.sol";

contract PayWithIQRLTest is Test {
    PayWithIQRL internal pm;
    MockERC20 internal iqrl;

    address internal alice = address(0xA11CE);
    address internal coinbase = address(0xC01);
    address internal SYSTEM_CALLER;

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        iqrl = new MockERC20("iQRL", "iQRL");
        pm = new PayWithIQRL(iqrl);

        SYSTEM_CALLER = pm.SYSTEM_CALLER();

        iqrl.mint(alice, 1e30);
        vm.prank(alice);
        iqrl.approve(address(pm), type(uint256).max);

        // Pin the block coinbase so transferred fees land somewhere
        // we can assert on.
        vm.coinbase(coinbase);
    }

    // ------------------------------------------------------------------
    // happy path
    // ------------------------------------------------------------------

    function test_EscrowAndSettle_FullFee_TipOnly() public {
        // Escrow 10 iQRL, settle entirely as tip (zero burn). Mirrors
        // a chain configured with baseFee = 0.
        vm.prank(SYSTEM_CALLER);
        pm.escrow(alice, 10 * ONE);
        assertEq(iqrl.balanceOf(address(pm)), 10 * ONE);

        vm.prank(SYSTEM_CALLER);
        pm.settle(alice, 10 * ONE, 0, 10 * ONE); // burn=0, tip=10
        assertEq(iqrl.balanceOf(coinbase), 10 * ONE);
        assertEq(iqrl.balanceOf(alice), 1e30 - 10 * ONE);
        assertEq(iqrl.balanceOf(address(pm)), 0);
    }

    function test_EscrowAndSettle_BurnAndTip() public {
        // Escrow 10 iQRL. 4 burn (mirrors baseFee), 3 tip, 3 refund.
        vm.prank(SYSTEM_CALLER);
        pm.escrow(alice, 10 * ONE);
        uint256 supplyBefore = iqrl.totalSupply();

        vm.prank(SYSTEM_CALLER);
        pm.settle(alice, 10 * ONE, 4 * ONE, 3 * ONE);

        // Coinbase gets the tip; the burn portion is destroyed.
        assertEq(iqrl.balanceOf(coinbase), 3 * ONE);
        // Alice spent 7 iQRL net (4 burned + 3 tipped).
        assertEq(iqrl.balanceOf(alice), 1e30 - 7 * ONE);
        // Paymaster fully drained.
        assertEq(iqrl.balanceOf(address(pm)), 0);
        // totalSupply dropped by exactly the burn amount.
        assertEq(iqrl.totalSupply(), supplyBefore - 4 * ONE);
    }

    function test_EscrowAndSettle_PureBurn() public {
        // Edge case: 100% base fee, no tip. Validator gets nothing,
        // entire fee is burned.
        vm.prank(SYSTEM_CALLER);
        pm.escrow(alice, 5 * ONE);
        uint256 supplyBefore = iqrl.totalSupply();

        vm.prank(SYSTEM_CALLER);
        pm.settle(alice, 5 * ONE, 5 * ONE, 0);

        assertEq(iqrl.balanceOf(coinbase), 0);
        assertEq(iqrl.balanceOf(alice), 1e30 - 5 * ONE);
        assertEq(iqrl.totalSupply(), supplyBefore - 5 * ONE);
    }

    function test_EscrowAndSettle_ZeroActualFee_FullRefund() public {
        vm.prank(SYSTEM_CALLER);
        pm.escrow(alice, 5 * ONE);
        uint256 supplyBefore = iqrl.totalSupply();

        // Edge: tx ran for zero gas (pre-execution failure caught
        // before any work). Both burn and tip are zero, full refund.
        vm.prank(SYSTEM_CALLER);
        pm.settle(alice, 5 * ONE, 0, 0);

        assertEq(iqrl.balanceOf(coinbase), 0);
        assertEq(iqrl.balanceOf(alice), 1e30);
        assertEq(iqrl.balanceOf(address(pm)), 0);
        assertEq(iqrl.totalSupply(), supplyBefore);
    }

    // ------------------------------------------------------------------
    // access control
    // ------------------------------------------------------------------

    function test_Escrow_RejectsNonSystemCaller() public {
        vm.prank(alice); // alice is the payer; she still can't call escrow
        vm.expectRevert(PayWithIQRL.NotSystemCaller.selector);
        pm.escrow(alice, ONE);
    }

    function test_Settle_RejectsNonSystemCaller() public {
        vm.prank(SYSTEM_CALLER);
        pm.escrow(alice, ONE);

        vm.prank(alice);
        vm.expectRevert(PayWithIQRL.NotSystemCaller.selector);
        pm.settle(alice, ONE, 0, ONE);
    }

    // ------------------------------------------------------------------
    // input validation
    // ------------------------------------------------------------------

    function test_Settle_RejectsActualGreaterThanMax() public {
        vm.prank(SYSTEM_CALLER);
        pm.escrow(alice, 5 * ONE);

        // burn + tip = 10 ONE > maxFee = 5 ONE → revert.
        vm.prank(SYSTEM_CALLER);
        vm.expectRevert(
            abi.encodeWithSelector(
                PayWithIQRL.InsufficientEscrow.selector,
                10 * ONE,
                5 * ONE
            )
        );
        pm.settle(alice, 5 * ONE, 4 * ONE, 6 * ONE);
    }

    function test_Escrow_RevertsIfPayerLacksAllowance() public {
        address bob = address(0xB0B);
        iqrl.mint(bob, ONE);
        // bob did NOT approve

        vm.prank(SYSTEM_CALLER);
        vm.expectRevert(); // SafeERC20FailedOperation or similar
        pm.escrow(bob, ONE);
    }

    function test_Escrow_RevertsIfPayerLacksBalance() public {
        address bob = address(0xB0B);
        // bob approves but has no balance
        vm.prank(bob);
        iqrl.approve(address(pm), type(uint256).max);

        vm.prank(SYSTEM_CALLER);
        vm.expectRevert();
        pm.escrow(bob, ONE);
    }

    // ------------------------------------------------------------------
    // grief resistance
    // ------------------------------------------------------------------

    function test_RandomCallerCannotDrainContract() public {
        // System escrows real iQRL into the contract...
        vm.prank(SYSTEM_CALLER);
        pm.escrow(alice, 10 * ONE);
        assertEq(iqrl.balanceOf(address(pm)), 10 * ONE);

        // ...and a random caller tries to siphon it via settle.
        address mallory = address(0xBAD);
        vm.prank(mallory);
        vm.expectRevert(PayWithIQRL.NotSystemCaller.selector);
        pm.settle(mallory, 10 * ONE, 0, 10 * ONE);

        // Contract balance untouched.
        assertEq(iqrl.balanceOf(address(pm)), 10 * ONE);
        assertEq(iqrl.balanceOf(mallory), 0);
    }
}
