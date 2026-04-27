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
	"github.com/theQRL/go-qrl/core/state"
	"github.com/theQRL/go-qrl/crypto"
)

// Voting parameters baked into the ValidatorOracle bytecode at the
// genesis predeploy. They are immutable in Solidity (constructor
// arguments stored as `immutable` fields), so the only way to read
// them off-chain without ABI calls is to mirror them as Go
// constants. Keep these in lockstep with
// contracts/script/Predeploy.s.sol.
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

// ValidatorOracle storage layout (from src/ValidatorOracle.sol).
// Since Ownable is no longer inherited, slot 0 is the validators
// array length:
//
//   - immutables (voteStalenessBlocks, minQuorum*) live in bytecode
//   - slot 0: validators[] length
//   - slot 1: validatorIndex[address] mapping
//   - slot 2: votes[address] mapping (struct Vote { uint128 price; uint64 blockNumber; })
//   - slot 3: cache (packed ViewCache)
//
// These slot constants drive the consensus-side read-paths used to
// gate the free-validator-vote rule and to refresh the txpool's
// paymaster-fairness factor.
const (
	validatorOracleValidatorsLengthSlot = 0
	validatorOracleValidatorIndexSlot   = 1
	validatorOracleVotesSlot            = 2
	validatorOracleCacheSlot            = 3
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
	YieldQSDAddress        = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010004"))
	YieldQSDDeskAddress    = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010005"))
)

// QSDPredeployParams configures the genesis-time deployment of the
// QSD stability layer.
//
// Currently no run-time-configurable knobs: the voting parameters
// (vote-staleness window, quorum fraction) are immutable in the
// ValidatorOracle bytecode and baked into the pre-dumped genesis
// state; the validator set is consensus-driven via per-block system
// calls so there is no owner to seed. The struct is kept for future
// fields and to preserve the API shape.
type QSDPredeployParams struct{}

// DefaultQSDPredeployParams returns the predeploy params for a
// developer network. The signature retains the (owner) parameter
// for backwards source compatibility with callers that still thread
// a "faucet" address through; the value is ignored.
func DefaultQSDPredeployParams(_ common.Address) QSDPredeployParams {
	return QSDPredeployParams{}
}

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

// AddQSDStabilityLayer registers the four QSD-stability-layer
// contracts in the supplied GenesisAlloc, populated from the embedded
// state dump produced by contracts/script/Predeploy.s.sol.
//
// The dump bakes in:
//
//   - deployed bytecode for each contract (with all `immutable`s
//     resolved to the reserved addresses);
//   - ERC-20 _name and _symbol slots for InverseQRL and QSD.
//
// The validator set is consensus-driven (per-block system calls into
// ValidatorOracle.setValidatorSet from the sentinel SYSTEM_CALLER
// address), so there is no owner key or initial validator list to
// seed at genesis. Storage slots not initialized by the constructor
// (e.g. ERC-7201 namespaced ReentrancyGuard._status) default to
// zero, which is functionally equivalent to NOT_ENTERED for fresh
// contracts.
func AddQSDStabilityLayer(alloc GenesisAlloc, _ QSDPredeployParams) {
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
		YieldQSDAddress,
		YieldQSDDeskAddress,
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
}

