// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/params"
)

func TestSetValidatorSetSelector_MatchesCast(t *testing.T) {
	// `cast sig "setValidatorSet(address[])"` -> 0xcfed2565
	want := []byte{0xcf, 0xed, 0x25, 0x65}
	if !bytes.Equal(setValidatorSetSelector, want) {
		t.Errorf("selector: got %x, want %x", setValidatorSetSelector, want)
	}
}

func TestEncodeSetValidatorSetCalldata_Layout(t *testing.T) {
	a := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	b := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000b0b0"))
	got := encodeSetValidatorSetCalldata([]common.Address{a, b})

	// 4 selector + 32 offset + 32 length + 2 * 32 elements.
	if len(got) != 4+32+32+64 {
		t.Fatalf("length: got %d, want %d", len(got), 4+32+32+64)
	}
	if !bytes.Equal(got[:4], setValidatorSetSelector) {
		t.Errorf("selector mismatch")
	}
	// Offset = 0x20.
	wantOffset := common.LeftPadBytes(big.NewInt(32).Bytes(), 32)
	if !bytes.Equal(got[4:36], wantOffset) {
		t.Errorf("offset: got %x, want %x", got[4:36], wantOffset)
	}
	// Length = 2.
	wantLen := common.LeftPadBytes(big.NewInt(2).Bytes(), 32)
	if !bytes.Equal(got[36:68], wantLen) {
		t.Errorf("length slot: got %x, want %x", got[36:68], wantLen)
	}
	// Element 0: a, right-aligned.
	wantA := common.LeftPadBytes(a.Bytes(), 32)
	if !bytes.Equal(got[68:100], wantA) {
		t.Errorf("elem 0: got %x, want %x", got[68:100], wantA)
	}
	// Element 1: b.
	wantB := common.LeftPadBytes(b.Bytes(), 32)
	if !bytes.Equal(got[100:132], wantB) {
		t.Errorf("elem 1: got %x, want %x", got[100:132], wantB)
	}
}

func TestEncodeSetValidatorSetCalldata_EmptyArray(t *testing.T) {
	got := encodeSetValidatorSetCalldata(nil)
	// Selector + offset + length-zero. No elements.
	if len(got) != 4+32+32 {
		t.Fatalf("empty calldata: got len %d, want %d", len(got), 4+32+32)
	}
	if got[35] != 32 {
		t.Errorf("offset bottom byte: got %d, want 32", got[35])
	}
	if got[67] != 0 {
		t.Errorf("length: got %d, want 0", got[67])
	}
}

// applyValidatorSetUpdate is a tiny wrapper that builds a fresh QRVM
// against the supplied state DB and runs ProcessSetValidatorSet.
// Mirrors the call site in state_processor.Process closely enough
// for assertions about the resulting on-chain state.
func applyValidatorSetUpdate(t *testing.T, sdb stateDBLike, validators []common.Address, blockNum uint64) error {
	t.Helper()
	chainCfg := &params.ChainConfig{ChainID: big.NewInt(1337)}
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		BlockNumber: new(big.Int).SetUint64(blockNum),
		Time:        1,
		GasLimit:    SetValidatorSetSystemGasLimit + 1_000_000,
		BaseFee:     big.NewInt(0),
	}
	qrvm := vm.NewQRVM(blockCtx, vm.TxContext{}, sdb.(stateDB), chainCfg, vm.Config{})
	return ProcessSetValidatorSet(qrvm, validators)
}

// stateDB / stateDBLike type plumbing — the QRVM constructor wants a
// concrete *state.StateDB; the helper above receives it via an
// interface so test files in this package can pass either the real
// type or any future fake.
type stateDBLike interface{}

// stateDB is the in-package alias for *state.StateDB used to satisfy
// the vm.NewQRVM signature without importing the state package
// directly into this file.
type stateDB = vm.StateDB

