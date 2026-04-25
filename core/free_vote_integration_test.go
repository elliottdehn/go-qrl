// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/state"
)

// registerValidator inserts `v` into ValidatorOracle's
// validatorIndex mapping. The validators[] array storage is left
// empty: submitVote() doesn't read it, and _updateCache iterates a
// length-zero array harmlessly. Tests that depend on the median
// being populated would also need to set the array.
func registerValidator(sdb *state.StateDB, v common.Address) {
	sdb.SetState(ValidatorOracleAddress, ValidatorIndexStorageSlot(v),
		common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"))
}

// buildSubmitVoteCalldata produces calldata for
// submitVote(uint256 forBlockNumber, uint256 priceUsd1e18).
func buildSubmitVoteCalldata(forBlock, price *big.Int) []byte {
	out := make([]byte, 0, 4+32+32)
	out = append(out, SubmitVoteSelector...)
	out = append(out, common.LeftPadBytes(forBlock.Bytes(), 32)...)
	out = append(out, common.LeftPadBytes(price.Bytes(), 32)...)
	return out
}

// fundNativeForGas tops up sender's native balance enough to cover
// gasLimit*gasPrice. With the free-vote rule the post-state still
// equals this pre-funded amount; tests assert exactly that.
func fundNativeForGas(sdb *state.StateDB, sender common.Address, gasLimit uint64, gasPrice *big.Int) *big.Int {
	cost := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), gasPrice)
	sdb.AddBalance(sender, cost)
	return cost
}

func TestFreeVote_FirstVoteIsFree(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))

	sdb := newPredeployedState(t, alice)
	registerValidator(sdb, alice)

	const gasLimit = 200_000
	gasPrice := big.NewInt(1_000_000_000)
	balanceBefore := fundNativeForGas(sdb, alice, gasLimit, gasPrice)

	msg := &Message{
		From:      alice,
		To:        &ValidatorOracleAddress,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  gasLimit,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Data:      buildSubmitVoteCalldata(big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)), // forBlock=1, price=1e18
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))
	if result.Failed() {
		t.Fatalf("submitVote failed: %v", result.Err)
	}

	balanceAfter := sdb.GetBalance(alice)
	if balanceAfter.Cmp(balanceBefore) != 0 {
		t.Errorf("alice balance: got %s, want %s (paid %s for a free tx)",
			balanceAfter, balanceBefore,
			new(big.Int).Sub(balanceBefore, balanceAfter))
	}
	// Coinbase should NOT have received a tip.
	coinbase := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))
	if got := sdb.GetBalance(coinbase); got.Sign() != 0 {
		t.Errorf("coinbase tip: got %s, want 0 for free tx", got)
	}
}

func TestFreeVote_SecondVoteSameBlock_IsPaid(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	coinbase := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))

	sdb := newPredeployedState(t, alice)
	registerValidator(sdb, alice)

	const gasLimit = 200_000
	gasPrice := big.NewInt(1_000_000_000)
	// Fund for two votes' worth of gas, in case the second pays.
	fundNativeForGas(sdb, alice, gasLimit*2, gasPrice)

	mkVote := func(nonce uint64) *Message {
		return &Message{
			From:      alice,
			To:        &ValidatorOracleAddress,
			Nonce:     nonce,
			Value:     big.NewInt(0),
			GasLimit:  gasLimit,
			GasPrice:  gasPrice,
			GasFeeCap: gasPrice,
			GasTipCap: gasPrice,
			Data:      buildSubmitVoteCalldata(big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)),
		}
	}

	// First vote — free.
	balanceBefore := sdb.GetBalance(alice)
	r1 := applyPaymasterMessage(t, sdb, mkVote(0), big.NewInt(0))
	if r1.Failed() {
		t.Fatalf("first vote: %v", r1.Err)
	}
	balanceAfterFirst := sdb.GetBalance(alice)
	if balanceAfterFirst.Cmp(balanceBefore) != 0 {
		t.Errorf("first vote should be free; alice paid %s",
			new(big.Int).Sub(balanceBefore, balanceAfterFirst))
	}

	// Second vote — same block, same validator. Contract accepts
	// it (overwrites the prior vote), but consensus charges normal
	// gas because the free slot is already taken.
	r2 := applyPaymasterMessage(t, sdb, mkVote(1), big.NewInt(0))
	if r2.Failed() {
		t.Fatalf("second vote (overwrite): %v", r2.Err)
	}
	balanceAfterSecond := sdb.GetBalance(alice)
	paidForSecond := new(big.Int).Sub(balanceAfterFirst, balanceAfterSecond)
	expected := new(big.Int).Mul(new(big.Int).SetUint64(r2.UsedGas), gasPrice)
	if paidForSecond.Cmp(expected) != 0 {
		t.Errorf("second vote payment: got %s, want %s (gasUsed=%d)",
			paidForSecond, expected, r2.UsedGas)
	}
	// Coinbase received the second vote's tip.
	if got := sdb.GetBalance(coinbase); got.Cmp(expected) != 0 {
		t.Errorf("coinbase tip: got %s, want %s", got, expected)
	}
}

