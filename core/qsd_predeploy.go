// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"math/big"

	"github.com/theQRL/go-qrl/common"
)

// QSD Stability Layer: addresses reserved for the on-chain primitives
// that together implement the QRL-native dollar-denominated stability
// stack.
//
//   - ValidatorOracle: validator-quorum CC/USD price oracle.
//     Validators submit signed price votes via submitVote(); the
//     contract caches the unweighted median per block and exposes it
//     via the IPriceOracle interface (price() / healthy()).
//
//   - InverseQRL (iQRL): ERC-20 token whose canonical USD value tracks
//     1/p, where p is the QRL/USD oracle price. Minted by destroying
//     QRL at the rate 1/p^2 per iQRL plus a 0.5% fee. Non-redeemable;
//     the only path to destroy iQRL is burn() / burnFrom().
//
//   - QSD: dollar-denominated stablecoin backed by a symmetric pool of
//     QRL and iQRL. Solvency is a closed-form arithmetic property of
//     the pool (Cauchy-Schwarz on per-deposit contributions); the pool
//     additionally serves as a no-fee CPMM for QRL <-> iQRL swaps
//     (zero impermanent loss for inverse-priced pairs).
//
// The addresses are sequential at the start of the application
// reserved namespace, well clear of all current precompile slots.
var (
	ValidatorOracleAddress = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010000"))
	InverseQRLAddress      = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010001"))
	QSDAddress             = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010002"))
)

// QSDPredeployParams configures the genesis-time deployment of the
// QSD stability layer.
type QSDPredeployParams struct {
	// OracleOwner becomes the initial owner of ValidatorOracle. It can
	// add and remove validators from the active set. In production
	// this should be a multisig or governance contract.
	OracleOwner common.Address

	// VoteStalenessBlocks is how old a vote may be before it is
	// excluded from the median.
	VoteStalenessBlocks uint64

	// MinQuorumNumerator / MinQuorumDenominator define the minimum
	// fraction of validators that must have fresh votes for the oracle
	// to report healthy() == true. 2 / 3 is conventional.
	MinQuorumNumerator   uint64
	MinQuorumDenominator uint64
}

// DefaultQSDPredeployParams returns sensible defaults for a developer
// network.
func DefaultQSDPredeployParams(owner common.Address) QSDPredeployParams {
	return QSDPredeployParams{
		OracleOwner:          owner,
		VoteStalenessBlocks:  10,
		MinQuorumNumerator:   2,
		MinQuorumDenominator: 3,
	}
}

// AddQSDStabilityLayer registers the three QSD-stability-layer
// contracts in the supplied GenesisAlloc.
//
// Note: this initial implementation only RESERVES the addresses with
// a zero balance and empty code; full bytecode + storage seeding will
// land in a subsequent commit once the Foundry artifacts are baked in.
// This lets us land the address constants and wiring first, validate
// they thread through dev genesis correctly, then layer the contract
// bytecode in without touching the address scheme.
func AddQSDStabilityLayer(alloc GenesisAlloc, params QSDPredeployParams) {
	// Reserve the three addresses. Once bytecode is baked in this
	// becomes a populated GenesisAccount{Code: ..., Storage: ...}.
	alloc[ValidatorOracleAddress] = GenesisAccount{Balance: big.NewInt(0)}
	alloc[InverseQRLAddress] = GenesisAccount{Balance: big.NewInt(0)}
	alloc[QSDAddress] = GenesisAccount{Balance: big.NewInt(0)}
}
