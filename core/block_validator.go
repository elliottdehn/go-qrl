// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/consensus"
	"github.com/theQRL/go-qrl/core/state"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/params"
	"github.com/theQRL/go-qrl/trie"
)

// BlockValidator is responsible for validating block headers and
// processed state.
//
// BlockValidator implements Validator.
type BlockValidator struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for validating
}

// NewBlockValidator returns a new block validator which is safe for re-use
func NewBlockValidator(config *params.ChainConfig, blockchain *BlockChain, engine consensus.Engine) *BlockValidator {
	validator := &BlockValidator{
		config: config,
		engine: engine,
		bc:     blockchain,
	}
	return validator
}

// ValidateBody verifies the block header's transaction root. The
// headers are assumed to be already validated at this point.
func (v *BlockValidator) ValidateBody(block *types.Block) error {
	// Check whether the block is already imported.
	if v.bc.HasBlockAndState(block.Hash(), block.NumberU64()) {
		return ErrKnownBlock
	}

	// Header validity is known at this point. Here we verify that transactions
	// and withdrawals given in the block body match the header.
	header := block.Header()
	if hash := types.DeriveSha(block.Transactions(), trie.NewStackTrie(nil)); hash != header.TxHash {
		return fmt.Errorf("transaction root hash mismatch (header value %x, calculated %x)", header.TxHash, hash)
	}

	// Withdrawals are present after the Zond fork.
	if header.WithdrawalsHash != nil {
		// Withdrawals list must be present in body after Zond.
		if block.Withdrawals() == nil {
			return errors.New("missing withdrawals in block body")
		}
		if hash := types.DeriveSha(block.Withdrawals(), trie.NewStackTrie(nil)); hash != *header.WithdrawalsHash {
			return fmt.Errorf("withdrawals root hash mismatch (header value %x, calculated %x)", *header.WithdrawalsHash, hash)
		}
	} else if block.Withdrawals() != nil {
		// Withdrawals are not allowed prior to Zond fork
		return errors.New("withdrawals present in block body")
	}

	// ValidatorsHash binds the body's Validators slice to the header.
	// Same nil / empty / populated shape as WithdrawalsHash.
	if header.ValidatorsHash != nil {
		validators := types.Validators(block.Validators())
		if hash := types.DeriveSha(validators, trie.NewStackTrie(nil)); hash != *header.ValidatorsHash {
			return fmt.Errorf("validators root hash mismatch (header value %x, calculated %x)", *header.ValidatorsHash, hash)
		}
	} else if len(block.Validators()) > 0 {
		return errors.New("validators present in block body but not committed to in header")
	}

	// Vote-ordering rule: all price-vote txs come before any
	// non-vote tx. See validateVoteOrdering for the exact predicate.
	if err := validateVoteOrdering(block.Transactions()); err != nil {
		return err
	}

	// Ancestor block must be known.
	if !v.bc.HasBlockAndState(block.ParentHash(), block.NumberU64()-1) {
		if !v.bc.HasBlock(block.ParentHash(), block.NumberU64()-1) {
			return consensus.ErrUnknownAncestor
		}
		return consensus.ErrPrunedAncestor
	}

	// Proposer-vote rule: if the block's coinbase is a registered
	// validator going into this block, the block must contain at
	// least one submitVote tx from the coinbase targeting this
	// block's number. This guarantees that every block produced by
	// an active validator carries at least one fresh price vote,
	// keeping the oracle median moving even when other validators
	// are silent. See ValidateProposerVote for the predicate.
	parent := v.bc.GetHeaderByHash(block.ParentHash())
	if parent != nil {
		parentState, err := v.bc.StateAt(parent.Root)
		if err != nil {
			return fmt.Errorf("proposer vote: load parent state: %w", err)
		}
		isValidator := func(addr common.Address) bool {
			return parentState.GetState(ValidatorOracleAddress, ValidatorIndexStorageSlot(addr)) != (common.Hash{})
		}
		if err := ValidateProposerVote(block.Coinbase(), block.NumberU64(), block.Transactions(), types.MakeSigner(v.config), isValidator); err != nil {
			return err
		}
	}
	return nil
}

