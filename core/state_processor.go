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
	"fmt"
	"math/big"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/consensus"
	"github.com/theQRL/go-qrl/core/state"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/crypto"
	"github.com/theQRL/go-qrl/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *StateProcessor {
	return &StateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// Process processes the state changes according to the QRL rules by running
// the transaction messages using the statedb and applying any rewards to the
// processor (coinbase).
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		usedGas     = new(uint64)
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		gp          = new(GasPool).AddGas(block.GasLimit())
	)
	var (
		context = NewQRVMBlockContext(header, p.bc, nil)
		vmenv   = vm.NewQRVM(context, vm.TxContext{}, statedb, p.config, cfg)
		signer  = types.MakeSigner(p.config)
	)

	// At the QSD fork activation block (and every block thereafter,
	// idempotently), make sure the four stability-layer predeploys
	// are installed. Networks that bake them into genesis hit the
	// fast path on the very first probe; networks that activate the
	// fork mid-chain pay one full install on the activation block
	// and the fast path forever after.
	if p.config.IsQSD(header.Time) {
		if err := InstallQSDPredeploysIfMissing(statedb); err != nil {
			return nil, nil, 0, fmt.Errorf("qsd predeploy install: %w", err)
		}
	}

	// Mirror the chain's PoS validator set into the ValidatorOracle
	// predeploy via a system call BEFORE executing transactions, so
	// any submitVote / free-vote machinery in this block sees the
	// fresh set. Gated on the QSD fork: pre-fork blocks have no
	// ValidatorOracle to mirror into. No-op when the body carries
	// no validator list (e.g. blocks built before the engine API
	// was extended to populate it).
	if p.config.IsQSD(header.Time) {
		if vs := block.Validators(); len(vs) > 0 {
			if err := ProcessSetValidatorSet(vmenv, vs); err != nil {
				return nil, nil, 0, fmt.Errorf("setValidatorSet system call: %w", err)
			}
		}
	}

	// Iterate over and process the individual transactions. The
	// vote-ordering rule guarantees all submitVote txs come before
	// any non-vote tx, so we can detect the phase transition by the
	// first tx that fails IsSubmitVoteTx. At that boundary we fire
	// the pokeCache system call: it rebuilds ValidatorOracle's
	// per-block median over every vote that just landed, so the
	// non-vote txs about to run read a fresh cache.
	pokedCache := !p.config.IsQSD(header.Time) // pre-fork: nothing to poke
	for i, tx := range block.Transactions() {
		if !pokedCache && !IsSubmitVoteTx(tx) {
			if err := ProcessPokeCache(vmenv); err != nil {
				return nil, nil, 0, fmt.Errorf("pokeCache system call: %w", err)
			}
			pokedCache = true
		}
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		statedb.SetTxContext(tx.Hash(), i)
		receipt, err := applyTransaction(msg, gp, statedb, blockNumber, blockHash, tx, usedGas, vmenv)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
	}
	// Edge case: the block was all-votes (or empty). Fire pokeCache
	// once at the end so the cache is fresh for any reader at the
	// block boundary (e.g. RPC eth_call against `latest`).
	if !pokedCache {
		if err := ProcessPokeCache(vmenv); err != nil {
			return nil, nil, 0, fmt.Errorf("pokeCache system call: %w", err)
		}
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.engine.Finalize(p.bc, header, statedb, block.Body())

	return receipts, allLogs, *usedGas, nil
}

func applyTransaction(msg *Message, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, tx *types.Transaction, usedGas *uint64, qrvm *vm.QRVM) (*types.Receipt, error) {
	// Create a new context to be used in the QRVM environment.
	txContext := NewQRVMTxContext(msg)
	qrvm.Reset(txContext, statedb)

	// Apply the transaction to the current state (included in the env).
	result, err := ApplyMessage(qrvm, msg, gp)
	if err != nil {
		return nil, err
	}

	// Update the state with pending changes.
	var root []byte
	statedb.Finalise(true)
	*usedGas += result.UsedGas

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: *usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	// If the transaction created a contract, store the creation address in the receipt.
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(qrvm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	return receipt, err
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(config *params.ChainConfig, bc ChainContext, author *common.Address, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64, cfg vm.Config) (*types.Receipt, error) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(config), header.BaseFee)
	if err != nil {
		return nil, err
	}
	// Create a new context to be used in the QRVM environment
	blockContext := NewQRVMBlockContext(header, bc, author)
	vmenv := vm.NewQRVM(blockContext, vm.TxContext{}, statedb, config, cfg)
	return applyTransaction(msg, gp, statedb, header.Number, header.Hash(), tx, usedGas, vmenv)
}
