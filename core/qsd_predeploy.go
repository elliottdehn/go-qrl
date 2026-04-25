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
	"sort"
	"strings"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/crypto"
)

// Voting parameters baked into the ValidatorOracle bytecode at the
// genesis predeploy. They are immutable in Solidity (constructor
// arguments stored as `immutable` fields), so the only way to read
// them off-chain without ABI calls is to mirror them as Go
// constants. Keep these in lockstep with
// qsd-contracts/script/Predeploy.s.sol.
const (
	OracleVoteStalenessBlocks  = 10
	OracleMinQuorumNumerator   = 2
	OracleMinQuorumDenominator = 3
)

// SubmitVoteSelector is the 4-byte function selector for
//
//	submitVote(uint256,uint256)
//
// invoked on ValidatorOracle. Computed once at init; verified
// against `cast sig` in tests.
var SubmitVoteSelector = crypto.Keccak256([]byte("submitVote(uint256,uint256)"))[:4]

// ValidatorOracle storage layout (from src/ValidatorOracle.sol):
//
//   - slot 0: Ownable._owner
//   - immutables (voteStalenessBlocks, minQuorum*) live in bytecode
//   - slot 1: validators[] length
//   - slot 2: validatorIndex[address] mapping
//   - slot 3: votes[address] mapping (struct Vote { uint128 price; uint64 blockNumber; })
//   - slot 4: cache (packed ViewCache)
//
// These slot constants drive the consensus-side read-paths used to
// gate the free-validator-vote rule.
const (
	validatorOracleValidatorsLengthSlot = 1
	validatorOracleValidatorIndexSlot   = 2
	validatorOracleVotesSlot            = 3
	validatorOracleCacheSlot            = 4
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

// ValidatorIndexStorageSlot returns the storage slot in
// ValidatorOracle that holds validatorIndex[validator]. The mapping
// reserves slot 2; entries live at keccak256(validator || slot 2).
// Non-zero means the address is a registered validator (1-indexed).
func ValidatorIndexStorageSlot(validator common.Address) common.Hash {
	return mappingSlot(validator.Bytes(), validatorOracleValidatorIndexSlot)
}

// ValidatorVoteStorageSlot returns the storage slot in
// ValidatorOracle that holds votes[validator]. The struct packs
// (uint128 price, uint64 blockNumber) into a single slot:
//
//	bytes [16:32] = price       (16 bytes)
//	bytes  [8:16] = blockNumber ( 8 bytes)
//	bytes  [0:8]  = padding     ( 8 bytes)
func ValidatorVoteStorageSlot(validator common.Address) common.Hash {
	return mappingSlot(validator.Bytes(), validatorOracleVotesSlot)
}

// VoteBlockNumberFromSlot extracts the blockNumber field from a
// votes[] mapping slot value, packed per the layout documented on
// ValidatorVoteStorageSlot.
func VoteBlockNumberFromSlot(slot common.Hash) uint64 {
	var n uint64
	for i := 0; i < 8; i++ {
		n = (n << 8) | uint64(slot[8+i])
	}
	return n
}

// mappingSlot computes the storage slot of mapping[key], where the
// mapping is declared at the given top-level slot. Uses the standard
// Solidity layout: keccak256(leftPad(key, 32) || leftPad(slot, 32)).
func mappingSlot(key []byte, slot uint64) common.Hash {
	keyPadded := make([]byte, 32)
	copy(keyPadded[32-len(key):], key)
	slotPadded := make([]byte, 32)
	for i := 0; i < 8; i++ {
		slotPadded[31-i] = byte(slot >> (8 * i))
	}
	return crypto.Keccak256Hash(keyPadded, slotPadded)
}

// OracleStorageReader is the slice of state.StateDB that
// ComputeOraclePrice needs. Lifted to an interface so non-state
// callers (test fakes, future RPC backends) can use the same logic.
type OracleStorageReader interface {
	GetState(addr common.Address, slot common.Hash) common.Hash
}

// ComputeOraclePrice off-chain-recomputes the (medianPrice, healthy)
// pair that the on-chain ValidatorOracle.price() and healthy()
// functions would return at `currentBlock`. Reads raw `validators[]`
// and `votes[]` storage; ignores the per-block cache slot, since
// the cache may be stale by one or more blocks if no recent tx
// poked it.
//
// Logic mirrors src/ValidatorOracle.sol:
//
//   - load validators[]; if empty, return (0, false).
//   - per-validator: load votes[v]. Drop if price == 0 or vote is
//     older than OracleVoteStalenessBlocks blocks.
//   - healthy = (fresh count) * QuorumDenom >= QuorumNum * total.
//   - median of fresh prices (lower of the two middle values for
//     even counts, matching the contract's integer-division avg).
func ComputeOraclePrice(state OracleStorageReader, currentBlock uint64) (*big.Int, bool) {
	lengthSlot := common.Hash{}
	lengthSlot[31] = byte(validatorOracleValidatorsLengthSlot)
	total := state.GetState(ValidatorOracleAddress, lengthSlot).Big().Uint64()
	if total == 0 {
		return new(big.Int), false
	}

	threshold := uint64(0)
	if currentBlock > OracleVoteStalenessBlocks {
		threshold = currentBlock - OracleVoteStalenessBlocks
	}

	// Dynamic-array element 0 lives at keccak256(slot 1); subsequent
	// elements are at consecutive slots.
	arrayBase := crypto.Keccak256Hash(common.LeftPadBytes(
		big.NewInt(validatorOracleValidatorsLengthSlot).Bytes(), 32))
	arrayBaseInt := new(big.Int).SetBytes(arrayBase.Bytes())

	var fresh []*big.Int
	for i := uint64(0); i < total; i++ {
		idxInt := new(big.Int).Add(arrayBaseInt, new(big.Int).SetUint64(i))
		validator := common.BytesToAddress(
			state.GetState(ValidatorOracleAddress, common.BytesToHash(common.LeftPadBytes(idxInt.Bytes(), 32))).Bytes(),
		)
		voteSlot := state.GetState(ValidatorOracleAddress, ValidatorVoteStorageSlot(validator))
		voteBlock := VoteBlockNumberFromSlot(voteSlot)
		if voteBlock < threshold {
			continue
		}
		// price is bytes [16:32] of the packed Vote struct.
		price := new(big.Int).SetBytes(voteSlot[16:32])
		if price.Sign() == 0 {
			continue
		}
		fresh = append(fresh, price)
	}

	healthy := uint64(len(fresh))*OracleMinQuorumDenominator >= OracleMinQuorumNumerator*total
	if len(fresh) == 0 {
		return new(big.Int), healthy
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Cmp(fresh[j]) < 0 })
	mid := len(fresh) / 2
	if len(fresh)%2 == 1 {
		return new(big.Int).Set(fresh[mid]), healthy
	}
	median := new(big.Int).Add(fresh[mid-1], fresh[mid])
	return median.Rsh(median, 1), healthy
}