func TestFreeVote_WrongBlockReverts_Paid(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))

	sdb := newPredeployedState(t, alice)
	registerValidator(sdb, alice)

	const gasLimit = 200_000
	gasPrice := big.NewInt(1_000_000_000)
	balanceBefore := fundNativeForGas(sdb, alice, gasLimit, gasPrice)

	msg := &Message{
		From:      alice,
		To:        &ValidatorOracleAddress,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  gasLimit,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		// Wrong target block: tx is for block 99 but executes in block 1.
		Data: buildSubmitVoteCalldata(big.NewInt(99), big.NewInt(1_000_000_000_000_000_000)),
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))
	if !result.Failed() {
		t.Fatal("expected revert for wrong forBlockNumber")
	}

	balanceAfter := sdb.GetBalance(alice)
	if balanceAfter.Cmp(balanceBefore) >= 0 {
		t.Errorf("alice balance should have decreased: before=%s, after=%s",
			balanceBefore, balanceAfter)
	}
	// Reverted txs pay normal gas; the free path was gated on vmerr == nil.
}

func TestFreeVote_NonValidator_Paid(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	// Note: deliberately do NOT register alice as a validator.

	sdb := newPredeployedState(t, alice)

	const gasLimit = 200_000
	gasPrice := big.NewInt(1_000_000_000)
	balanceBefore := fundNativeForGas(sdb, alice, gasLimit, gasPrice)

	msg := &Message{
		From:      alice,
		To:        &ValidatorOracleAddress,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  gasLimit,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Data:      buildSubmitVoteCalldata(big.NewInt(1), big.NewInt(1_000_000_000_000_000_000)),
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))
	if !result.Failed() {
		t.Fatal("non-validator submitVote should revert")
	}

	if balanceAfter := sdb.GetBalance(alice); balanceAfter.Cmp(balanceBefore) >= 0 {
		t.Errorf("non-validator should pay; balance went %s -> %s", balanceBefore, balanceAfter)
	}
}

func TestFreeVote_NonSubmitVoteCallToOracle_Paid(t *testing.T) {
	// A validator calling some non-submitVote function on the oracle
	// (here: a function that doesn't exist, which falls through to
	// the empty fallback) must NOT get the free path.
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	coinbase := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))

	sdb := newPredeployedState(t, alice)
	registerValidator(sdb, alice)

	const gasLimit = 200_000
	gasPrice := big.NewInt(1_000_000_000)
	balanceBefore := fundNativeForGas(sdb, alice, gasLimit, gasPrice)

	// Bogus selector — not submitVote.
	msg := &Message{
		From:      alice,
		To:        &ValidatorOracleAddress,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  gasLimit,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Data:      []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x00, 0x00, 0x00},
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))
	// The oracle has no fallback that accepts arbitrary calldata; it
	// will revert. Whether it reverts or succeeds, the test is the
	// same: the free path must NOT engage. Validate via balance.
	balanceAfter := sdb.GetBalance(alice)
	if !result.Failed() && balanceAfter.Cmp(balanceBefore) == 0 {
		t.Errorf("non-submitVote call got the free path treatment")
	}
	// Sanity: regardless of vmerr, alice paid for what was used.
	expected := new(big.Int).Mul(new(big.Int).SetUint64(result.UsedGas), gasPrice)
	paid := new(big.Int).Sub(balanceBefore, balanceAfter)
	if paid.Cmp(expected) != 0 {
		t.Errorf("alice paid %s, want %s (gasUsed=%d)", paid, expected, result.UsedGas)
	}
	if got := sdb.GetBalance(coinbase); got.Cmp(expected) != 0 {
		t.Errorf("coinbase tip: got %s, want %s", got, expected)
	}
}