// InstallQSDPredeploysIfMissing installs the QSD stability-layer
// predeploys (ValidatorOracle, InverseQRL, QSD, PayWithIQRL) into
// `state` if they are not already present. Used to bring the
// predeploys live at the QSD fork activation block on chains that
// did not include them in their genesis alloc.
//
// The function is idempotent: it gates on a single GetCodeSize
// probe of ValidatorOracle, so post-activation blocks pay only one
// storage read before short-circuiting. Dev networks that bake the
// predeploys into genesis hit the fast path on every block.
//
// Source of truth for the bytecode and storage is the same embedded
// JSON dump that AddQSDStabilityLayer reads at genesis time, so the
// fork-activated state is byte-for-byte identical to a fresh
// genesis install.
func InstallQSDPredeploysIfMissing(sdb *state.StateDB) error {
	if sdb.GetCodeSize(ValidatorOracleAddress) > 0 {
		return nil
	}
	dump, err := parseQSDPredeployDump()
	if err != nil {
		return fmt.Errorf("qsd predeploy: parse embedded state: %w", err)
	}
	for _, addr := range []common.Address{
		ValidatorOracleAddress,
		InverseQRLAddress,
		QSDAddress,
		PayWithIQRLAddress,
		YieldQSDAddress,
		YieldQSDDeskAddress,
	} {
		key := "0x" + common.Bytes2Hex(addr.Bytes())
		acct, ok := dump[key]
		if !ok {
			return fmt.Errorf("qsd predeploy: dump missing %s", key)
		}
		ga, err := acct.toGenesisAccount()
		if err != nil {
			return fmt.Errorf("qsd predeploy: %s: %w", key, err)
		}
		sdb.CreateAccount(addr)
		sdb.SetCode(addr, ga.Code)
		for slot, val := range ga.Storage {
			sdb.SetState(addr, slot, val)
		}
		sdb.SetNonce(addr, ga.Nonce)
		// Predeploys hold no native balance.
	}
	return nil
}

// SeedQSDDevValidator mutates the genesis alloc to install
// `validator` as the sole entry in ValidatorOracle's validator set
// AND pre-seeds a fresh vote at `priceUsd1e18`. Used only by the
// dev-genesis path: real networks bring the validator set up via
// the consensus → setValidatorSet system call, which doesn't fire
// in --dev mode (no CL is populating PayloadAttributes.Validators).
//
// The seeded vote is dated at block 2^60 to keep it permanently
// "fresh" relative to the staleness window — the dev faucet doesn't
// need to run qsdfeeder to keep the oracle alive.
func SeedQSDDevValidator(alloc GenesisAlloc, validator common.Address, priceUsd1e18 *big.Int) {
	acct, ok := alloc[ValidatorOracleAddress]
	if !ok {
		panic("SeedQSDDevValidator: ValidatorOracle predeploy not present in alloc")
	}
	if acct.Storage == nil {
		acct.Storage = make(map[common.Hash]common.Hash)
	}

	// validators[] length at slot 0 = 1.
	lengthSlot := common.Hash{}
	lengthSlot[31] = byte(validatorOracleValidatorsLengthSlot)
	acct.Storage[lengthSlot] = common.BigToHash(big.NewInt(1))

	// validators[0] at keccak256(slot 0).
	arrayBase := crypto.Keccak256Hash(common.LeftPadBytes(
		big.NewInt(validatorOracleValidatorsLengthSlot).Bytes(), 32))
	acct.Storage[arrayBase] = common.BytesToHash(common.LeftPadBytes(validator.Bytes(), 32))

	// validatorIndex[validator] = 1.
	acct.Storage[ValidatorIndexStorageSlot(validator)] = common.BigToHash(big.NewInt(1))

	// votes[validator]: pack price (uint128, low 16 bytes of slot)
	// and a far-future blockNumber (uint64, bytes [8:16]) so the
	// vote stays "fresh" forever for staleness purposes.
	var voteSlot common.Hash
	priceBytes := common.LeftPadBytes(priceUsd1e18.Bytes(), 16)
	copy(voteSlot[16:32], priceBytes)
	const farFutureBlock uint64 = 1 << 60
	for i := 0; i < 8; i++ {
		voteSlot[15-i] = byte(farFutureBlock >> (8 * i))
	}
	acct.Storage[ValidatorVoteStorageSlot(validator)] = voteSlot

	alloc[ValidatorOracleAddress] = acct
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

// IsSubmitVoteTx reports whether `tx` is a `submitVote(uint256,uint256)`
// call to the ValidatorOracle predeploy. Used to enforce the
// "all price votes come first" block-ordering rule (see
// core/block_validator.go) and for txpool eviction of stale-target
// votes. The check is purely on (To, calldata selector); the
// caller is responsible for any per-tx authentication.
func IsSubmitVoteTx(tx interface {
	To() *common.Address
	Data() []byte
}) bool {
	to := tx.To()
	if to == nil || *to != ValidatorOracleAddress {
		return false
	}
	data := tx.Data()
	if len(data) < 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if data[i] != SubmitVoteSelector[i] {
			return false
		}
	}
	return true
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
