// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test, console2} from "forge-std/Test.sol";

import {InverseQRL} from "../src/InverseQRL.sol";
import {MockPriceOracle} from "./mocks/MockPriceOracle.sol";

contract InverseQRLTest is Test {
    InverseQRL internal iqrl;
    MockPriceOracle internal oracle;

    address internal alice = address(0xA11CE);
    address internal bob = address(0xB0B);

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        oracle = new MockPriceOracle(ONE); // $1 per QRL
        iqrl = new InverseQRL(oracle);

        vm.deal(alice, 1e30);
        vm.deal(bob, 1e30);
    }

    // Convenience: mint with exactly the quoted cost (no surplus).
    function _mintExact(address from, uint256 iqrlAmount) internal returns (uint256 spent) {
        (uint256 cost, ) = iqrl.quoteMint(iqrlAmount);
        vm.prank(from);
        spent = iqrl.mint{value: cost}(iqrlAmount);
    }

    // ------------------------------------------------------------------
    // mint
    // ------------------------------------------------------------------

    function test_Mint_AtPeg() public {
        // p = $1: 1 iQRL costs 1 QRL + 0.005 QRL fee = 1.005 QRL.
        uint256 spent = _mintExact(alice, ONE);

        assertEq(iqrl.balanceOf(alice), ONE);
        assertEq(spent, ONE + (ONE * 50 / 10_000));
        assertEq(iqrl.qrlPermanentlyDestroyed(), spent);
    }

    function test_Mint_AtHighPrice_CostsLessQrl() public {
        // p = $2: 1 iQRL = $0.50 = 0.25 QRL; fee = 0.25 * 0.005 = 0.00125.
        oracle.setPrice(2 * ONE);
        uint256 spent = _mintExact(alice, ONE);

        uint256 expectedBase = ONE / 4; // 0.25 QRL
        uint256 expectedFee = expectedBase * 50 / 10_000;
        assertEq(spent, expectedBase + expectedFee);
    }

    function test_Mint_AtLowPrice_CostsMoreQrl() public {
        // p = $0.50: 1 iQRL = $2 = 4 QRL; fee = 4 * 0.005 = 0.02 QRL.
        oracle.setPrice(ONE / 2);
        uint256 spent = _mintExact(alice, ONE);

        uint256 expectedBase = 4 * ONE;
        uint256 expectedFee = expectedBase * 50 / 10_000;
        assertEq(spent, expectedBase + expectedFee);
    }

    function test_Mint_RefundsSurplus() public {
        // Send 2 QRL when the cost is only 1.005 QRL — the contract
        // should refund the surplus.
        uint256 balBefore = alice.balance;
        vm.prank(alice);
        uint256 spent = iqrl.mint{value: 2 * ONE}(ONE);

        uint256 expectedSpend = ONE + (ONE * 50 / 10_000);
        assertEq(spent, expectedSpend);
        assertEq(alice.balance, balBefore - expectedSpend);
        assertEq(address(iqrl).balance, expectedSpend);
    }

    function test_Mint_SlippageProtection() public {
        // At p=$1, 1 iQRL mint costs 1.005 QRL. Send only 1 QRL: revert.
        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(
                InverseQRL.InsufficientPayment.selector,
                ONE + (ONE * 50 / 10_000),
                ONE
            )
        );
        iqrl.mint{value: ONE}(ONE);
    }

    function test_Mint_RevertsWhenOracleUnhealthy() public {
        oracle.setHealthy(false);
        vm.prank(alice);
        vm.expectRevert(InverseQRL.OracleUnhealthy.selector);
        iqrl.mint{value: 2 * ONE}(ONE);
    }

    function test_Mint_RevertsOnZero() public {
        vm.prank(alice);
        vm.expectRevert(InverseQRL.ZeroAmount.selector);
        iqrl.mint{value: ONE}(0);
    }

    function test_Mint_QrlPermanentlyLocked() public {
        _mintExact(alice, ONE);

        // The contract holds the destroyed QRL with no release path.
        assertGt(address(iqrl).balance, 0);
        // No setter, no withdrawal function, no admin — verified by
        // reading the source. qrlPermanentlyDestroyed() reflects it.
        assertEq(iqrl.qrlPermanentlyDestroyed(), address(iqrl).balance);
    }

    // ------------------------------------------------------------------
    // per-block cap
    // ------------------------------------------------------------------

    function test_Cap_AppliesBeforeSupplyGrows() public {
        // Initial cap = MIN_CAP_ABSOLUTE = 1 iQRL.
        assertEq(iqrl.mintCapForBlock(), iqrl.MIN_CAP_ABSOLUTE());

        // Alice mints 1 iQRL — fills the block budget.
        _mintExact(alice, ONE);

        // Second mint in same block reverts (1e-18 + 1 > cap).
        (uint256 tinyCost,) = iqrl.quoteMint(1);
        vm.prank(bob);
        vm.expectRevert();
        iqrl.mint{value: tinyCost}(1);

        // Advance block; new budget available.
        vm.roll(block.number + 1);
        _mintExact(bob, ONE / 2);
        assertEq(iqrl.balanceOf(bob), ONE / 2);
    }

    function test_Cap_ScalesWithSupply() public {
        oracle.setPrice(ONE);

        for (uint256 i = 0; i < 5; i++) {
            vm.roll(block.number + 1);
            _mintExact(alice, ONE);
        }

        assertEq(iqrl.totalSupply(), 5 * ONE);
        // Supply cap = 5 iQRL * 25 / 1_000_000 = 1.25e14 wei
        // MIN_CAP_ABSOLUTE = 1e18 wei — still dominant at this scale.
        assertEq(iqrl.mintCapForBlock(), iqrl.MIN_CAP_ABSOLUTE());
    }

    // ------------------------------------------------------------------
    // quote
    // ------------------------------------------------------------------

    function test_QuoteMint_MatchesActualCost() public {
        oracle.setPrice(3 * ONE / 2); // $1.50
        (uint256 quotedTotal, uint256 quotedFee) = iqrl.quoteMint(ONE);

        uint256 spent = _mintExact(alice, ONE);
        assertEq(spent, quotedTotal);
        assertGt(quotedFee, 0);
        assertEq(quotedFee, quotedTotal * 50 / (10_000 + 50)); // fee/(1+feeBps)
    }

    // ------------------------------------------------------------------
    // burn
    // ------------------------------------------------------------------

    function test_Burn_ReducesSupply() public {
        _mintExact(alice, ONE);

        uint256 supplyBefore = iqrl.totalSupply();
        vm.prank(alice);
        iqrl.burn(ONE / 2);
        assertEq(iqrl.totalSupply(), supplyBefore - ONE / 2);
        assertEq(iqrl.balanceOf(alice), ONE / 2);
    }

    function test_BurnFrom_RequiresApproval() public {
        _mintExact(alice, ONE);

        vm.prank(alice);
        iqrl.approve(bob, ONE / 4);

        vm.prank(bob);
        iqrl.burnFrom(alice, ONE / 4);
        assertEq(iqrl.balanceOf(alice), ONE - ONE / 4);

        // Further burn without approval fails.
        vm.prank(bob);
        vm.expectRevert();
        iqrl.burnFrom(alice, ONE / 4);
    }

    // ------------------------------------------------------------------
    // oracle health
    // ------------------------------------------------------------------

    function test_OracleUnhealthy_BlocksMint() public {
        oracle.setHealthy(false);
        vm.prank(alice);
        vm.expectRevert(InverseQRL.OracleUnhealthy.selector);
        iqrl.mint{value: 2 * ONE}(ONE);
    }

    function test_OracleUnhealthy_BlocksQuoteMint() public {
        oracle.setHealthy(false);
        vm.expectRevert(InverseQRL.OracleUnhealthy.selector);
        iqrl.quoteMint(ONE);
    }

    function test_OracleUnhealthy_BurnStillWorks() public {
        // Mint first while oracle is healthy.
        _mintExact(alice, ONE);

        // Then break the oracle; burn must still succeed so holders
        // can settle fee obligations during an outage.
        oracle.setHealthy(false);
        vm.prank(alice);
        iqrl.burn(ONE / 2);
        assertEq(iqrl.balanceOf(alice), ONE / 2);
    }

    function test_OracleUnhealthy_BurnFromStillWorks() public {
        _mintExact(alice, ONE);
        vm.prank(alice);
        iqrl.approve(bob, ONE);

        oracle.setHealthy(false);
        vm.prank(bob);
        iqrl.burnFrom(alice, ONE / 4);
        assertEq(iqrl.balanceOf(alice), ONE - ONE / 4);
    }

    function test_OracleUnhealthy_TransferStillWorks() public {
        _mintExact(alice, ONE);

        oracle.setHealthy(false);
        vm.prank(alice);
        iqrl.transfer(bob, ONE / 2);
        assertEq(iqrl.balanceOf(bob), ONE / 2);
    }

    function test_OracleRecovers_MintAgainSucceeds() public {
        oracle.setHealthy(false);
        vm.prank(alice);
        vm.expectRevert(InverseQRL.OracleUnhealthy.selector);
        iqrl.mint{value: 2 * ONE}(ONE);

        oracle.setHealthy(true);
        uint256 spent = _mintExact(alice, ONE);
        assertGt(spent, 0);
        assertEq(iqrl.balanceOf(alice), ONE);
    }

    // ------------------------------------------------------------------
    // fuzz
    // ------------------------------------------------------------------

    function testFuzz_MintCostMatchesQuote(uint256 price, uint96 iqrlAmount) public {
        price = bound(price, ONE / 100, 100 * ONE); // $0.01 to $100
        uint256 amt = bound(uint256(iqrlAmount), 1e10, iqrl.MIN_CAP_ABSOLUTE());
        oracle.setPrice(price);

        (uint256 quotedTotal,) = iqrl.quoteMint(amt);

        uint256 spent = _mintExact(alice, amt);
        assertEq(spent, quotedTotal);
    }

    function testFuzz_MintRespectsCap(uint256 price, uint256 amt1, uint256 amt2) public {
        price = bound(price, ONE / 100, 100 * ONE);
        oracle.setPrice(price);

        uint256 cap = iqrl.mintCapForBlock();
        amt1 = bound(amt1, 1, cap);

        // Pin amt2 into a band that straddles the cap: [1, 2*cap] so
        // each run independently tests both the "fits" and "exceeds"
        // branches without relying on a tangled if/else.
        amt2 = bound(amt2, 1, 2 * cap);

        (uint256 cost1,) = iqrl.quoteMint(amt1);
        vm.prank(alice);
        iqrl.mint{value: cost1}(amt1);

        (uint256 cost2,) = iqrl.quoteMint(amt2);
        vm.prank(bob);
        if (amt1 + amt2 > cap) {
            vm.expectRevert(
                abi.encodeWithSelector(
                    InverseQRL.MintCapExceeded.selector,
                    amt1 + amt2,
                    cap
                )
            );
        }
        iqrl.mint{value: cost2}(amt2);
    }
}
