// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/crypto"
	"github.com/theQRL/go-qrl/params"
)

// TestPokeCacheSelector pins the on-chain selector. If anyone
// renames the contract function, this test catches the silent
// drift.
func TestPokeCacheSelector(t *testing.T) {
	want := crypto.Keccak256([]byte("pokeCache()"))[:4]
	if string(pokeCacheSelector) != string(want) {
		t.Errorf("selector: got %x, want %x", pokeCacheSelector, want)
	}
}

// TestProcessPokeCache_PopulatesCacheAfterVote: the canonical post-
// vote-phase flow. setValidatorSet (no cache update), submitVote
// (no cache update), pokeCache → cache reflects the vote.
func TestProcessPokeCache_PopulatesCacheAfterVote(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	sdb := newPredeployedState(t, alice)
	registerValidator(t, sdb, alice)

	chainCfg := params.AllDevChainProtocolChanges
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b")),
		BlockNumber: big.NewInt(1),
		Time:        1,
		GasLimit:    PokeCacheSystemGasLimit + 1_000_000,
		BaseFee:     big.NewInt(0),
	}
	qrvm := vm.NewQRVM(blockCtx, vm.TxContext{}, sdb, chainCfg, vm.Config{})

	// Pre-state: cache is empty (cachedAtBlock == 0).
	cacheSlot := common.Hash{}
	cacheSlot[31] = byte(validatorOracleCacheSlot)
	beforeCache := sdb.GetState(ValidatorOracleAddress, cacheSlot)
	if VoteBlockNumberFromSlot(beforeCache) != 0 {
		t.Fatalf("setup: cache should start clean, got block %d",
			VoteBlockNumberFromSlot(beforeCache))
	}

	// Apply a submitVote message directly. submitVote no longer
	// updates the cache, so cachedAtBlock should still be 0
	// afterwards.
	wantPrice, _ := new(big.Int).SetString("1500000000000000000", 10) // $1.50
	voteData := buildSubmitVoteCalldata(big.NewInt(1), wantPrice)
	voteMsg := &Message{
		From:      alice,
		To:        &ValidatorOracleAddress,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  200_000,
		GasPrice:  big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(1_000_000_000),
		GasTipCap: big.NewInt(1_000_000_000),
		Data:      voteData,
	}
	// Top up alice's balance for the buyGas pre-charge.
	sdb.AddBalance(alice, new(big.Int).Mul(big.NewInt(200_000), big.NewInt(1_000_000_000)))
	qrvm.Reset(NewQRVMTxContext(voteMsg), sdb)
	if _, err := ApplyMessage(qrvm, voteMsg, new(GasPool).AddGas(blockCtx.GasLimit)); err != nil {
		t.Fatalf("submitVote: %v", err)
	}

	midCache := sdb.GetState(ValidatorOracleAddress, cacheSlot)
	if VoteBlockNumberFromSlot(midCache) != 0 {
		t.Errorf("cache should still be untouched after submitVote, got block %d",
			VoteBlockNumberFromSlot(midCache))
	}

	// Now fire pokeCache as the system call would. Cache should
	// populate with the vote we just landed.
	qrvm.Reset(vm.TxContext{}, sdb)
	if err := ProcessPokeCache(qrvm); err != nil {
		t.Fatalf("ProcessPokeCache: %v", err)
	}

	afterCache := sdb.GetState(ValidatorOracleAddress, cacheSlot)
	if got := VoteBlockNumberFromSlot(afterCache); got != 1 {
		t.Errorf("post-poke cachedAtBlock: got %d, want 1", got)
	}
	gotPrice, healthy := ComputeOraclePrice(sdb, 1)
	if !healthy || gotPrice.Cmp(wantPrice) != 0 {
		t.Errorf("oracle: got (price=%s, healthy=%v), want (%s, true)",
			gotPrice, healthy, wantPrice)
	}
}

// TestProcessPokeCache_NoValidators_OK: pokeCache must succeed even
// on a chain with zero validators (e.g. immediately post-fork before
// the first setValidatorSet has fired). Cache is set to zero/false.
func TestProcessPokeCache_NoValidators_OK(t *testing.T) {
	sdb := newPredeployedState(t, common.Address{})

	chainCfg := params.AllDevChainProtocolChanges
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		BlockNumber: big.NewInt(1),
		Time:        1,
		GasLimit:    PokeCacheSystemGasLimit + 1_000_000,
		BaseFee:     big.NewInt(0),
	}
	qrvm := vm.NewQRVM(blockCtx, vm.TxContext{}, sdb, chainCfg, vm.Config{})

	if err := ProcessPokeCache(qrvm); err != nil {
		t.Fatalf("ProcessPokeCache on empty validator set: %v", err)
	}
}
