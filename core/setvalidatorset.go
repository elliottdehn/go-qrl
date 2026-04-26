// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"fmt"
	"math/big"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/crypto"
)

// SetValidatorSetSystemGasLimit is the gas budget allocated to the
// per-block setValidatorSet() system call. Generous: the contract's
// internal diff loop is O(N*M) where N and M are bounded by
// MAX_VALIDATORS = 100, plus the swap-pop / vote-clear / push work,
// plus a per-validator SLOAD/SSTORE. Empirically a one-validator
// add costs ~150k gas; a full 100-validator rotation can plausibly
// cost ~5M. This budget is well above worst case and never charged
// to a user — it's amortized into block production.
const SetValidatorSetSystemGasLimit uint64 = 30_000_000

// setValidatorSetSelector is the 4-byte function selector for
//
//	setValidatorSet(address[])
//
// computed once at init and verified against `cast sig` in tests.
var setValidatorSetSelector = crypto.Keccak256([]byte("setValidatorSet(address[])"))[:4]

// ProcessSetValidatorSet emits the per-block consensus-driven
// system call that mirrors the chain's PoS validator set into the
// ValidatorOracle predeploy. Called from state_processor.Process at
// the start of each block, before transactions execute, so
// in-block submitVote calls see the up-to-date set.
//
// The validator list is whatever the block carries in
// body.Validators; consensus is responsible for populating it from
// beacon state. An empty list is a no-op (the helper isn't called
// in that case — see Process).
func ProcessSetValidatorSet(qrvm *vm.QRVM, validators []common.Address) error {
	if len(validators) > maxValidatorSetSize {
		return fmt.Errorf("validator set size %d exceeds limit %d",
			len(validators), maxValidatorSetSize)
	}
	calldata := encodeSetValidatorSetCalldata(validators)

	sender := vm.AccountRef(SystemCallerAddress)
	_, _, vmerr := qrvm.Call(sender, ValidatorOracleAddress, calldata,
		SetValidatorSetSystemGasLimit, common.Big0)
	if vmerr != nil {
		return fmt.Errorf("setValidatorSet reverted: %w", vmerr)
	}
	return nil
}

// maxValidatorSetSize must match MAX_VALIDATORS in
// contracts/src/ValidatorOracle.sol. Pinned here to fail fast on
// absurd input before the EVM runs out of gas inside the diff
// loop. The 2625 ceiling reflects the chain's economic max
// (max_supply / min_stake), so it's effectively non-binding;
// quickselect keeps the per-block median compute O(N) at this
// scale, which fits comfortably in QRL's 60s slot.
const maxValidatorSetSize = 2625

// encodeSetValidatorSetCalldata produces the ABI calldata for
//
//	setValidatorSet(address[])
//
// Layout: 4-byte selector || 32-byte offset (= 0x20, points at the
// dynamic-array length) || 32-byte length || N * 32-byte addresses
// (right-aligned).
func encodeSetValidatorSetCalldata(validators []common.Address) []byte {
	const headerSize = 4 + 32 + 32
	out := make([]byte, 0, headerSize+32*len(validators))
	out = append(out, setValidatorSetSelector...)

	// Offset to the dynamic-array data, measured from the start of
	// the argument area (after the selector). Single dynamic arg →
	// data starts immediately after the offset slot, i.e. at byte 32.
	offset := common.LeftPadBytes(big.NewInt(32).Bytes(), 32)
	out = append(out, offset...)

	// Array length.
	out = append(out, common.LeftPadBytes(big.NewInt(int64(len(validators))).Bytes(), 32)...)

	// Each address right-aligned in a 32-byte slot.
	for _, v := range validators {
		var slot common.Hash
		copy(slot[12:], v.Bytes())
		out = append(out, slot.Bytes()...)
	}
	return out
}
