// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {YieldQSDDesk} from "../src/YieldQSDDesk.sol";
import {MockERC20} from "./mocks/MockERC20.sol";

contract YieldQSDDeskTest is Test {
    YieldQSDDesk public desk;
    MockERC20    public yqsd;

    address public alice = address(0xA11CE);
    address public bob   = address(0xB0B);
    address public carol = address(0xCAA01);

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        yqsd = new MockERC20("Yield QSD", "yQSD");
        desk = new YieldQSDDesk(yqsd);

        // Seed yQSD balances.
        yqsd.mint(alice, 1_000 * ONE);
        yqsd.mint(bob,   1_000 * ONE);
        yqsd.mint(carol, 1_000 * ONE);

        // Approvals: makers approve desk to pull yQSD on postSell;
        // takers approve desk on take of BuyYQSD quotes.
        vm.prank(alice); yqsd.approve(address(desk), type(uint256).max);
        vm.prank(bob);   yqsd.approve(address(desk), type(uint256).max);
        vm.prank(carol); yqsd.approve(address(desk), type(uint256).max);

        // Native QRL.
        vm.deal(alice, 100 * ONE);
        vm.deal(bob,   100 * ONE);
        vm.deal(carol, 100 * ONE);
    }

    function _futureExpiry() internal view returns (uint64) {
        return uint64(block.timestamp + 1 hours);
    }

    // ------------------------------------------------------------------
    // postSell / postBuy basic mechanics
    // ------------------------------------------------------------------

    function test_PostSell_LocksYqsd() public {
        uint256 yqsdBefore = yqsd.balanceOf(alice);
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);

        assertEq(id, 1, "first quote id is 1");
        assertEq(yqsd.balanceOf(alice), yqsdBefore - 10 * ONE, "yQSD pulled from alice");
        assertEq(yqsd.balanceOf(address(desk)), 10 * ONE, "desk holds the yQSD");
        assertEq(desk.openQuoteCount(), 1);
    }

    function test_PostBuy_LocksQrl() public {
        uint256 aliceBefore = alice.balance;
        vm.prank(alice);
        uint256 id = desk.postBuy{value: 12 * ONE}(10 * ONE, _futureExpiry(), false);

        assertEq(id, 1);
        assertEq(alice.balance, aliceBefore - 12 * ONE, "QRL escrowed from alice");
        assertEq(address(desk).balance, 12 * ONE, "desk holds the QRL");
        assertEq(desk.openQuoteCount(), 1);
    }

    function test_PostSell_RevertsOnZero() public {
        vm.prank(alice);
        vm.expectRevert(YieldQSDDesk.ZeroAmount.selector);
        desk.postSell(0, 12 * ONE, _futureExpiry(), false);
        vm.prank(alice);
        vm.expectRevert(YieldQSDDesk.ZeroAmount.selector);
        desk.postSell(10 * ONE, 0, _futureExpiry(), false);
    }

    function test_PostSell_RevertsOnPastExpiry() public {
        vm.warp(1000);
        vm.prank(alice);
        vm.expectRevert(YieldQSDDesk.InvalidExpiry.selector);
        desk.postSell(10 * ONE, 12 * ONE, uint64(500), false);
    }

    // ------------------------------------------------------------------
    // take: full fill
    // ------------------------------------------------------------------

    function test_Take_FullFill_SellSide() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);

        uint256 aliceQrlBefore = alice.balance;
        uint256 bobYqsdBefore  = yqsd.balanceOf(bob);

        vm.prank(bob);
        desk.take{value: 12 * ONE}(id, 10 * ONE);

        assertEq(yqsd.balanceOf(bob) - bobYqsdBefore, 10 * ONE, "bob received yQSD");
        assertEq(alice.balance - aliceQrlBefore, 12 * ONE, "alice received QRL");
        assertEq(desk.openQuoteCount(), 0, "quote closed after full fill");

        YieldQSDDesk.Quote memory q = desk.getQuote(id);
        assertFalse(q.open);
        assertEq(q.yqsdRemaining, 0);
        assertEq(q.qrlRemaining, 0);
    }

    function test_Take_FullFill_BuySide() public {
        // Alice posts BuyYQSD: she offers 12 QRL for 10 yQSD.
        vm.prank(alice);
        uint256 id = desk.postBuy{value: 12 * ONE}(10 * ONE, _futureExpiry(), false);

        uint256 aliceYqsdBefore = yqsd.balanceOf(alice);
        uint256 bobQrlBefore    = bob.balance;

        // Bob fills: delivers 10 yQSD, receives 12 QRL.
        vm.prank(bob);
        desk.take{value: 0}(id, 10 * ONE);

        assertEq(yqsd.balanceOf(alice) - aliceYqsdBefore, 10 * ONE, "alice received yQSD");
        assertEq(bob.balance - bobQrlBefore, 12 * ONE, "bob received QRL");
        assertEq(desk.openQuoteCount(), 0);
    }

    function test_Take_FullFill_RevertsWrongMsgValue_SellSide() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);

        vm.prank(bob);
        vm.expectRevert(
            abi.encodeWithSelector(YieldQSDDesk.InvalidValue.selector, 12 * ONE, 11 * ONE)
        );
        desk.take{value: 11 * ONE}(id, 10 * ONE);
    }

    function test_Take_FullFill_RevertsWrongMsgValue_BuySide() public {
        vm.prank(alice);
        uint256 id = desk.postBuy{value: 12 * ONE}(10 * ONE, _futureExpiry(), false);

        vm.prank(bob);
        vm.expectRevert(
            abi.encodeWithSelector(YieldQSDDesk.InvalidValue.selector, 0, 5 * ONE)
        );
        desk.take{value: 5 * ONE}(id, 10 * ONE);
    }

    // ------------------------------------------------------------------
    // take: partial fill
    // ------------------------------------------------------------------

    function test_Take_PartialFill_DisallowedByDefault() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), /*allowPartial=*/false);

        vm.prank(bob);
        vm.expectRevert(YieldQSDDesk.PartialFillsDisabled.selector);
        desk.take{value: 4 * ONE}(id, 3 * ONE);
    }

    function test_Take_PartialFill_AllowedWhenEnabled() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), /*allowPartial=*/true);

        // Bob fills 3 yQSD; qrlPortion = 3 * 12 / 10 = 3.6 (exact in scaled).
        uint256 expectedQrl = 36 * ONE / 10;  // = 3.6 * ONE
        uint256 aliceQrlBefore = alice.balance;
        uint256 bobYqsdBefore  = yqsd.balanceOf(bob);

        vm.prank(bob);
        desk.take{value: expectedQrl}(id, 3 * ONE);

        assertEq(yqsd.balanceOf(bob) - bobYqsdBefore, 3 * ONE, "bob got 3 yQSD");
        assertEq(alice.balance - aliceQrlBefore, expectedQrl, "alice got 3.6 QRL");

        YieldQSDDesk.Quote memory q = desk.getQuote(id);
        assertTrue(q.open, "still open after partial");
        assertEq(q.yqsdRemaining, 7 * ONE);
        assertEq(q.qrlRemaining,  84 * ONE / 10);  // 8.4 ONE
        assertEq(desk.openQuoteCount(), 1);
    }

    function test_Take_PartialThenFull_SumsToInitial() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), true);

        // Carol partially takes 3 yQSD for 3.6 QRL.
        uint256 firstPortion = 36 * ONE / 10;
        vm.prank(carol);
        desk.take{value: firstPortion}(id, 3 * ONE);

        // Bob takes the remaining 7 yQSD; final-fill sweeps qrlRemaining = 8.4.
        uint256 secondPortion = 84 * ONE / 10;
        vm.prank(bob);
        desk.take{value: secondPortion}(id, 7 * ONE);

        // Total: 3.6 + 8.4 = 12 QRL for 3 + 7 = 10 yQSD = original ratio exactly.
        YieldQSDDesk.Quote memory q = desk.getQuote(id);
        assertFalse(q.open);
        assertEq(q.yqsdRemaining, 0);
        assertEq(q.qrlRemaining,  0);
        assertEq(desk.openQuoteCount(), 0);
    }

    function test_Take_PartialFill_BuySide() public {
        // Alice offers 12 QRL for 10 yQSD, partial allowed.
        vm.prank(alice);
        uint256 id = desk.postBuy{value: 12 * ONE}(10 * ONE, _futureExpiry(), true);

        // Bob delivers 5 yQSD; qrlPortion = 5 * 12 / 10 = 6 QRL (exact).
        uint256 bobQrlBefore = bob.balance;
        vm.prank(bob);
        desk.take(id, 5 * ONE);

        assertEq(bob.balance - bobQrlBefore, 6 * ONE, "bob received 6 QRL");
        assertEq(yqsd.balanceOf(alice), 1_000 * ONE + 5 * ONE, "alice got 5 yQSD");

        YieldQSDDesk.Quote memory q = desk.getQuote(id);
        assertTrue(q.open);
        assertEq(q.yqsdRemaining, 5 * ONE);
        assertEq(q.qrlRemaining,  6 * ONE);
    }

    // ------------------------------------------------------------------
    // take: error paths
    // ------------------------------------------------------------------

    function test_Take_ExpiredQuote_Reverts() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, uint64(block.timestamp + 60), false);

        vm.warp(block.timestamp + 120);
        vm.prank(bob);
        vm.expectRevert(YieldQSDDesk.QuoteExpired.selector);
        desk.take{value: 12 * ONE}(id, 10 * ONE);
    }

    function test_Take_ClosedQuote_Reverts() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);

        vm.prank(bob);
        desk.take{value: 12 * ONE}(id, 10 * ONE);

        // Quote is fully filled. Second take reverts.
        vm.prank(carol);
        vm.expectRevert(YieldQSDDesk.QuoteNotOpen.selector);
        desk.take{value: 12 * ONE}(id, 10 * ONE);
    }

    function test_Take_OverRemaining_Reverts() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), true);

        vm.prank(bob);
        vm.expectRevert(
            abi.encodeWithSelector(YieldQSDDesk.InsufficientLiquidity.selector, 11 * ONE, 10 * ONE)
        );
        desk.take{value: 13 * ONE}(id, 11 * ONE);
    }

    // ------------------------------------------------------------------
    // cancel
    // ------------------------------------------------------------------

    function test_Cancel_ByMaker_RefundsYqsd() public {
        uint256 aliceBefore = yqsd.balanceOf(alice);
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);

        vm.prank(alice);
        desk.cancel(id);

        assertEq(yqsd.balanceOf(alice), aliceBefore, "yQSD refunded");
        assertEq(desk.openQuoteCount(), 0);
        assertFalse(desk.getQuote(id).open);
    }

    function test_Cancel_ByMaker_RefundsQrl() public {
        uint256 aliceBefore = alice.balance;
        vm.prank(alice);
        uint256 id = desk.postBuy{value: 12 * ONE}(10 * ONE, _futureExpiry(), false);

        vm.prank(alice);
        desk.cancel(id);

        assertEq(alice.balance, aliceBefore, "QRL refunded");
        assertEq(desk.openQuoteCount(), 0);
    }

    function test_Cancel_ByNonMakerBeforeExpiry_Reverts() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);

        vm.prank(bob);
        vm.expectRevert(YieldQSDDesk.NotMaker.selector);
        desk.cancel(id);
    }

    function test_Cancel_ByAnyoneAfterExpiry() public {
        uint256 aliceBefore = yqsd.balanceOf(alice);
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, uint64(block.timestamp + 60), false);

        vm.warp(block.timestamp + 120);
        vm.prank(bob);  // anyone may cancel an expired quote
        desk.cancel(id);

        assertEq(yqsd.balanceOf(alice), aliceBefore, "yQSD refunded to alice (the maker)");
        assertEq(desk.openQuoteCount(), 0);
    }

    function test_Cancel_PartiallyFilled_RefundsRemainder() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), true);

        // Carol takes 4 yQSD for 4 * 12 / 10 = 4.8 QRL (exact).
        vm.prank(carol);
        desk.take{value: 48 * ONE / 10}(id, 4 * ONE);

        // Alice cancels; remaining 6 yQSD refunded.
        uint256 aliceBefore = yqsd.balanceOf(alice);
        vm.prank(alice);
        desk.cancel(id);

        assertEq(yqsd.balanceOf(alice) - aliceBefore, 6 * ONE, "remaining yQSD refunded");
    }

    // ------------------------------------------------------------------
    // Pagination
    // ------------------------------------------------------------------

    function test_ListOpenQuotes_Pagination() public {
        // Post 5 quotes from alice.
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(alice);
            desk.postSell((i + 1) * ONE, (i + 1) * 2 * ONE, _futureExpiry(), false);
        }

        (uint256[] memory ids, YieldQSDDesk.Quote[] memory page, uint256 total) =
            desk.listOpenQuotes(0, 3);
        assertEq(total, 5);
        assertEq(ids.length, 3);
        assertEq(page.length, 3);
        assertEq(ids[0], 1);
        assertEq(ids[1], 2);
        assertEq(ids[2], 3);

        (ids, page, total) = desk.listOpenQuotes(3, 10);
        assertEq(total, 5);
        assertEq(ids.length, 2);
        assertEq(ids[0], 4);
        assertEq(ids[1], 5);

        // Offset beyond total returns empty.
        (ids, page, total) = desk.listOpenQuotes(10, 5);
        assertEq(total, 5);
        assertEq(ids.length, 0);
    }

    function test_ListOpenQuotes_AfterClose_Compacts() public {
        // Post 3 quotes; cancel #2; verify list is contiguous.
        vm.startPrank(alice);
        uint256 id1 = desk.postSell(1 * ONE, 1 * ONE, _futureExpiry(), false);
        uint256 id2 = desk.postSell(2 * ONE, 2 * ONE, _futureExpiry(), false);
        uint256 id3 = desk.postSell(3 * ONE, 3 * ONE, _futureExpiry(), false);
        desk.cancel(id2);
        vm.stopPrank();

        (uint256[] memory ids, , uint256 total) = desk.listOpenQuotes(0, 10);
        assertEq(total, 2);
        assertEq(ids.length, 2);
        // After swap-and-pop, id1 stays at index 0 and id3 lands at index 1.
        assertEq(ids[0], id1);
        assertEq(ids[1], id3);
    }

    // ------------------------------------------------------------------
    // Price view
    // ------------------------------------------------------------------

    function test_QuotePrice_ScaledQrlPerYqsd() public {
        vm.prank(alice);
        uint256 id = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);
        // 12 / 10 = 1.2 QRL per yQSD = 1.2 × 1e18.
        assertEq(desk.quotePriceQrlPerYqsd(id), 12 * ONE * 1e18 / (10 * ONE));
    }

    // ------------------------------------------------------------------
    // openQuoteCount tracks correctly
    // ------------------------------------------------------------------

    function test_OpenQuoteCount_TracksLifecycle() public {
        assertEq(desk.openQuoteCount(), 0);

        vm.prank(alice);
        uint256 id1 = desk.postSell(10 * ONE, 12 * ONE, _futureExpiry(), false);
        assertEq(desk.openQuoteCount(), 1);

        vm.prank(bob);
        uint256 id2 = desk.postBuy{value: 5 * ONE}(4 * ONE, _futureExpiry(), false);
        assertEq(desk.openQuoteCount(), 2);

        vm.prank(carol);
        desk.take{value: 12 * ONE}(id1, 10 * ONE);  // fills id1
        assertEq(desk.openQuoteCount(), 1);

        vm.prank(bob);
        desk.cancel(id2);
        assertEq(desk.openQuoteCount(), 0);
    }
}