// ValidateState validates the various changes that happen after a state transition,
// such as amount of used gas, the receipt roots and the state root itself.
func (v *BlockValidator) ValidateState(block *types.Block, statedb *state.StateDB, receipts types.Receipts, usedGas uint64) error {
	header := block.Header()
	if block.GasUsed() != usedGas {
		return fmt.Errorf("invalid gas used (remote: %d local: %d)", block.GasUsed(), usedGas)
	}
	// Validate the received block's bloom with the one derived from the generated receipts.
	// For valid blocks this should always validate to true.
	rbloom := types.CreateBloom(receipts)
	if rbloom != header.Bloom {
		return fmt.Errorf("invalid bloom (remote: %x  local: %x)", header.Bloom, rbloom)
	}
	// Tre receipt Trie's root (R = (Tr [[H1, R1], ... [Hn, Rn]]))
	receiptSha := types.DeriveSha(receipts, trie.NewStackTrie(nil))
	if receiptSha != header.ReceiptHash {
		return fmt.Errorf("invalid receipt root hash (remote: %x local: %x)", header.ReceiptHash, receiptSha)
	}
	// Validate the state root against the received state root and throw
	// an error if they don't match.
	if root := statedb.IntermediateRoot(true); header.Root != root {
		return fmt.Errorf("invalid merkle root (remote: %x local: %x) dberr: %w", header.Root, root, statedb.Error())
	}
	return nil
}

// CalcGasLimit computes the gas limit of the next block after parent. It aims
// to keep the baseline gas close to the provided target, and increase it towards
// the target if the baseline gas is lower.
func CalcGasLimit(parentGasLimit, desiredLimit uint64) uint64 {
	delta := parentGasLimit/params.GasLimitBoundDivisor - 1
	limit := parentGasLimit
	if desiredLimit < params.MinGasLimit {
		desiredLimit = params.MinGasLimit
	}
	// If we're outside our allowed gas range, we try to hone towards them
	if limit < desiredLimit {
		limit = min(parentGasLimit+delta, desiredLimit)
		return limit
	}
	if limit > desiredLimit {
		limit = max(parentGasLimit-delta, desiredLimit)
	}
	return limit
}

// validateVoteOrdering enforces the "price votes first" rule: in
// any block, every submitVote tx (call to the ValidatorOracle
// predeploy with the submitVote selector) must come before every
// non-vote tx. Returns nil when the ordering holds.
//
// Why: this guarantees the on-chain oracle's per-block median is
// finalized by the time any non-vote tx in the same block runs.
// Paymaster fee splits, QSD.redeem, InverseQRL.mint, etc. all read
// oracle.price() / .healthy() — without the ordering, they could
// see a stale median when they execute and a fresh one when a
// later in-block submitVote moves it.
func validateVoteOrdering(txs []*types.Transaction) error {
	sawNonVote := false
	for i, tx := range txs {
		if IsSubmitVoteTx(tx) {
			if sawNonVote {
				return fmt.Errorf("vote ordering: tx %d is a submitVote but a non-vote tx already preceded it", i)
			}
		} else {
			sawNonVote = true
		}
	}
	return nil
}

// ValidateProposerVote enforces the rule: if the proposer (the
// block's coinbase) is a registered validator (per `isValidator`,
// which the caller wires to parent-state lookup), the txs list must
// contain at least one entry satisfying:
//
//   - sender == proposer
//   - IsSubmitVoteTx(tx) is true
//   - the decoded forBlockNumber argument equals blockNumber
//
// Why: a non-voting active validator could otherwise propose blocks
// indefinitely without contributing to the oracle median. The rule
// guarantees one fresh vote per block from the producer themselves;
// they're free to include other validators' votes too (those are
// regular user-signed txs and don't trigger this check).
//
// Edge cases: a proposer who's not in parent-state validators[] is
// not required to vote. A proposer who's in the parent set but
// removed by setValidatorSet at block start would be required by
// this rule but unable to satisfy it (their submitVote would revert).
// This is treated as a consensus invariant violation — consensus
// must not propose a block from a validator it concurrently removes.
//
// The signature takes primitives rather than *types.Block so the
// miner can call it on a partially-built environment (env.txs +
// env.coinbase + env.header.Number) before the block is assembled.
func ValidateProposerVote(proposer common.Address, blockNumber uint64, txs []*types.Transaction, signer types.Signer, isValidator func(common.Address) bool) error {
	if !isValidator(proposer) {
		return nil
	}

	for _, tx := range txs {
		if !IsSubmitVoteTx(tx) {
			continue
		}
		sender, err := types.Sender(signer, tx)
		if err != nil || sender != proposer {
			continue
		}
		// submitVote(uint256 forBlockNumber, uint256 priceUsd1e18):
		// 4-byte selector + two 32-byte args. The first arg is the
		// forBlockNumber the vote binds to.
		data := tx.Data()
		if len(data) < 4+32 {
			continue
		}
		forBlock := new(big.Int).SetBytes(data[4:36])
		if !forBlock.IsUint64() || forBlock.Uint64() != blockNumber {
			continue
		}
		return nil
	}
	return fmt.Errorf("proposer vote: block %d coinbase %s is a registered validator but block contains no submitVote(%d, _) tx from coinbase",
		blockNumber, proposer.Hex(), blockNumber)
}
