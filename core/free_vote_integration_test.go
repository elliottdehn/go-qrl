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
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/crypto"
	"github.com/theQRL/go-qrl/params"
)

// registerValidator faithfully mirrors what
// ValidatorOracle.setValidatorSet does on a fresh add: appends `v`
// to the validators[] dynamic array (slot 1 length + element at
// keccak256(slot 1) + i) AND sets validatorIndex[v] to its 1-indexed
// position. Without the array push, the oracle's median computation
// would silently exclude this validator.
func registerValidator(t *testing.T, sdb *state.StateDB, v common.Address) {
	t.Helper()

	// Read current array length.
	lengthSlot := common.Hash{}
	lengthSlot[31] = byte(validatorOracleValidatorsLengthSlot)
	current := sdb.GetState(ValidatorOracleAddress, lengthSlot).Big().Uint64()

	// Append: validators[current] at keccak256(slot 1) + current.
	arrayBase := crypto.Keccak256Hash(common.LeftPadBytes(
		big.NewInt(validatorOracleValidatorsLengthSlot).Bytes(), 32))
	idxInt := new(big.Int).Add(new(big.Int).SetBytes(arrayBase.Bytes()), new(big.Int).SetUint64(current))
	elemSlot := common.BytesToHash(common.LeftPadBytes(idxInt.Bytes(), 32))
	sdb.SetState(ValidatorOracleAddress, elemSlot,
		common.BytesToHash(common.LeftPadBytes(v.Bytes(), 32)))

	// Bump length by 1.
	sdb.SetState(ValidatorOracleAddress, lengthSlot,
		common.BigToHash(new(big.Int).SetUint64(current+1)))

	// Set validatorIndex[v] = current + 1 (1-indexed).
	sdb.SetState(ValidatorOracleAddress, ValidatorIndexStorageSlot(v),
		common.BigToHash(new(big.Int).SetUint64(current+1)))
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
	registerValidator(t, sdb, alice)

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
	registerValidator(t, sdb, alice)

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
	registerValidator(t, sdb, alice)

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

// TestFreeVote_VoteFedIntoOracleMedian verifies the end-to-end path
// the chain relies on for fee-token pricing: a registered validator
// submits a vote, and that vote shows up in the oracle's median —
// which is what every paymaster-fairness comparison ultimately
// reads. Catches the class of bug where the validators[] array and
// the votes[] mapping fall out of sync (e.g. registerValidator
// previously only wrote validatorIndex, leaving validators[] empty
// and the vote orphaned).
func TestFreeVote_VoteFedIntoOracleMedian(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))

	sdb := newPredeployedState(t, alice)
	registerValidator(t, sdb, alice)

	const gasLimit = 200_000
	gasPrice := big.NewInt(1_000_000_000)
	fundNativeForGas(sdb, alice, gasLimit, gasPrice)

	// Alice's intended vote — exactly the value we expect to read
	// back out of the oracle's median.
	want, _ := new(big.Int).SetString("1500000000000000000", 10) // $1.50

	msg := &Message{
		From:      alice,
		To:        &ValidatorOracleAddress,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  gasLimit,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Data:      buildSubmitVoteCalldata(big.NewInt(1), want),
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))
	if result.Failed() {
		t.Fatalf("submitVote failed: %v", result.Err)
	}

	// Sanity: alice's vote IS in storage at votes[alice].
	voteSlot := sdb.GetState(ValidatorOracleAddress, ValidatorVoteStorageSlot(alice))
	storedPrice := new(big.Int).SetBytes(voteSlot[16:32])
	if storedPrice.Cmp(want) != 0 {
		t.Fatalf("vote not stored: votes[alice] price=%s, want %s", storedPrice, want)
	}

	// And: the oracle's median — driven by iterating validators[] —
	// reflects alice's vote. With the registerValidator helper now
	// pushing into the array, this round-trips end-to-end.
	got, healthy := ComputeOraclePrice(sdb, 1)
	if !healthy {
		t.Fatalf("oracle should be healthy with one fresh vote in a one-validator set")
	}
	if got.Cmp(want) != 0 {
		t.Errorf("oracle median: got %s, want %s", got, want)
	}
}

// TestFreeVote_DoesNotCountTowardBlockGas verifies the consensus
// invariant added when the network outgrew the "free vote still
// debits the block gas limit" stopgap: a successful free vote
// returns ALL of its initialGas to the block's GasPool and reports
// UsedGas == 0, so a block packed with N validator votes has the
// same effective gas budget for user txs as a block with zero
// votes. Without this, a 100-validator network would burn
// ~100 * 30k of every block's gas-limit budget on infrastructure
// before any user tx got a chance to land.
func TestFreeVote_DoesNotCountTowardBlockGas(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))

	sdb := newPredeployedState(t, alice)
	registerValidator(t, sdb, alice)

	const gasLimit = 200_000
	gasPrice := big.NewInt(1_000_000_000)
	fundNativeForGas(sdb, alice, gasLimit, gasPrice)

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

	chainCfg := &params.ChainConfig{ChainID: big.NewInt(1337)}
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b")),
		BlockNumber: big.NewInt(1),
		Time:        1,
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(0),
	}
	txCtx := vm.TxContext{Origin: msg.From, GasPrice: msg.GasPrice}
	qrvm := vm.NewQRVM(blockCtx, txCtx, sdb, chainCfg, vm.Config{})

	const startingPool uint64 = 30_000_000
	gp := new(GasPool).AddGas(startingPool)

	result, err := ApplyMessage(qrvm, msg, gp)
	if err != nil {
		t.Fatalf("ApplyMessage: %v", err)
	}
	if result.Failed() {
		t.Fatalf("submitVote failed: %v", result.Err)
	}

	if result.UsedGas != 0 {
		t.Errorf("free vote result.UsedGas: got %d, want 0", result.UsedGas)
	}
	if got := uint64(*gp); got != startingPool {
		t.Errorf("free vote depleted block gas pool: got %d, want %d", got, startingPool)
	}
}

func TestFreeVote_NonSubmitVoteCallToOracle_Paid(t *testing.T) {
	// A validator calling some non-submitVote function on the oracle
	// (here: a function that doesn't exist, which falls through to
	// the empty fallback) must NOT get the free path.
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	coinbase := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))

	sdb := newPredeployedState(t, alice)
	registerValidator(t, sdb, alice)

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
