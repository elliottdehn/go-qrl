// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {YieldQSD} from "../src/YieldQSD.sol";
import {MockERC20} from "./mocks/MockERC20.sol";

/// @notice Unit tests for YieldQSD as a co-minted yield-claim token.
///         These tests use a fake-QSD address to drive issueTo /
///         redeemFrom directly. End-to-end tests against the real QSD
///         contract live in QSD.t.sol and QSDLeverage.t.sol.
contract YieldQSDTest is Test {
    YieldQSD public yqsd;
    MockERC20 public qsdMock;
    MockERC20 public iqrl;

    // We use qsdMock as the "authorized caller" address. yQSD trusts
    // whoever owns this address; in production it's the real QSD
    // contract, in tests it's a MockERC20 that we vm.prank() through.
    address public alice = address(0xA11CE);
    address public bob   = address(0xB0B);
    address public carol = address(0xCAA01);
    address public eve   = address(0xEEE); // unauthorized actor
    address public distributor = address(0xD157);

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        qsdMock = new MockERC20("Mock QSD", "QSD");
        iqrl    = new MockERC20("Inverse QRL", "iQRL");
        yqsd    = new YieldQSD(qsdMock, iqrl);

        iqrl.mint(distributor, 10_000 * ONE);
        vm.prank(distributor); iqrl.approve(address(yqsd), type(uint256).max);
    }

    function _issue(address to, uint256 amount) internal {
        vm.prank(address(qsdMock));
        yqsd.issueTo(to, amount);
    }

    function _redeem(address from, uint256 amount) internal {
        vm.prank(address(qsdMock));
        yqsd.redeemFrom(from, amount);
    }

    function _distribute(uint256 amount) internal {
        vm.prank(distributor);
        yqsd.distribute(amount);
    }

    // ------------------------------------------------------------------
    // Access control: only QSD can issueTo / redeemFrom
    // ------------------------------------------------------------------

    function test_IssueTo_OnlyQsd() public {
        vm.prank(eve);
        vm.expectRevert(YieldQSD.OnlyQsd.selector);
        yqsd.issueTo(alice, 100 * ONE);
    }

    function test_RedeemFrom_OnlyQsd() public {
        _issue(alice, 100 * ONE);
        vm.prank(eve);
        vm.expectRevert(YieldQSD.OnlyQsd.selector);
        yqsd.redeemFrom(alice, 50 * ONE);
    }

    function test_IssueTo_Zero_Reverts() public {
        vm.prank(address(qsdMock));
        vm.expectRevert(YieldQSD.ZeroAmount.selector);
        yqsd.issueTo(alice, 0);
    }

    function test_RedeemFrom_Zero_Reverts() public {
        vm.prank(address(qsdMock));
        vm.expectRevert(YieldQSD.ZeroAmount.selector);
        yqsd.redeemFrom(alice, 0);
    }

    function test_RedeemFrom_InsufficientBalance_Reverts() public {
        _issue(alice, 50 * ONE);
        vm.prank(address(qsdMock));
        vm.expectRevert(
            abi.encodeWithSelector(
                YieldQSD.InsufficientYieldClaim.selector,
                alice,
                100 * ONE,
                50 * ONE
            )
        );
        yqsd.redeemFrom(alice, 100 * ONE);
    }

    // ------------------------------------------------------------------
    // Issue / redeem mechanics
    // ------------------------------------------------------------------

    function test_IssueTo_MintsTokens() public {
        _issue(alice, 100 * ONE);
        assertEq(yqsd.balanceOf(alice), 100 * ONE);
        assertEq(yqsd.totalSupply(), 100 * ONE);
    }

    function test_RedeemFrom_BurnsTokens() public {
        _issue(alice, 100 * ONE);
        _redeem(alice, 40 * ONE);
        assertEq(yqsd.balanceOf(alice), 60 * ONE);
        assertEq(yqsd.totalSupply(), 60 * ONE);
    }

    // ------------------------------------------------------------------
    // Reward distribution & claim
    // ------------------------------------------------------------------

    function test_Distribute_ProRata_TwoHolders() public {
        _issue(alice, 60 * ONE);
        _issue(bob,   40 * ONE);
        _distribute(100 * ONE);

        assertEq(yqsd.pending(alice), 60 * ONE);
        assertEq(yqsd.pending(bob),   40 * ONE);
    }

    function test_Claim_TransfersIqrl() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);

        uint256 balBefore = iqrl.balanceOf(alice);
        vm.prank(alice);
        uint256 amount = yqsd.claim();
        assertEq(amount, 50 * ONE);
        assertEq(iqrl.balanceOf(alice) - balBefore, 50 * ONE);
        assertEq(yqsd.pending(alice), 0);
    }

    function test_Claim_TwiceIsIdempotent() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);

        vm.prank(alice); yqsd.claim();
        vm.prank(alice);
        uint256 amount = yqsd.claim();
        assertEq(amount, 0);
    }

    function test_Distribute_BeforeIssuance_Stranded() public {
        // No yQSD holders → distribute → iQRL sits in contract.
        _distribute(100 * ONE);
        assertEq(yqsd.accRewardPerShare(), 0);
        assertEq(iqrl.balanceOf(address(yqsd)), 100 * ONE);

        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);
        // Alice only earns the post-issuance distribution.
        assertEq(yqsd.pending(alice), 50 * ONE);
    }

    // ------------------------------------------------------------------
    // Sequencing: issue / distribute / issue / distribute
    // ------------------------------------------------------------------

    function test_LateHolder_NoBackdatedRewards() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);  // alice → 50

        _issue(bob, 100 * ONE);  // late
        _distribute(50 * ONE);   // alice + bob split: 25 each

        assertEq(yqsd.pending(alice), 75 * ONE);
        assertEq(yqsd.pending(bob),   25 * ONE);
    }

    function test_RedeemFrom_PreservesPendingRewards() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);

        // Burn all yQSD from alice; pending iQRL still claimable.
        _redeem(alice, 100 * ONE);

        assertEq(yqsd.balanceOf(alice), 0);
        assertEq(yqsd.pending(alice), 50 * ONE);

        vm.prank(alice);
        uint256 claimed = yqsd.claim();
        assertEq(claimed, 50 * ONE);
    }

    function test_RedeemFrom_NoFutureRewards() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);

        _redeem(alice, 100 * ONE);

        _issue(bob, 100 * ONE);
        _distribute(50 * ONE);

        assertEq(yqsd.pending(alice), 50 * ONE);
        assertEq(yqsd.pending(bob), 50 * ONE);
    }

    // ------------------------------------------------------------------
    // ERC20 transfer settlement (yQSD is freely transferable)
    // ------------------------------------------------------------------

    function test_Transfer_SettlesRewards() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);

        // Alice transfers 50 yQSD to Carol — alice's pending crystallizes.
        vm.prank(alice);
        yqsd.transfer(carol, 50 * ONE);

        assertEq(yqsd.pending(alice), 50 * ONE);
        assertEq(yqsd.pending(carol), 0);

        _distribute(40 * ONE);
        assertEq(yqsd.pending(alice), 50 * ONE + 20 * ONE);
        assertEq(yqsd.pending(carol), 20 * ONE);
    }

    function test_Transfer_ToFreshRecipient_NoBackdate() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);

        vm.prank(alice);
        yqsd.transfer(bob, 100 * ONE);

        assertEq(yqsd.pending(alice), 50 * ONE);
        assertEq(yqsd.pending(bob), 0);
    }

    // ------------------------------------------------------------------
    // Distribute is permissionless
    // ------------------------------------------------------------------

    function test_Distribute_PermissionlessCaller() public {
        _issue(alice, 100 * ONE);

        iqrl.mint(bob, 100 * ONE);
        vm.prank(bob); iqrl.approve(address(yqsd), 100 * ONE);
        vm.prank(bob); yqsd.distribute(75 * ONE);

        assertEq(yqsd.pending(alice), 75 * ONE);
    }

    function test_Distribute_Zero_Reverts() public {
        vm.prank(distributor);
        vm.expectRevert(YieldQSD.ZeroAmount.selector);
        yqsd.distribute(0);
    }

    function test_Distribute_RecordsTotal() public {
        _issue(alice, 100 * ONE);
        _distribute(50 * ONE);
        _distribute(75 * ONE);
        assertEq(yqsd.totalDistributed(), 125 * ONE);
    }

    // ------------------------------------------------------------------
    // Three-holder pro-rata fuzz
    // ------------------------------------------------------------------

    function testFuzz_ProRata_ThreeHolders(uint96 a, uint96 b, uint96 c, uint96 d) public {
        a = uint96(bound(uint256(a), 1e15, 1_000 * ONE));
        b = uint96(bound(uint256(b), 1e15, 1_000 * ONE));
        c = uint96(bound(uint256(c), 1e15, 1_000 * ONE));
        d = uint96(bound(uint256(d), 1e15, 1_000 * ONE));

        _issue(alice, a);
        _issue(bob,   b);
        _issue(carol, c);
        _distribute(d);

        uint256 supply = uint256(a) + uint256(b) + uint256(c);
        uint256 tol = supply / 1e18 + 100;
        assertApproxEqAbs(yqsd.pending(alice), uint256(d) * a / supply, tol);
        assertApproxEqAbs(yqsd.pending(bob),   uint256(d) * b / supply, tol);
        assertApproxEqAbs(yqsd.pending(carol), uint256(d) * c / supply, tol);
    }

    function test_TotalPending_NeverExceedsDistributed() public {
        _issue(alice, 100 * ONE);
        _issue(bob,   60  * ONE);
        _issue(carol, 40  * ONE);
        _distribute(200 * ONE);

        uint256 total = yqsd.pending(alice) + yqsd.pending(bob) + yqsd.pending(carol);
        assertLe(total, 200 * ONE);
        assertApproxEqAbs(total, 200 * ONE, 10);
    }
}
