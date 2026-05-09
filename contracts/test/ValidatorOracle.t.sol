// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import {Test} from "forge-std/Test.sol";

import {ValidatorOracle} from "../src/ValidatorOracle.sol";

contract ValidatorOracleTest is Test {
    ValidatorOracle internal oracle;
    address internal SYSTEM;

    address[5] internal vs = [
        address(0xA1),
        address(0xA2),
        address(0xA3),
        address(0xA4),
        address(0xA5)
    ];

    uint256 internal constant ONE = 1e18;
    uint256 internal constant STALE = 10;

    function setUp() public {
        oracle = new ValidatorOracle(STALE, 2, 3); // 2/3 quorum
        SYSTEM = oracle.SYSTEM_CALLER();
    }

    /// Helper: invoke the consensus-driven setValidatorSet from
    /// the sentinel system caller with the supplied list.
    function _setSet(address[] memory addrs) internal {
        vm.prank(SYSTEM);
        oracle.setValidatorSet(addrs);
    }

    function _addAllValidators() internal {
        address[] memory all = new address[](vs.length);
        for (uint256 i = 0; i < vs.length; i++) all[i] = vs[i];
        _setSet(all);
    }

    // ------------------------------------------------------------------
    // validator set
    // ------------------------------------------------------------------

    function test_SetValidatorSet_OnlySystemCaller() public {
        address[] memory one = new address[](1);
        one[0] = vs[0];
        vm.expectRevert(ValidatorOracle.NotSystemCaller.selector);
        oracle.setValidatorSet(one);
    }

    function test_SetValidatorSet_StoresAndIndexes() public {
        address[] memory one = new address[](1);
        one[0] = vs[0];
        _setSet(one);

        assertEq(oracle.validatorCount(), 1);
        assertEq(oracle.validators(0), vs[0]);
        assertEq(oracle.validatorIndex(vs[0]), 1);
    }

    function test_SetValidatorSet_DeduplicatesWithinSet() public {
        address[] memory dupes = new address[](3);
        dupes[0] = vs[0];
        dupes[1] = vs[0]; // duplicate
        dupes[2] = vs[1];
        _setSet(dupes);

        assertEq(oracle.validatorCount(), 2);
        assertEq(oracle.validators(0), vs[0]);
        assertEq(oracle.validators(1), vs[1]);
    }

    function test_SetValidatorSet_RemovesDeparted_KeepsCommon() public {
        _addAllValidators();
        // Drop vs[1] (middle).
        address[] memory next = new address[](4);
        next[0] = vs[0];
        next[1] = vs[2];
        next[2] = vs[3];
        next[3] = vs[4];
        _setSet(next);

        assertEq(oracle.validatorCount(), 4);
        // vs[1] is gone.
        assertEq(oracle.validatorIndex(vs[1]), 0);
        // The retained validators all still have non-zero indices.
        assertGt(oracle.validatorIndex(vs[0]), 0);
        assertGt(oracle.validatorIndex(vs[2]), 0);
        assertGt(oracle.validatorIndex(vs[3]), 0);
        assertGt(oracle.validatorIndex(vs[4]), 0);
    }

    function test_SetValidatorSet_DeletesDepartedVote() public {
        _addAllValidators();
        vm.prank(vs[0]);
        oracle.submitVote(block.number, ONE);
        (uint128 p,) = oracle.votes(vs[0]);
        assertEq(p, ONE);

        // Drop vs[0].
        address[] memory next = new address[](4);
        next[0] = vs[1];
        next[1] = vs[2];
        next[2] = vs[3];
        next[3] = vs[4];
        _setSet(next);

        (p,) = oracle.votes(vs[0]);
        assertEq(p, 0, "vote not cleared on removal");
    }

    function test_SetValidatorSet_RetainedValidatorKeepsVote() public {
        _addAllValidators();
        vm.prank(vs[2]);
        oracle.submitVote(block.number, 7 * ONE);

        // Set unchanged — vs[2] is still in.
        _addAllValidators();

        (uint128 p,) = oracle.votes(vs[2]);
        assertEq(p, 7 * ONE, "retained validator's vote was dropped");
    }

    function test_SetValidatorSet_LimitEnforced() public {
        address[] memory tooMany = new address[](oracle.MAX_VALIDATORS() + 1);
        for (uint256 i = 0; i < tooMany.length; i++) {
            tooMany[i] = address(uint160(i + 1));
        }
        vm.prank(SYSTEM);
        vm.expectRevert(ValidatorOracle.ValidatorLimitReached.selector);
        oracle.setValidatorSet(tooMany);
    }

    function test_SetValidatorSet_EmptySet_ClearsAll() public {
        _addAllValidators();
        _setSet(new address[](0));
        assertEq(oracle.validatorCount(), 0);
        assertEq(oracle.validatorIndex(vs[0]), 0);
    }

    // ------------------------------------------------------------------
    // voting
    // ------------------------------------------------------------------

    function test_SubmitVote_OnlyValidators() public {
        vm.expectRevert(ValidatorOracle.NotValidator.selector);
        oracle.submitVote(block.number,ONE);
    }

    function test_SubmitVote_ZeroRejected() public {
        _addAllValidators();
        vm.prank(vs[0]);
        vm.expectRevert(ValidatorOracle.ZeroPrice.selector);
        oracle.submitVote(block.number,0);
    }

    function test_SubmitVote_Overwrites() public {
        _addAllValidators();
        vm.prank(vs[0]);
        oracle.submitVote(block.number,ONE);
        vm.roll(block.number + 1);
        vm.prank(vs[0]);
        oracle.submitVote(block.number,2 * ONE);
        (uint128 p, uint64 b) = oracle.votes(vs[0]);
        assertEq(p, 2 * ONE);
        assertEq(b, block.number);
    }

    function test_SubmitVote_WrongBlockNumber_Past_Reverts() public {
        _addAllValidators();
        vm.roll(100);
        vm.prank(vs[0]);
        vm.expectRevert(
            abi.encodeWithSelector(
                ValidatorOracle.WrongBlockNumber.selector,
                99,
                100
            )
        );
        oracle.submitVote(99, ONE); // targeting last block
    }

    function test_SubmitVote_WrongBlockNumber_Future_Reverts() public {
        _addAllValidators();
        vm.roll(100);
        vm.prank(vs[0]);
        vm.expectRevert(
            abi.encodeWithSelector(
                ValidatorOracle.WrongBlockNumber.selector,
                101,
                100
            )
        );
        oracle.submitVote(101, ONE); // targeting next block
    }

    function test_SubmitVote_StaleVote_DoesNotOverwrite_FreshVote() public {
        // The whole reason for the strict block check: a tx submitted
        // for block N that mines late must NOT silently overwrite a
        // fresher vote that landed first.
        _addAllValidators();

        // Imagine V's qsdfeeder ticked at block 100 and signed a
        // submitVote(100, $1) tx that got stuck in the mempool.
        // Meanwhile V's daemon ticked again at block 105 and a
        // submitVote(105, $2) tx mined into block 105 successfully.
        vm.roll(105);
        vm.prank(vs[0]);
        oracle.submitVote(105, 2 * ONE);

        // The stuck $1 vote (intended for block 100) finally mines
        // in block 106. Strict-targeting reverts it instead of
        // letting the stale price clobber the fresh one.
        vm.roll(106);
        vm.prank(vs[0]);
        vm.expectRevert(
            abi.encodeWithSelector(
                ValidatorOracle.WrongBlockNumber.selector,
                100,
                106
            )
        );
        oracle.submitVote(100, ONE);

        // V's recorded vote remains the $2 from block 105.
        (uint128 p,) = oracle.votes(vs[0]);
        assertEq(p, 2 * ONE);
    }

    // ------------------------------------------------------------------
    // median
    // ------------------------------------------------------------------

    function test_Median_OddCount() public {
        _addAllValidators();
        uint256[5] memory prices = [ONE * 3, ONE * 5, ONE * 1, ONE * 4, ONE * 2];
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,prices[i]);
        }
        // Sorted: 1, 2, 3, 4, 5. Median = 3.
        assertEq(oracle.price(), ONE * 3);
    }

    function test_Median_EvenCount() public {
        address[] memory four = new address[](4);
        for (uint256 i = 0; i < 4; i++) four[i] = vs[i];
        _setSet(four);

        uint256[4] memory prices = [ONE * 10, ONE * 20, ONE * 40, ONE * 30];
        for (uint256 i = 0; i < 4; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,prices[i]);
        }
        // Sorted: 10, 20, 30, 40. Median = (20+30)/2 = 25.
        assertEq(oracle.price(), ONE * 25);
    }

    function test_Median_IgnoresStaleVotes() public {
        _addAllValidators();
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE * (i + 1));
        }
        // Advance past staleness window.
        vm.roll(block.number + STALE + 1);
        // Re-vote for only 3 validators with a different price.
        for (uint256 i = 0; i < 3; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE * 100);
        }
        // Only 3 fresh votes (all 100), median = 100.
        // Stale votes for vs[3], vs[4] are ignored.
        assertEq(oracle.price(), ONE * 100);
    }

    function test_Price_ZeroWithNoFreshVotes() public {
        _addAllValidators();
        assertEq(oracle.price(), 0);
    }

    // ------------------------------------------------------------------
    // healthy
    // ------------------------------------------------------------------

    function test_Healthy_RequiresQuorum() public {
        _addAllValidators(); // 5 validators; 2/3 quorum → need >= 4 fresh votes
        assertFalse(oracle.healthy());

        vm.prank(vs[0]);
        oracle.submitVote(block.number,ONE);
        assertFalse(oracle.healthy()); // 1/5

        vm.prank(vs[1]);
        oracle.submitVote(block.number,ONE);
        assertFalse(oracle.healthy()); // 2/5

        vm.prank(vs[2]);
        oracle.submitVote(block.number,ONE);
        assertFalse(oracle.healthy()); // 3/5: 3*3=9 < 2*5=10

        vm.prank(vs[3]);
        oracle.submitVote(block.number,ONE);
        assertTrue(oracle.healthy()); // 4/5: 4*3=12 >= 2*5=10
    }

    function test_Healthy_StaleVotesDrop() public {
        _addAllValidators();
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE);
        }
        assertTrue(oracle.healthy());

        vm.roll(block.number + STALE + 1);
        assertFalse(oracle.healthy());
    }

    function test_Healthy_FalseWithNoValidators() public {
        assertFalse(oracle.healthy());
    }

    // ------------------------------------------------------------------
    // per-block cache
    // ------------------------------------------------------------------

    // Cache-population tests. The contract intentionally does NOT
    // update the cache from inside submitVote / setValidatorSet:
    // consensus calls pokeCache() once per block as a system call
    // after the vote phase, and that is the canonical refresh path.
    // These tests invoke pokeCache() directly to simulate the
    // consensus invocation.

    function test_PokeCache_PopulatesAfterSubmitVotes() public {
        _addAllValidators();
        for (uint256 i = 0; i < 4; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE * (i + 1));
        }
        // submitVote alone does NOT touch the cache.
        (, uint64 beforeBlock,) = oracle.cache();
        assertEq(beforeBlock, 0);

        oracle.pokeCache();

        (uint128 cachedPrice, uint64 cachedBlock, uint8 healthyFlag) = oracle.cache();
        assertEq(cachedBlock, uint64(block.number));
        // 4 votes: 1, 2, 3, 4. Median = (2 + 3) / 2 = 2.5
        assertEq(cachedPrice, uint128((2 * ONE + 3 * ONE) / 2));
        assertEq(healthyFlag, 1); // 4 of 5 = 12/15 ≥ 10/15
    }

    function test_PokeCache_AfterSetValidatorSet_Add() public {
        address[] memory one = new address[](1);
        one[0] = vs[0];
        _setSet(one);
        // setValidatorSet alone does NOT touch the cache.
        (, uint64 b0,) = oracle.cache();
        assertEq(b0, 0);

        oracle.pokeCache();
        (, uint64 b1,) = oracle.cache();
        assertEq(b1, uint64(block.number));

        vm.roll(block.number + 1);
        address[] memory two = new address[](2);
        two[0] = vs[0];
        two[1] = vs[1];
        _setSet(two);
        oracle.pokeCache();
        (, uint64 b2,) = oracle.cache();
        assertEq(b2, uint64(block.number));
    }

    function test_PokeCache_AfterSetValidatorSet_Remove() public {
        _addAllValidators();
        oracle.pokeCache();
        vm.roll(block.number + 1);
        // Drop vs[2].
        address[] memory next = new address[](4);
        next[0] = vs[0];
        next[1] = vs[1];
        next[2] = vs[3];
        next[3] = vs[4];
        _setSet(next);
        oracle.pokeCache();
        (, uint64 cachedBlock,) = oracle.cache();
        assertEq(cachedBlock, uint64(block.number));
    }

    function test_Cache_HitVsMiss_GasComparison() public {
        _addAllValidators();
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE * (i + 1));
        }
        // Consensus would invoke pokeCache here as a system call.
        oracle.pokeCache();

        // First read in same block as pokeCache → cache hit.
        uint256 g1 = gasleft();
        oracle.price();
        uint256 hitGas = g1 - gasleft();

        // Advance a block; cache becomes stale → miss → recompute.
        vm.roll(block.number + 1);
        uint256 g2 = gasleft();
        oracle.price();
        uint256 missGas = g2 - gasleft();

        // Hit should be substantially cheaper than miss.
        assertLt(hitGas, missGas, "cache hit not cheaper");
        // Concrete bound: cache hit reads 1 storage slot; miss reads N
        // storage slots and runs quickselect. We expect at least 2x.
        assertLt(hitGas * 2, missGas);
    }

    function test_Cache_Invalidates_OnNewBlock() public {
        _addAllValidators();
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE * (i + 1));
        }
        oracle.pokeCache();
        (, uint64 cachedAt,) = oracle.cache();
        assertEq(cachedAt, uint64(block.number));

        vm.roll(block.number + 1);
        // Cache still has the OLD block. Reads must recompute.
        (, uint64 stillCachedAt,) = oracle.cache();
        assertEq(stillCachedAt, uint64(block.number) - 1);

        // Despite stale cache, price() must return fresh-recomputed
        // value (still 3 since no new votes).
        assertEq(oracle.price(), ONE * 3);
    }

    function test_Cache_RecomputesCorrectlyOnMiss() public {
        _addAllValidators();
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE * (i + 1));
        }

        // Advance past staleness window so all votes become stale
        // without any new write.
        vm.roll(block.number + STALE + 1);

        // Cache from old block; read must recompute and find no fresh
        // votes.
        assertEq(oracle.price(), 0);
        assertFalse(oracle.healthy());
    }

    function test_PokeCache_RefreshesWithoutVote() public {
        _addAllValidators();
        for (uint256 i = 0; i < 5; i++) {
            vm.prank(vs[i]);
            oracle.submitVote(block.number,ONE * (i + 1));
        }

        vm.roll(block.number + 1);
        // Cache is stale (from previous block). Poke updates it.
        oracle.pokeCache();
        (uint128 cachedPrice, uint64 cachedBlock,) = oracle.cache();
        assertEq(cachedBlock, uint64(block.number));
        assertEq(cachedPrice, uint128(3 * ONE));
    }

    // ------------------------------------------------------------------
    // construction
    // ------------------------------------------------------------------

    function test_BadQuorumRejected() public {
        vm.expectRevert(ValidatorOracle.BadQuorum.selector);
        new ValidatorOracle(STALE, 5, 0); // denom = 0

        vm.expectRevert(ValidatorOracle.BadQuorum.selector);
        new ValidatorOracle(STALE, 5, 3); // numer > denom
    }

    // ------------------------------------------------------------------
    // fuzz
    // ------------------------------------------------------------------

    function testFuzz_Median_MatchesSortedMiddle(uint64[5] memory prices) public {
        _addAllValidators();
        for (uint256 i = 0; i < 5; i++) {
            uint256 p = uint256(prices[i]);
            if (p == 0) p = 1;
            vm.prank(vs[i]);
            oracle.submitVote(block.number,p);
        }

        uint256 med = oracle.price();

        // Independent median check: sort the inputs and pick the middle.
        uint256[] memory sorted = new uint256[](5);
        for (uint256 i = 0; i < 5; i++) {
            uint256 p = uint256(prices[i]);
            sorted[i] = p == 0 ? 1 : p;
        }
        for (uint256 i = 1; i < 5; i++) {
            uint256 key = sorted[i];
            uint256 j = i;
            while (j > 0 && sorted[j - 1] > key) {
                sorted[j] = sorted[j - 1];
                j--;
            }
            sorted[j] = key;
        }
        assertEq(med, sorted[2]);
    }
}
