// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

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
//   - PayWithIQRL: paymaster that lets transactions settle fees in
//     iQRL instead of native QRL. Consensus invokes escrow() before
//     execution and settle() after, transferring iQRL from the payer
//     to the block coinbase (and refunding any unused fee).
//
// The addresses are sequential at the start of the application
// reserved namespace, well clear of all current precompile slots.
var (
	ValidatorOracleAddress = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010000"))
	InverseQRLAddress      = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010001"))
	QSDAddress             = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010002"))
	PayWithIQRLAddress     = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010003"))
)

// QSDPredeployParams configures the genesis-time deployment of the
// QSD stability layer.
//
// Only OracleOwner is run-time configurable. The voting parameters
// (vote-staleness window, quorum fraction) are immutable in the
// ValidatorOracle bytecode and are therefore baked into the
// pre-dumped genesis state; changing them requires regenerating
// qsd_predeploy_state.json from script/Predeploy.s.sol.
type QSDPredeployParams struct {
	// OracleOwner becomes the initial owner of ValidatorOracle. It can
	// add and remove validators from the active set. In production
	// this should be a multisig or governance contract.
	OracleOwner common.Address
}

// DefaultQSDPredeployParams returns sensible defaults for a developer
// network.
func DefaultQSDPredeployParams(owner common.Address) QSDPredeployParams {
	return QSDPredeployParams{
		OracleOwner: owner,
	}
}

// predeploySentinelOwner is the placeholder address baked into
// qsd_predeploy_state.json's owner slot. AddQSDStabilityLayer rewrites
// it to the live network's chosen owner.
//
// Must match the OWNER default in script/Predeploy.s.sol.
var predeploySentinelOwner = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000001"))

//go:embed qsd_predeploy_state.json
var qsdPredeployStateJSON []byte

// dumpedAccount mirrors the schema written by Foundry's
// vm.dumpState. Fields are hex strings; we parse them once at
// AddQSDStabilityLayer time.
type dumpedAccount struct {
	Nonce   string            `json:"nonce"`
	Balance string            `json:"balance"`
	Code    string            `json:"code"`
	Storage map[string]string `json:"storage"`
}

// AddQSDStabilityLayer registers the three QSD-stability-layer
// contracts in the supplied GenesisAlloc, populated from the embedded
// state dump produced by qsd-contracts/script/Predeploy.s.sol.
//
// The dump bakes in:
//
//   - deployed bytecode for each contract (with all `immutable`s
//     resolved to the reserved addresses);
//   - ERC-20 _name and _symbol slots for InverseQRL and QSD;
//   - a placeholder owner address in ValidatorOracle's _owner slot,
//     which this function rewrites to params.OracleOwner.
//
// Storage slots not initialized by the constructor (e.g. ERC-7201
// namespaced ReentrancyGuard._status) default to zero, which is
// functionally equivalent to NOT_ENTERED for fresh contracts.
func AddQSDStabilityLayer(alloc GenesisAlloc, params QSDPredeployParams) {
	dump, err := parseQSDPredeployDump()
	if err != nil {
		// The JSON is embedded at build time; an error here means the
		// asset is corrupt or the schema changed without a matching
		// loader update.
		panic(fmt.Errorf("qsd predeploy: parse embedded state: %w", err))
	}

	for _, addr := range []common.Address{
		ValidatorOracleAddress,
		InverseQRLAddress,
		QSDAddress,
		PayWithIQRLAddress,
	} {
		// Foundry's vm.dumpState writes 0x-prefixed lower-case keys;
		// common.Address.Hex() on this fork returns Q-prefixed, so we
		// build the lookup key from the raw bytes instead.
		key := "0x" + common.Bytes2Hex(addr.Bytes())
		acct, ok := dump[key]
		if !ok {
			panic(fmt.Errorf("qsd predeploy: dump missing %s", key))
		}
		ga, err := acct.toGenesisAccount()
		if err != nil {
			panic(fmt.Errorf("qsd predeploy: %s: %w", key, err))
		}
		alloc[addr] = ga
	}

	overrideOracleOwner(alloc, params.OracleOwner)
}

// parseQSDPredeployDump unmarshals the embedded vm.dumpState JSON
// into a lower-cased-address map.
func parseQSDPredeployDump() (map[string]dumpedAccount, error) {
	var raw map[string]dumpedAccount
	if err := json.Unmarshal(qsdPredeployStateJSON, &raw); err != nil {
		return nil, err
	}
	// Normalize keys to lower-case 0x... so lookups by common.Address
	// hash representations are consistent regardless of forge's casing.
	out := make(map[string]dumpedAccount, len(raw))
	for k, v := range raw {
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

func (a dumpedAccount) toGenesisAccount() (GenesisAccount, error) {
	balance, ok := new(big.Int).SetString(strings.TrimPrefix(a.Balance, "0x"), 16)
	if !ok {
		return GenesisAccount{}, fmt.Errorf("balance %q not hex", a.Balance)
	}
	nonce, ok := new(big.Int).SetString(strings.TrimPrefix(a.Nonce, "0x"), 16)
	if !ok {
		return GenesisAccount{}, fmt.Errorf("nonce %q not hex", a.Nonce)
	}
	code := common.FromHex(a.Code)

	storage := make(map[common.Hash]common.Hash, len(a.Storage))
	for k, v := range a.Storage {
		storage[common.HexToHash(k)] = common.HexToHash(v)
	}
	return GenesisAccount{
		Code:    code,
		Storage: storage,
		Balance: balance,
		Nonce:   nonce.Uint64(),
	}, nil
}

// overrideOracleOwner rewrites ValidatorOracle's slot-0 owner from
// the dump's placeholder to the live network's chosen owner. Slot 0
// is OZ Ownable's _owner field; ValidatorOracle has no other base
// contracts with storage above Ownable.
func overrideOracleOwner(alloc GenesisAlloc, owner common.Address) {
	acct, ok := alloc[ValidatorOracleAddress]
	if !ok {
		return
	}
	if acct.Storage == nil {
		acct.Storage = make(map[common.Hash]common.Hash)
	}
	const ownerSlot = "0x0000000000000000000000000000000000000000000000000000000000000000"

	// The address is right-aligned in the 32-byte slot. Build the slot
	// value as 12 zero bytes followed by the 20-byte address.
	var ownerHash common.Hash
	copy(ownerHash[12:], owner.Bytes())
	acct.Storage[common.HexToHash(ownerSlot)] = ownerHash
	alloc[ValidatorOracleAddress] = acct
}

// PredeploySentinelOwner is the placeholder owner address embedded
// in qsd_predeploy_state.json. Exposed for tests that need to
// distinguish "the dump was loaded but the owner override was not
// applied" from "the dump was never loaded".
func PredeploySentinelOwner() common.Address {
	return predeploySentinelOwner
}
