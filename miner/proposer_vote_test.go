// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package miner

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core"
	"github.com/theQRL/go-qrl/core/rawdb"
	"github.com/theQRL/go-qrl/core/state"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
	"github.com/theQRL/go-qrl/params"
)

// proposerVoteTestState builds an in-memory state DB with the QSD
// predeploys loaded and `validator` registered as the sole entry of
// ValidatorOracle.validators[]. Mirrors the pattern used by
// core/paymaster_integration_test.go::newPredeployedState.
func proposerVoteTestState(t *testing.T, validator common.Address) *state.StateDB {
	t.Helper()
	alloc := core.GenesisAlloc{}
	core.AddQSDStabilityLayer(alloc, core.DefaultQSDPredeployParams(common.Address{}))
	core.SeedQSDDevValidator(alloc, validator, big.NewInt(1_000_000_000_000_000_000))

	db := rawdb.NewMemoryDatabase()
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabase(db), nil)
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	for addr, acc := range alloc {
		sdb.CreateAccount(addr)
		sdb.SetCode(addr, acc.Code)
		for slot, val := range acc.Storage {
			sdb.SetState(addr, slot, val)
		}
		if acc.Balance != nil && acc.Balance.Sign() > 0 {
			sdb.AddBalance(addr, acc.Balance)
		}
	}
	return sdb
}

func proposerVoteEnv(coinbase common.Address, number uint64, sdb *state.StateDB, txs []*types.Transaction) *environment {
	header := &types.Header{
		Number:  new(big.Int).SetUint64(number),
		BaseFee: big.NewInt(1),
	}
	return &environment{
		signer:   types.LatestSignerForChainID(params.TestChainConfig.ChainID),
		state:    sdb,
		coinbase: coinbase,
		header:   header,
		txs:      txs,
	}
}

func voteTx(t *testing.T, w wallet.Wallet, nonce uint64, forBlock uint64) *types.Transaction {
	t.Helper()
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	calldata := append([]byte{}, core.SubmitVoteSelector...)
	calldata = append(calldata, common.LeftPadBytes(new(big.Int).SetUint64(forBlock).Bytes(), 32)...)
	calldata = append(calldata, common.LeftPadBytes(big.NewInt(1_000_000_000_000_000_000).Bytes(), 32)...)
	to := core.ValidatorOracleAddress
	tx, err := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID: params.TestChainConfig.ChainID, Nonce: nonce,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1),
		Gas: 100_000, To: &to, Value: big.NewInt(0), Data: calldata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func walletAddress(w wallet.Wallet) common.Address {
	a := w.GetAddress()
	return common.BytesToAddress(a[:])
}

func TestEnsureProposerVote_NotAValidator_OK(t *testing.T) {
	w := partitionTestWallet(t)
	some := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000aaaa"))
	// Seed the state with `some` as the validator, but the coinbase
	// is `walletAddress(w)`, which is NOT registered.
	sdb := proposerVoteTestState(t, some)
	env := proposerVoteEnv(walletAddress(w), 7, sdb, nil)
	if err := ensureProposerVote(env); err != nil {
		t.Errorf("non-validator coinbase should be skipped, got %v", err)
	}
}

func TestEnsureProposerVote_ValidatorWithVote_OK(t *testing.T) {
	w := partitionTestWallet(t)
	coinbase := walletAddress(w)
	sdb := proposerVoteTestState(t, coinbase)
	env := proposerVoteEnv(coinbase, 7, sdb, []*types.Transaction{
		voteTx(t, w, 0, 7),
	})
	if err := ensureProposerVote(env); err != nil {
		t.Errorf("validator coinbase with proper vote should pass, got %v", err)
	}
}

func TestEnsureProposerVote_ValidatorWithoutVote_Fails(t *testing.T) {
	w := partitionTestWallet(t)
	coinbase := walletAddress(w)
	sdb := proposerVoteTestState(t, coinbase)
	env := proposerVoteEnv(coinbase, 7, sdb, nil)
	if err := ensureProposerVote(env); err == nil {
		t.Fatal("validator coinbase without own submitVote should fail to assemble")
	}
}

func TestEnsureProposerVote_ValidatorWithStaleTargetVote_Fails(t *testing.T) {
	w := partitionTestWallet(t)
	coinbase := walletAddress(w)
	sdb := proposerVoteTestState(t, coinbase)
	// Vote targets block 6; we're building block 7.
	env := proposerVoteEnv(coinbase, 7, sdb, []*types.Transaction{
		voteTx(t, w, 0, 6),
	})
	if err := ensureProposerVote(env); err == nil {
		t.Fatal("vote targeting wrong block should fail to satisfy proposer rule")
	}
}
