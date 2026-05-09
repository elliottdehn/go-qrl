// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"fmt"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/crypto"
)

// PokeCacheSystemGasLimit is the gas budget allocated to the per-
// block pokeCache() system call. It rebuilds the median over up to
// MAX_VALIDATORS (= 2625) fresh votes via quickselect, plus the
// per-validator SLOAD on votes[v]. Worst case is ~5.5M gas in
// SLOADs (1 read per validator) plus ~50k for the quickselect over
// memory. Pad heavily: this budget is never charged to a user.
const PokeCacheSystemGasLimit uint64 = 30_000_000

// pokeCacheSelector is the 4-byte function selector for
//
//	pokeCache()
//
// computed once at init and verified against `cast sig` in tests.
var pokeCacheSelector = crypto.Keccak256([]byte("pokeCache()"))[:4]

// ProcessPokeCache emits the per-block consensus-driven system call
// that finalizes ValidatorOracle's per-block median cache. Called
// from state_processor.Process at the boundary between the vote
// phase and the non-vote phase, so non-vote txs (paymaster fee
// splits, iQRL.mint, QSD.redeem, ...) read a cache that already
// reflects every submitVote in the block.
//
// submitVote and setValidatorSet deliberately do NOT update the
// cache themselves; rebuilding it once per block via this system
// call is O(N) per block, versus O(N^2) if every submitVote
// recomputed.
func ProcessPokeCache(qrvm *vm.QRVM) error {
	sender := vm.AccountRef(SystemCallerAddress)
	_, _, vmerr := qrvm.Call(sender, ValidatorOracleAddress, pokeCacheSelector,
		PokeCacheSystemGasLimit, common.Big0)
	if vmerr != nil {
		return fmt.Errorf("pokeCache reverted: %w", vmerr)
	}
	return nil
}
