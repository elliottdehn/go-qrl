// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test, console2} from "forge-std/Test.sol";

import {InverseQRL} from "../src/InverseQRL.sol";
import {MockPriceOracle} from "./mocks/MockPriceOracle.sol";
import {MockQsdPool} from "./mocks/MockQsdPool.sol";

contract InverseQRLTest is Test {
    InverseQRL internal iqrl;
    MockPriceOracle internal oracle;
    MockQsdPool internal pool;

    address internal alice = address(0xA11CE);
    address internal bob = address(0xB0B);

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        oracle = new MockPriceOracle(ONE); // $1 per QRL
        pool = new MockQsdPool();
        iqrl = new InverseQRL(oracle, pool);

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
        // p = $1: 1 iQRL costs 1 QRL exactly (no mint fee).
        uint256 spent = _mintExact(alice, ONE);

        assertEq(iqrl.balanceOf(alice), ONE);
        assertEq(spent, ONE);
        assertEq(iqrl.qrlPermanentlyDestroyed(), spent);
    }

    function test_Mint_AtHighPrice_CostsLessQrl() public {
        // p = $2: 1 iQRL = $0.50 = 0.25 QRL.
        oracle.setPrice(2 * ONE);
        uint256 spent = _mintExact(alice, ONE);
        assertEq(spent, ONE / 4);
    }

    function test_Mint_AtLowPrice_CostsMoreQrl() public {
        // p = $0.50: 1 iQRL = $2 = 4 QRL.
        oracle.setPrice(ONE / 2);
        uint256 spent = _mintExact(alice, ONE);
        assertEq(spent, 4 * ONE);
    }

    function test_Mint_RefundsSurplus() public {
        // Send 2 QRL when the cost is only 1 QRL — the contract should
        // refund the surplus.
        uint256 balBefore = alice.balance;
        vm.prank(alice);
        uint256 spent = iqrl.mint{value: 2 * ONE}(ONE);

        assertEq(spent, ONE);
        assertEq(alice.balance, balBefore - ONE);
        assertEq(address(iqrl).balance, ONE);
    }

    function test_Mint_SlippageProtection() public {
        // At p=$1, 1 iQRL mint costs exactly 1 QRL. Send only 0.5 QRL: revert.
        vm.prank(alice);
        vm.expectRevert(
            abi.encodeWithSelector(
                InverseQRL.InsufficientPayment.selector,
                ONE,
                ONE / 2
            )
        );
        iqrl.mint{value: ONE / 2}(ONE);
    }

    function test_Mint_RevertsWhenBothPriceSourcesUnavailable() public {
        // Oracle unhealthy AND no TWAP snapshot established yet:
        // mint has no price source and must revert with
        // PriceUnavailable.
        oracle.setHealthy(false);
        vm.prank(alice);
        vm.expectRevert(InverseQRL.PriceUnavailable.selector);
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
        assertEq(quotedFee, 0);
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

    function test_OracleUnhealthy_NoTwap_BlocksMint() public {
        // Oracle unhealthy AND no TWAP snapshot: both price sources
        // unavailable, mint reverts with PriceUnavailable.
        oracle.setHealthy(false);
        vm.prank(alice);
        vm.expectRevert(InverseQRL.PriceUnavailable.selector);
        iqrl.mint{value: 2 * ONE}(ONE);
    }

    function test_OracleUnhealthy_NoTwap_BlocksQuoteMint() public {
        oracle.setHealthy(false);
        vm.expectRevert(InverseQRL.PriceUnavailable.selector);
        iqrl.quoteMint(ONE);
    }

    function test_OracleUnhealthy_TwapAvailable_MintSucceeds() public {
        // Bootstrap a TWAP snapshot via a successful mint while
        // oracle is healthy. Initial spot price = $1.
        pool.setSpotPrice(ONE);
        _mintExact(alice, ONE / 10);

        // Wait past MIN_TWAP_WINDOW.
        vm.warp(block.timestamp + iqrl.MIN_TWAP_WINDOW() + 1);

        // Now break the oracle. Pool TWAP should carry mint.
        oracle.setHealthy(false);
        uint256 spent = _mintExact(alice, ONE / 10);
        assertGt(spent, 0, "TWAP-priced mint should succeed when oracle is down");
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
        vm.expectRevert(InverseQRL.PriceUnavailable.selector);
        iqrl.mint{value: 2 * ONE}(ONE);

        oracle.setHealthy(true);
        uint256 spent = _mintExact(alice, ONE);
        assertGt(spent, 0);
        assertEq(iqrl.balanceOf(alice), ONE);
    }

    // ------------------------------------------------------------------
    // mint-price source selection (min(oracle, TWAP))
    // ------------------------------------------------------------------

    function test_Mint_PicksOracleWhenLower() public {
        // Bootstrap TWAP at p=$2 (more iQRL per QRL → cheaper mint),
        // oracle stays at p=$1 (more expensive). min should pick
        // oracle, charging the higher cost.
        pool.setSpotPrice(2 * ONE);
        _mintExact(alice, ONE / 10);
        vm.warp(block.timestamp + iqrl.MIN_TWAP_WINDOW() + 1);

        // Oracle still at $1; TWAP averages toward $2.
        // Cost at p=$1 is iqrlAmount/p^2 = iqrlAmount.
        // Cost at p=$2 is iqrlAmount/4.
        // Oracle (lower price) → higher cost → that's what we charge.
        (uint256 cost,) = iqrl.quoteMint(ONE / 10);
        // Cost should be near (ONE / 10), NOT (ONE / 40).
        assertGt(cost, ONE / 12, "should reflect oracle p=$1, not pool p=$2");
    }

    function test_Mint_PicksTwapWhenLower() public {
        // Symmetric: bootstrap TWAP at p=$0.5 (more expensive mint),
        // oracle at $1. min picks pool (lower price → higher cost).
        pool.setSpotPrice(ONE / 2);
        _mintExact(alice, ONE / 10);
        vm.warp(block.timestamp + iqrl.MIN_TWAP_WINDOW() + 1);

        // At p=$0.5, cost = iqrlAmount / 0.25 = 4 * iqrlAmount.
        // At p=$1.0, cost = iqrlAmount.
        // The TWAP path charges much more.
        (uint256 cost,) = iqrl.quoteMint(ONE / 10);
        assertGt(cost, 3 * ONE / 10, "should reflect pool p=$0.5, not oracle p=$1");
    }

    function test_Mint_TwapWindowTooShort_FallsBackToOracle() public {
        // Bootstrap TWAP at p=$0.5 with snapshot, but mint again
        // before MIN_TWAP_WINDOW elapses. TWAP must be ignored.
        pool.setSpotPrice(ONE / 2);
        _mintExact(alice, ONE / 10);
        vm.warp(block.timestamp + 1); // way under MIN_TWAP_WINDOW

        // TWAP unavailable → oracle alone (p=$1) → cost ≈ iqrlAmount.
        (uint256 cost,) = iqrl.quoteMint(ONE / 10);
        // Cost should be near (ONE / 10), NOT 4x that.
        assertLt(cost, 2 * ONE / 10, "TWAP under MIN_TWAP_WINDOW should be ignored");
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
