// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
	"github.com/theQRL/go-qrl/params"
)

// voteTestWallet is a deterministic wallet used by every test in this
// file so signed-tx hashes are stable.
func voteTestWallet(t *testing.T) wallet.Wallet {
	t.Helper()
	w, err := wallet.RestoreFromSeedHex(
		"010000b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f29100000000000000000000000000000000",
	)
	if err != nil {
		t.Fatalf("restore wallet: %v", err)
	}
	return w
}

// signSubmitVote builds and signs a submitVote(forBlock, price) tx
// targeted at the ValidatorOracle predeploy.
func signSubmitVote(t *testing.T, w wallet.Wallet, nonce uint64, forBlock, price *big.Int) *types.Transaction {
	t.Helper()
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	calldata := append([]byte{}, SubmitVoteSelector...)
	calldata = append(calldata, common.LeftPadBytes(forBlock.Bytes(), 32)...)
	calldata = append(calldata, common.LeftPadBytes(price.Bytes(), 32)...)
	to := ValidatorOracleAddress
	tx, err := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       100_000,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      calldata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// signTransfer builds and signs a vanilla value-transfer tx — a
// "non-vote" tx for the purposes of the ordering rule.
func signTransfer(t *testing.T, w wallet.Wallet, nonce uint64) *types.Transaction {
	t.Helper()
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	to := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	tx, err := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21_000,
		To:        &to,
		Value:     big.NewInt(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestValidateVoteOrdering_AllVotesFirst_OK(t *testing.T) {
	w := voteTestWallet(t)
	txs := []*types.Transaction{
		signSubmitVote(t, w, 0, big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)),
		signSubmitVote(t, w, 1, big.NewInt(2), big.NewInt(1_000_000_000_000_000_000)),
		signTransfer(t, w, 2),
		signTransfer(t, w, 3),
	}
	if err := validateVoteOrdering(txs); err != nil {
		t.Errorf("expected nil error for vote-first ordering, got %v", err)
	}
}

func TestValidateVoteOrdering_AllVotes_OK(t *testing.T) {
	w := voteTestWallet(t)
	txs := []*types.Transaction{
		signSubmitVote(t, w, 0, big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)),
		signSubmitVote(t, w, 1, big.NewInt(2), big.NewInt(1_000_000_000_000_000_000)),
	}
	if err := validateVoteOrdering(txs); err != nil {
		t.Errorf("expected nil error for all-votes block, got %v", err)
	}
}

func TestValidateVoteOrdering_NoVotes_OK(t *testing.T) {
	w := voteTestWallet(t)
	txs := []*types.Transaction{
		signTransfer(t, w, 0),
		signTransfer(t, w, 1),
	}
	if err := validateVoteOrdering(txs); err != nil {
		t.Errorf("expected nil error for no-votes block, got %v", err)
	}
}

func TestValidateVoteOrdering_VoteAfterNonVote_Rejected(t *testing.T) {
	w := voteTestWallet(t)
	txs := []*types.Transaction{
		signTransfer(t, w, 0),
		signSubmitVote(t, w, 1, big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)),
	}
	err := validateVoteOrdering(txs)
	if err == nil {
		t.Fatal("expected error: submitVote at index 1 follows a non-vote at index 0")
	}
	if !contains(err.Error(), "vote ordering") {
		t.Errorf("error %q does not mention vote ordering", err.Error())
	}
}

func TestValidateVoteOrdering_InterleavedRejected(t *testing.T) {
	w := voteTestWallet(t)
	txs := []*types.Transaction{
		signSubmitVote(t, w, 0, big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)),
		signTransfer(t, w, 1),
		signSubmitVote(t, w, 2, big.NewInt(2), big.NewInt(1_000_000_000_000_000_000)),
	}
	err := validateVoteOrdering(txs)
	if err == nil {
		t.Fatal("expected error: vote at index 2 follows transfer at index 1")
	}
}

func TestValidateVoteOrdering_EmptyBlock_OK(t *testing.T) {
	if err := validateVoteOrdering(nil); err != nil {
		t.Errorf("empty block should pass, got %v", err)
	}
}

// TestValidateVoteOrdering_NonOracleSubmitVote_NotAVote sanity-
// checks that IsSubmitVoteTx requires the recipient to be the
// ValidatorOracle predeploy. A submitVote-shaped call to a
// different address shouldn't be treated as a vote, so a non-vote
// tx ordering rule doesn't apply.
func TestValidateVoteOrdering_NonOracleSubmitVote_NotAVote(t *testing.T) {
	w := voteTestWallet(t)
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	other := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000beef"))
	calldata := append([]byte{}, SubmitVoteSelector...)
	calldata = append(calldata, make([]byte, 64)...)
	weirdTx, err := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       100_000,
		To:        &other, // NOT the oracle
		Value:     big.NewInt(0),
		Data:      calldata,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Place the weird tx after a real vote: a real submitVote at
	// index 0, then this non-vote-shaped-as-vote at 1. Should pass.
	txs := []*types.Transaction{
		signSubmitVote(t, w, 1, big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)),
		weirdTx,
	}
	if err := validateVoteOrdering(txs); err != nil {
		t.Errorf("non-oracle submitVote should be classified as non-vote, got %v", err)
	}
}

// helper — the Go stdlib's strings.Contains is fine but avoiding
// the import keeps the test file diff minimal.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
