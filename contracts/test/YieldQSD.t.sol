// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";
import {YieldQSD} from "../src/YieldQSD.sol";
import {MockERC20} from "./mocks/MockERC20.sol";

contract YieldQSDTest is Test {
    YieldQSD public yqsd;
    MockERC20 public qsd;
    MockERC20 public iqrl;

    address public alice = address(0xA11CE);
    address public bob   = address(0xB0B);
    address public carol = address(0xCAA01);
    address public distributor = address(0xD157);

    uint256 internal constant ONE = 1e18;

    function setUp() public {
        qsd  = new MockERC20("Quantum Stable Dollar", "QSD");
        iqrl = new MockERC20("Inverse QRL", "iQRL");
        yqsd = new YieldQSD(qsd, iqrl);

        // Seed test balances.
        qsd.mint(alice, 1_000 * ONE);
        qsd.mint(bob,   1_000 * ONE);
        qsd.mint(carol, 1_000 * ONE);
        iqrl.mint(distributor, 10_000 * ONE);

        // Approvals: stakers approve yQSD to pull QSD; distributor
        // approves yQSD to pull iQRL.
        vm.prank(alice); qsd.approve(address(yqsd), type(uint256).max);
        vm.prank(bob);   qsd.approve(address(yqsd), type(uint256).max);
        vm.prank(carol); qsd.approve(address(yqsd), type(uint256).max);
        vm.prank(distributor); iqrl.approve(address(yqsd), type(uint256).max);
    }

    function _stake(address from, uint256 amount) internal {
        vm.prank(from);
        yqsd.deposit(amount);
    }

    function _distribute(uint256 amount) internal {
        vm.prank(distributor);
        yqsd.distribute(amount);
    }

    // ------------------------------------------------------------------
    // 1:1 wrap mechanics
    // ------------------------------------------------------------------

    function test_Deposit_MintsOneToOne() public {
        _stake(alice, 100 * ONE);
        assertEq(yqsd.balanceOf(alice), 100 * ONE);
        assertEq(qsd.balanceOf(address(yqsd)), 100 * ONE);
        assertEq(yqsd.totalSupply(), 100 * ONE);
    }

    function test_Withdraw_ReturnsOneToOne() public {
        _stake(alice, 100 * ONE);
        vm.prank(alice);
        yqsd.withdraw(40 * ONE);
        assertEq(yqsd.balanceOf(alice), 60 * ONE);
        assertEq(qsd.balanceOf(alice), 1_000 * ONE - 60 * ONE);
    }

    function test_Deposit_Zero_Reverts() public {
        vm.prank(alice);
        vm.expectRevert(YieldQSD.ZeroAmount.selector);
        yqsd.deposit(0);
    }

    function test_Withdraw_Zero_Reverts() public {
        _stake(alice, 100 * ONE);
        vm.prank(alice);
        vm.expectRevert(YieldQSD.ZeroAmount.selector);
        yqsd.withdraw(0);
    }

    // ------------------------------------------------------------------
    // Reward distribution & claim
    // ------------------------------------------------------------------

    function test_Distribute_ProRata_TwoStakers() public {
        // Alice 60, Bob 40. Distribute 100.
        _stake(alice, 60 * ONE);
        _stake(bob,   40 * ONE);
        _distribute(100 * ONE);

        assertEq(yqsd.pending(alice), 60 * ONE);
        assertEq(yqsd.pending(bob),   40 * ONE);
    }

    function test_Claim_TransfersIqrl() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);

        uint256 balBefore = iqrl.balanceOf(alice);
        vm.prank(alice);
        uint256 amount = yqsd.claim();
        assertEq(amount, 50 * ONE);
        assertEq(iqrl.balanceOf(alice) - balBefore, 50 * ONE);
        assertEq(yqsd.pending(alice), 0);
    }

    function test_Claim_TwiceIsIdempotent() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);

        vm.prank(alice);
        yqsd.claim();

        // Second claim with no further distributions returns 0 — no over-credit.
        vm.prank(alice);
        uint256 amount = yqsd.claim();
        assertEq(amount, 0);
        assertEq(yqsd.pending(alice), 0);
    }

    function test_Distribute_BeforeStaker_Stranded() public {
        // No stakers → distribute → iQRL stays in contract but
        // accRewardPerShare doesn't advance. New stakers don't see it.
        _distribute(100 * ONE);
        assertEq(yqsd.accRewardPerShare(), 0);
        assertEq(iqrl.balanceOf(address(yqsd)), 100 * ONE);

        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);

        // Alice only earns the post-stake distribution.
        assertEq(yqsd.pending(alice), 50 * ONE);
    }

    // ------------------------------------------------------------------
    // Sequencing: stake / distribute / stake / distribute
    // ------------------------------------------------------------------

    function test_LateStaker_NoBackdatedRewards() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);  // alice gets 50

        _stake(bob, 100 * ONE);  // late
        _distribute(50 * ONE);   // alice + bob split: 25 each

        assertEq(yqsd.pending(alice), 75 * ONE);
        assertEq(yqsd.pending(bob),   25 * ONE);
    }

    function test_Withdraw_PreservesPendingRewards() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);

        // Withdraw all yQSD; pending iQRL should still be claimable.
        vm.prank(alice);
        yqsd.withdraw(100 * ONE);

        assertEq(yqsd.balanceOf(alice), 0);
        assertEq(yqsd.pending(alice), 50 * ONE);

        vm.prank(alice);
        uint256 claimed = yqsd.claim();
        assertEq(claimed, 50 * ONE);
    }

    function test_Withdraw_NoFutureRewards() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);

        vm.prank(alice);
        yqsd.withdraw(100 * ONE);

        // Bob stakes; subsequent distribution goes entirely to Bob.
        _stake(bob, 100 * ONE);
        _distribute(50 * ONE);

        // Alice still has her 50 from the pre-withdraw distribution.
        assertEq(yqsd.pending(alice), 50 * ONE);
        assertEq(yqsd.pending(bob), 50 * ONE);
    }

    // ------------------------------------------------------------------
    // ERC20 transfer settlement
    // ------------------------------------------------------------------

    function test_Transfer_SettlesRewards() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);  // alice has 50 pending

        // Alice transfers 50 yQSD to Carol. Alice's pending should
        // crystallize at 50; Carol starts with 0 pending.
        vm.prank(alice);
        yqsd.transfer(carol, 50 * ONE);

        assertEq(yqsd.pending(alice), 50 * ONE);
        assertEq(yqsd.pending(carol), 0);

        // Next distribution splits alice/carol 50/50.
        _distribute(40 * ONE);
        assertEq(yqsd.pending(alice), 50 * ONE + 20 * ONE);
        assertEq(yqsd.pending(carol), 20 * ONE);
    }

    function test_Transfer_ToFreshRecipient_NoBackdate() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);

        vm.prank(alice);
        yqsd.transfer(bob, 100 * ONE);

        // Alice keeps her 50 settled rewards; Bob gets 0 pending
        // despite now holding 100 yQSD.
        assertEq(yqsd.pending(alice), 50 * ONE);
        assertEq(yqsd.pending(bob), 0);
    }

    // ------------------------------------------------------------------
    // Distribute can be called by anyone
    // ------------------------------------------------------------------

    function test_Distribute_PermissionlessCaller() public {
        _stake(alice, 100 * ONE);

        // Bob donates iQRL to the pool; no special role required.
        iqrl.mint(bob, 100 * ONE);
        vm.prank(bob);
        iqrl.approve(address(yqsd), 100 * ONE);
        vm.prank(bob);
        yqsd.distribute(75 * ONE);

        assertEq(yqsd.pending(alice), 75 * ONE);
    }

    function test_Distribute_Zero_Reverts() public {
        vm.prank(distributor);
        vm.expectRevert(YieldQSD.ZeroAmount.selector);
        yqsd.distribute(0);
    }

    function test_Distribute_RecordsTotal() public {
        _stake(alice, 100 * ONE);
        _distribute(50 * ONE);
        _distribute(75 * ONE);
        assertEq(yqsd.totalDistributed(), 125 * ONE);
    }

    // ------------------------------------------------------------------
    // Three-staker pro-rata fuzz
    // ------------------------------------------------------------------

    function testFuzz_ProRata_ThreeStakers(uint96 a, uint96 b, uint96 c, uint96 d) public {
        a = uint96(bound(uint256(a), 1e15, 1_000 * ONE));
        b = uint96(bound(uint256(b), 1e15, 1_000 * ONE));
        c = uint96(bound(uint256(c), 1e15, 1_000 * ONE));
        d = uint96(bound(uint256(d), 1e15, 1_000 * ONE));

        _stake(alice, a);
        _stake(bob,   b);
        _stake(carol, c);
        _distribute(d);

        uint256 supply = uint256(a) + uint256(b) + uint256(c);
        // accRewardPerShare divides by supply; pending multiplies balance
        // and divides by SCALE. Allow rounding loss bounded by 1 wei per
        // staker plus floor((supply-1)/SCALE) per distribution — in
        // practice ≤ supply/SCALE wei. Cap absolute tolerance at 100 wei
        // for sanity; integer rounding never exceeds this for reasonable
        // bounds.
        uint256 tol = supply / 1e18 + 100;
        assertApproxEqAbs(yqsd.pending(alice), uint256(d) * a / supply, tol);
        assertApproxEqAbs(yqsd.pending(bob),   uint256(d) * b / supply, tol);
        assertApproxEqAbs(yqsd.pending(carol), uint256(d) * c / supply, tol);
    }

    // ------------------------------------------------------------------
    // Pending iQRL conserved
    // ------------------------------------------------------------------

    function test_TotalPending_NeverExceedsDistributed() public {
        _stake(alice, 100 * ONE);
        _stake(bob,   60  * ONE);
        _stake(carol, 40  * ONE);

        _distribute(200 * ONE);

        uint256 total = yqsd.pending(alice) + yqsd.pending(bob) + yqsd.pending(carol);
        // Conservation: total pending ≤ total distributed (allowing rounding).
        assertLe(total, 200 * ONE);
        // Three stakers, one distribution: rounding loss bounded by ~3 wei
        // per the integer division of accRewardPerShare by supply.
        assertApproxEqAbs(total, 200 * ONE, 10);
    }
}
