// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package types_test

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/trie"
)

// Re-exported aliases for terser test code.
var (
	emptyValidatorsHash = types.EmptyValidatorsHash
	emptyTxsHash        = types.EmptyTxsHash
)
type (
	header     = types.Header
	body       = types.Body
	validators = types.Validators
)

func newHasher() types.TrieHasher { return trie.NewStackTrie(nil) }

func newBlock(h *header, b *body, receipts []*types.Receipt) *types.Block {
	return types.NewBlock(h, b, receipts, newHasher())
}

func TestNewBlock_ValidatorsHash_NilLeavesUnset(t *testing.T) {
	hdr := &header{
		Number:  big.NewInt(1),
		BaseFee: big.NewInt(1),
	}
	bd := &body{} // Validators == nil
	block := newBlock(hdr, bd, nil)

	if got := block.Header().ValidatorsHash; got != nil {
		t.Errorf("nil Validators should leave ValidatorsHash unset, got %x", got)
	}
}

func TestNewBlock_ValidatorsHash_EmptyUsesSentinel(t *testing.T) {
	hdr := &header{
		Number:  big.NewInt(1),
		BaseFee: big.NewInt(1),
	}
	bd := &body{Validators: []common.Address{}} // explicitly empty
	block := newBlock(hdr, bd, nil)

	got := block.Header().ValidatorsHash
	if got == nil {
		t.Fatal("empty Validators should set the empty-sentinel hash")
	}
	if *got != emptyValidatorsHash {
		t.Errorf("hash: got %x, want %x", *got, emptyValidatorsHash)
	}
}

func TestNewBlock_ValidatorsHash_PopulatedComputesRoot(t *testing.T) {
	a := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	b := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000b0b0"))

	hdr := &header{
		Number:  big.NewInt(1),
		BaseFee: big.NewInt(1),
	}
	bd := &body{Validators: []common.Address{a, b}}
	block := newBlock(hdr, bd, nil)

	got := block.Header().ValidatorsHash
	if got == nil {
		t.Fatal("populated Validators should set ValidatorsHash")
	}
	if *got == emptyValidatorsHash {
		t.Errorf("populated Validators should not equal the empty sentinel")
	}
	// Recomputing the same trie should yield the same hash.
	want := types.DeriveSha(validators{a, b}, trie.NewStackTrie(nil))
	if *got != want {
		t.Errorf("hash: got %x, want %x", *got, want)
	}

	// And: a different set produces a different hash.
	c := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c0de"))
	bd2 := &body{Validators: []common.Address{a, c}}
	block2 := newBlock(&header{Number: big.NewInt(1), BaseFee: big.NewInt(1)}, bd2, nil)
	got2 := block2.Header().ValidatorsHash
	if got2 == nil || *got == *got2 {
		t.Error("different Validators slices should produce different hashes")
	}
}

func TestCopyHeader_ClonesValidatorsHash(t *testing.T) {
	hash := common.HexToHash("0xdeadbeef00000000000000000000000000000000000000000000000000000001")
	src := &header{
		Number:         big.NewInt(1),
		BaseFee:        big.NewInt(1),
		ValidatorsHash: &hash,
	}
	cpy := types.CopyHeader(src)

	if cpy.ValidatorsHash == nil {
		t.Fatal("clone lost ValidatorsHash")
	}
	if cpy.ValidatorsHash == src.ValidatorsHash {
		t.Error("clone shares pointer with src — should be a deep copy")
	}
	if *cpy.ValidatorsHash != *src.ValidatorsHash {
		t.Errorf("hash: got %x, want %x", *cpy.ValidatorsHash, *src.ValidatorsHash)
	}
}

func TestEmptyBody_AccountsForValidators(t *testing.T) {
	// Header with empty txs + empty withdrawals + non-empty validators
	// commitment → not empty body.
	hash := common.HexToHash("0xabcd0000000000000000000000000000000000000000000000000000000000ab")
	h := &header{
		TxHash:         emptyTxsHash,
		ValidatorsHash: &hash,
	}
	if h.EmptyBody() {
		t.Error("header with non-empty ValidatorsHash should not be EmptyBody")
	}

	// Empty across all three: should be empty body.
	h2 := &header{
		TxHash:         emptyTxsHash,
		ValidatorsHash: &emptyValidatorsHash,
	}
	if !h2.EmptyBody() {
		t.Error("header with all-empty roots should be EmptyBody")
	}
}
