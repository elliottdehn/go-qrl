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
	"github.com/theQRL/go-qrl/core/txpool"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
	"github.com/theQRL/go-qrl/params"
)

func partitionTestWallet(t *testing.T) wallet.Wallet {
	t.Helper()
	w, err := wallet.RestoreFromSeedHex(
		"010000b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f29100000000000000000000000000000000",
	)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func voteLazy(t *testing.T, w wallet.Wallet, nonce uint64) *txpool.LazyTransaction {
	t.Helper()
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	calldata := append([]byte{}, core.SubmitVoteSelector...)
	calldata = append(calldata, common.LeftPadBytes(big.NewInt(1).Bytes(), 32)...)
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
	return &txpool.LazyTransaction{Tx: tx, Hash: tx.Hash(), GasFeeCap: tx.GasFeeCap(), GasTipCap: tx.GasTipCap(), Gas: tx.Gas()}
}

func nonVoteLazy(t *testing.T, w wallet.Wallet, nonce uint64) *txpool.LazyTransaction {
	t.Helper()
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	to := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	tx, err := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID: params.TestChainConfig.ChainID, Nonce: nonce,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1),
		Gas: 21_000, To: &to, Value: big.NewInt(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &txpool.LazyTransaction{Tx: tx, Hash: tx.Hash(), GasFeeCap: tx.GasFeeCap(), GasTipCap: tx.GasTipCap(), Gas: tx.Gas()}
}

func TestPartitionVoteTxs_LeadingVotes(t *testing.T) {
	w := partitionTestWallet(t)
	addr := common.BytesToAddress(common.FromHex("0xaa"))
	in := map[common.Address][]*txpool.LazyTransaction{
		addr: {
			voteLazy(t, w, 0),
			voteLazy(t, w, 1),
			nonVoteLazy(t, w, 2),
			nonVoteLazy(t, w, 3),
		},
	}
	votes, other := partitionVoteTxs(in)
	if got := len(votes[addr]); got != 2 {
		t.Errorf("votes: got %d entries, want 2", got)
	}
	if got := len(other[addr]); got != 2 {
		t.Errorf("other: got %d entries, want 2", got)
	}
}

func TestPartitionVoteTxs_NoVotes(t *testing.T) {
	w := partitionTestWallet(t)
	addr := common.BytesToAddress(common.FromHex("0xaa"))
	in := map[common.Address][]*txpool.LazyTransaction{
		addr: {
			nonVoteLazy(t, w, 0),
			nonVoteLazy(t, w, 1),
		},
	}
	votes, other := partitionVoteTxs(in)
	if _, ok := votes[addr]; ok {
		t.Error("votes: should be empty for no-votes account")
	}
	if got := len(other[addr]); got != 2 {
		t.Errorf("other: got %d entries, want 2", got)
	}
}

func TestPartitionVoteTxs_AllVotes(t *testing.T) {
	w := partitionTestWallet(t)
	addr := common.BytesToAddress(common.FromHex("0xaa"))
	in := map[common.Address][]*txpool.LazyTransaction{
		addr: {voteLazy(t, w, 0), voteLazy(t, w, 1), voteLazy(t, w, 2)},
	}
	votes, other := partitionVoteTxs(in)
	if got := len(votes[addr]); got != 3 {
		t.Errorf("votes: got %d, want 3", got)
	}
	if _, ok := other[addr]; ok {
		t.Error("other: should be empty for all-votes account")
	}
}

// TestPartitionVoteTxs_VoteAfterNonVote_Truncates: when a vote
// follows a non-vote in an account's nonce stream, everything from
// the misplaced vote onward is held back. The block gets the
// leading non-votes only; the vote (and any txs after it) stay in
// the pool for a later block.
func TestPartitionVoteTxs_VoteAfterNonVote_Truncates(t *testing.T) {
	w := partitionTestWallet(t)
	addr := common.BytesToAddress(common.FromHex("0xaa"))
	in := map[common.Address][]*txpool.LazyTransaction{
		addr: {
			nonVoteLazy(t, w, 0),
			nonVoteLazy(t, w, 1),
			voteLazy(t, w, 2),    // misplaced
			nonVoteLazy(t, w, 3), // also held back
		},
	}
	votes, other := partitionVoteTxs(in)
	if _, ok := votes[addr]; ok {
		t.Error("votes: account starts with non-vote, should not contribute")
	}
	if got := len(other[addr]); got != 2 {
		t.Errorf("other: got %d, want 2 (only the leading non-votes)", got)
	}
}

func TestPartitionVoteTxs_LeadingThenTrailingVote_TruncatesAtTrailing(t *testing.T) {
	w := partitionTestWallet(t)
	addr := common.BytesToAddress(common.FromHex("0xaa"))
	in := map[common.Address][]*txpool.LazyTransaction{
		addr: {
			voteLazy(t, w, 0),    // leading vote
			nonVoteLazy(t, w, 1), // ok
			voteLazy(t, w, 2),    // misplaced
			nonVoteLazy(t, w, 3), // held back
		},
	}
	votes, other := partitionVoteTxs(in)
	if got := len(votes[addr]); got != 1 {
		t.Errorf("votes: got %d, want 1", got)
	}
	if got := len(other[addr]); got != 1 {
		t.Errorf("other: got %d, want 1 (only the one non-vote between the votes)", got)
	}
}