func TestProcessSetValidatorSet_PopulatesOracleStorage(t *testing.T) {
	a := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	b := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000b0b0"))
	c := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c0de"))

	// Predeployed state with empty validator set.
	sdb := newPredeployedState(t, common.Address{})

	if err := applyValidatorSetUpdate(t, sdb, []common.Address{a, b, c}, 1); err != nil {
		t.Fatalf("ProcessSetValidatorSet: %v", err)
	}

	// validators[] length at slot 0 should be 3.
	lengthSlot := common.Hash{}
	lengthSlot[31] = byte(validatorOracleValidatorsLengthSlot)
	got := sdb.GetState(ValidatorOracleAddress, lengthSlot).Big().Uint64()
	if got != 3 {
		t.Errorf("validators[] length: got %d, want 3", got)
	}

	// validatorIndex[a..c] all non-zero.
	for _, v := range []common.Address{a, b, c} {
		idx := sdb.GetState(ValidatorOracleAddress, ValidatorIndexStorageSlot(v)).Big().Uint64()
		if idx == 0 {
			t.Errorf("%s not registered (index 0)", v.Hex())
		}
	}
}

func TestProcessSetValidatorSet_DiffsAgainstCurrentSet(t *testing.T) {
	a := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	b := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000b0b0"))
	c := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c0de"))

	sdb := newPredeployedState(t, common.Address{})

	// Initial set: [a, b].
	if err := applyValidatorSetUpdate(t, sdb, []common.Address{a, b}, 1); err != nil {
		t.Fatal(err)
	}
	// Second set: [b, c]. a removed, c added, b retained.
	if err := applyValidatorSetUpdate(t, sdb, []common.Address{b, c}, 2); err != nil {
		t.Fatal(err)
	}

	// a is gone.
	if idx := sdb.GetState(ValidatorOracleAddress, ValidatorIndexStorageSlot(a)).Big().Uint64(); idx != 0 {
		t.Errorf("a should be removed, got index=%d", idx)
	}
	// b retained, c added.
	for _, v := range []common.Address{b, c} {
		if idx := sdb.GetState(ValidatorOracleAddress, ValidatorIndexStorageSlot(v)).Big().Uint64(); idx == 0 {
			t.Errorf("%s should be registered", v.Hex())
		}
	}
}

func TestProcessSetValidatorSet_RetainedValidatorKeepsVote(t *testing.T) {
	a := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	b := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000b0b0"))

	sdb := newPredeployedState(t, common.Address{})
	if err := applyValidatorSetUpdate(t, sdb, []common.Address{a, b}, 1); err != nil {
		t.Fatal(err)
	}

	// Pre-seed a vote for `a` directly in storage (mirrors what
	// submitVote would have done in a real chain run).
	const price uint64 = 1234
	var voteSlot common.Hash
	// Layout: bytes [16:32] = price (uint128), bytes [8:16] =
	// blockNumber (uint64). The block we're "voting in" is 1.
	for i := 0; i < 8; i++ {
		voteSlot[31-16-i] = byte(price >> (8 * i))
	}
	voteSlot[15] = 1 // blockNumber low byte = 1
	sdb.SetState(ValidatorOracleAddress, ValidatorVoteStorageSlot(a), voteSlot)

	// Re-run with a different set that still contains a (and adds b).
	if err := applyValidatorSetUpdate(t, sdb, []common.Address{a, b}, 2); err != nil {
		t.Fatal(err)
	}

	got := sdb.GetState(ValidatorOracleAddress, ValidatorVoteStorageSlot(a))
	if got != voteSlot {
		t.Errorf("retained validator's vote was clobbered: got %x, want %x",
			got, voteSlot)
	}
}

func TestProcessSetValidatorSet_OverLimit_Rejected(t *testing.T) {
	sdb := newPredeployedState(t, common.Address{})
	too := make([]common.Address, maxValidatorSetSize+1)
	for i := range too {
		too[i] = common.BytesToAddress(big.NewInt(int64(i + 1)).Bytes())
	}
	if err := applyValidatorSetUpdate(t, sdb, too, 1); err == nil {
		t.Fatal("expected error for oversized validator set")
	}
}
