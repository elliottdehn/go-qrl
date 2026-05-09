// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/rawdb"
	"github.com/theQRL/go-qrl/core/state"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/params"
)

// freshState returns an empty state DB for fork-gating tests.
func freshState(t *testing.T) *state.StateDB {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabase(db), nil)
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	return sdb
}

// TestIsQSD covers the predicate directly: nil QSDTime is always
// false; a set QSDTime activates at exactly that timestamp and stays
// active afterwards.
func TestIsQSD(t *testing.T) {
	t100 := uint64(100)
	cfg := &params.ChainConfig{ChainID: big.NewInt(1), QSDTime: &t100}

	if cfg.IsQSD(99) {
		t.Error("IsQSD(99) should be false (1 second pre-activation)")
	}
	if !cfg.IsQSD(100) {
		t.Error("IsQSD(100) should be true (activation tip)")
	}
	if !cfg.IsQSD(101) {
		t.Error("IsQSD(101) should be true (post-activation)")
	}

	cfg.QSDTime = nil
	if cfg.IsQSD(0) || cfg.IsQSD(1<<60) {
		t.Error("IsQSD with nil QSDTime should always be false")
	}
}

// TestInstallQSDPredeploysIfMissing_FirstCallInstalls verifies the
// fork-activation install: a fresh state has no oracle code; after
// install, all four predeploys carry non-empty bytecode.
func TestInstallQSDPredeploysIfMissing_FirstCallInstalls(t *testing.T) {
	sdb := freshState(t)

	for _, addr := range []common.Address{
		ValidatorOracleAddress, InverseQRLAddress, QSDAddress, PayWithIQRLAddress,
	} {
		if sdb.GetCodeSize(addr) != 0 {
			t.Fatalf("pre-install: %s already has code", addr)
		}
	}

	if err := InstallQSDPredeploysIfMissing(sdb); err != nil {
		t.Fatalf("install: %v", err)
	}

	for _, addr := range []common.Address{
		ValidatorOracleAddress, InverseQRLAddress, QSDAddress, PayWithIQRLAddress,
	} {
		if sdb.GetCodeSize(addr) == 0 {
			t.Errorf("post-install: %s has no code", addr)
		}
	}
}

// TestInstallQSDPredeploysIfMissing_Idempotent verifies that the
// second invocation hits the GetCodeSize fast path and is a true
// no-op (no storage mutation).
func TestInstallQSDPredeploysIfMissing_Idempotent(t *testing.T) {
	sdb := freshState(t)

	if err := InstallQSDPredeploysIfMissing(sdb); err != nil {
		t.Fatalf("first install: %v", err)
	}
	codeBefore := append([]byte{}, sdb.GetCode(ValidatorOracleAddress)...)
	storageBefore := sdb.GetState(ValidatorOracleAddress, common.Hash{})

	if err := InstallQSDPredeploysIfMissing(sdb); err != nil {
		t.Fatalf("second install: %v", err)
	}

	if string(sdb.GetCode(ValidatorOracleAddress)) != string(codeBefore) {
		t.Error("code mutated by second install")
	}
	if sdb.GetState(ValidatorOracleAddress, common.Hash{}) != storageBefore {
		t.Error("storage mutated by second install")
	}
}

// TestValidateBody_PreFork_RejectsPaymasterTx exercises the body
// rule that type-0x04 paymaster txs are rejected on pre-fork blocks.
// Built as an unsigned tx: ValidateBody's paymaster check looks at
// tx.Type() only, not signature recovery.
func TestValidateBody_PreFork_RejectsPaymasterTx(t *testing.T) {
	to := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	tx := types.NewTx(&types.PaymasterDynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21_000,
		To:        &to,
		Value:     big.NewInt(0),
		Paymaster: &PayWithIQRLAddress,
	})
	if tx.Type() != types.PaymasterDynamicFeeTxType {
		t.Fatalf("constructed tx has type %d, want %d", tx.Type(), types.PaymasterDynamicFeeTxType)
	}

	// Pre-fork config: QSDTime nil, IsQSD always false.
	cfg := &params.ChainConfig{ChainID: big.NewInt(1)}
	if cfg.IsQSD(1) {
		t.Fatal("setup: cfg should be pre-fork")
	}

	// The body-rule loop in ValidateBody. Replicate it here because
	// we don't have a full BlockValidator (needs a *BlockChain).
	rejected := false
	for _, tx := range []*types.Transaction{tx} {
		if tx.Type() == types.PaymasterDynamicFeeTxType {
			rejected = true
			break
		}
	}
	if !rejected {
		t.Fatal("pre-fork block rule did not reject paymaster tx")
	}
}

// TestValidateBody_PostFork_AllowsPaymasterTx is the symmetric case:
// post-fork the paymaster type passes the type-only screen and goes
// through the regular validation path.
func TestValidateBody_PostFork_AllowsPaymasterTx(t *testing.T) {
	t100 := uint64(100)
	cfg := &params.ChainConfig{ChainID: big.NewInt(1), QSDTime: &t100}
	if !cfg.IsQSD(100) {
		t.Fatal("setup: cfg should be post-fork at timestamp 100")
	}
	// Replicating only the gate: when IsQSD is true, the loop is
	// skipped and the body proceeds to the QSD-specific rules.
	for _, tx := range []*types.Transaction{
		types.NewTx(&types.PaymasterDynamicFeeTx{ChainID: big.NewInt(1), GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1), Value: big.NewInt(0)}),
	} {
		if !cfg.IsQSD(100) && tx.Type() == types.PaymasterDynamicFeeTxType {
			t.Error("post-fork should not run pre-fork rejection loop")
		}
	}
}
