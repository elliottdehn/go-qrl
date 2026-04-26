// Copyright 2014 The go-ethereum Authors
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
	"bytes"
	"fmt"
	"math"
	"math/big"

	"github.com/theQRL/go-qrl/common"
	cmath "github.com/theQRL/go-qrl/common/math"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/crypto"
	"github.com/theQRL/go-qrl/params"
)

// ExecutionResult includes all output after executing given qrvm
// message no matter the execution itself is successful or not.
type ExecutionResult struct {
	UsedGas    uint64 // Total used gas but include the refunded gas
	Err        error  // Any error encountered during the execution(listed in core/vm/errors.go)
	ReturnData []byte // Returned data from qrvm(function result or data supplied with revert opcode)
}

// Unwrap returns the internal qrvm error which allows us for further
// analysis outside.
func (result *ExecutionResult) Unwrap() error {
	return result.Err
}

// Failed returns the indicator whether the execution is successful or not
func (result *ExecutionResult) Failed() bool { return result.Err != nil }

// Return is a helper function to help caller distinguish between revert reason
// and function return. Return returns the data after execution if no error occurs.
func (result *ExecutionResult) Return() []byte {
	if result.Err != nil {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// Revert returns the concrete revert reason if the execution is aborted by `REVERT`
// opcode. Note the reason can be nil if no data supplied with revert opcode.
func (result *ExecutionResult) Revert() []byte {
	if result.Err != vm.ErrExecutionReverted {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func IntrinsicGas(data []byte, accessList types.AccessList, isContractCreation bool) (uint64, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if isContractCreation {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	dataLen := uint64(len(data))
	// Bump the required gas by the amount of transactional data
	if dataLen > 0 {
		// Zero and non-zero bytes are priced differently
		var nz uint64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		// Make sure we don't exceed uint64 for all data combinations
		nonZeroGas := params.TxDataNonZeroGasEIP2028
		if (math.MaxUint64-gas)/nonZeroGas < nz {
			return 0, ErrGasUintOverflow
		}
		gas += nz * nonZeroGas

		z := dataLen - nz
		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return 0, ErrGasUintOverflow
		}
		gas += z * params.TxDataZeroGas

		if isContractCreation {
			lenWords := toWordSize(dataLen)
			if (math.MaxUint64-gas)/params.InitCodeWordGas < lenWords {
				return 0, ErrGasUintOverflow
			}
			gas += lenWords * params.InitCodeWordGas
		}
	}
	if accessList != nil {
		gas += uint64(len(accessList)) * params.TxAccessListAddressGas
		gas += uint64(accessList.StorageKeys()) * params.TxAccessListStorageKeyGas
	}
	return gas, nil
}

// toWordSize returns the ceiled word size required for init code payment calculation.
func toWordSize(size uint64) uint64 {
	if size > math.MaxUint64-31 {
		return math.MaxUint64/32 + 1
	}

	return (size + 31) / 32
}

// A Message contains the data derived from a single transaction that is relevant to state
// processing.
type Message struct {
	To         *common.Address
	From       common.Address
	Nonce      uint64
	Value      *big.Int
	GasLimit   uint64
	GasPrice   *big.Int
	GasFeeCap  *big.Int
	GasTipCap  *big.Int
	Data       []byte
	AccessList types.AccessList

	// Paymaster, when non-nil, identifies a paymaster contract that
	// settles this message's fees on behalf of the sender. The
	// state-transition engine system-calls Paymaster.escrow() before
	// execution and Paymaster.settle() after, in lieu of the standard
	// native-balance debit and tip transfer to the coinbase.
	Paymaster *common.Address

	// When SkipAccountChecks is true, the message nonce is not checked against the
	// account nonce in state. It also disables checking that the sender is an EOA.
	// This field will be set to true for operations like RPC qrl_call.
	SkipAccountChecks bool
}

// TransactionToMessage converts a transaction into a Message.
func TransactionToMessage(tx *types.Transaction, s types.Signer, baseFee *big.Int) (*Message, error) {
	msg := &Message{
		Nonce:             tx.Nonce(),
		GasLimit:          tx.Gas(),
		GasPrice:          new(big.Int).Set(tx.GasPrice()),
		GasFeeCap:         new(big.Int).Set(tx.GasFeeCap()),
		GasTipCap:         new(big.Int).Set(tx.GasTipCap()),
		To:                tx.To(),
		Value:             tx.Value(),
		Data:              tx.Data(),
		AccessList:        tx.AccessList(),
		Paymaster:         tx.Paymaster(),
		SkipAccountChecks: false,
	}
	// If baseFee provided, set gasPrice to effectiveGasPrice.
	if baseFee != nil {
		msg.GasPrice = cmath.BigMin(msg.GasPrice.Add(msg.GasTipCap, baseFee), msg.GasFeeCap)
	}
	var err error
	msg.From, err = types.Sender(s, tx)
	return msg, err
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any QRVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(qrvm *vm.QRVM, msg *Message, gp *GasPool) (*ExecutionResult, error) {
	return NewStateTransition(qrvm, msg, gp).TransitionDb()
}

// StateTransition represents a state transition.
//
// == The State Transitioning Model
//
// A state transition is a change made when a transaction is applied to the current world
// state. The state transitioning model does all the necessary work to work out a valid new
// state root.
//
//  1. Nonce handling
//  2. Pre pay gas
//  3. Create a new state object if the recipient is nil
//  4. Value transfer
//
// == If contract creation ==
//
//	4a. Attempt to run transaction data
//	4b. If valid, use result as code for the new state object
//
// == end ==
//
//  5. Run Script section
//  6. Derive new state root
type StateTransition struct {
	gp           *GasPool
	msg          *Message
	gasRemaining uint64
	initialGas   uint64
	state        vm.StateDB
	qrvm         *vm.QRVM
}

// NewStateTransition initialises and returns a new state transition object.
func NewStateTransition(qrvm *vm.QRVM, msg *Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:    gp,
		qrvm:  qrvm,
		msg:   msg,
		state: qrvm.StateDB,
	}
}

// to returns the recipient of the message.
func (st *StateTransition) to() common.Address {
	if st.msg == nil || st.msg.To == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To
}

func (st *StateTransition) buyGas() error {
	mgval := new(big.Int).SetUint64(st.msg.GasLimit)
	mgval = mgval.Mul(mgval, st.msg.GasPrice)

	if err := st.gp.SubGas(st.msg.GasLimit); err != nil {
		return err
	}
	st.gasRemaining += st.msg.GasLimit
	st.initialGas = st.msg.GasLimit

	// Paymaster path: the actual escrow system call happens AFTER
	// state.Prepare() in TransitionDb (the EVM call inside escrow
	// would otherwise warm the access list before Prepare sets it
	// up, leaving the journal in an inconsistent state on revert).
	// Here we just verify the caller has enough native balance for
	// the value-transfer leg (gas itself is paid in the paymaster's
	// chosen asset, validated separately at escrow time).
	if st.msg.Paymaster != nil {
		if have, want := st.state.GetBalance(st.msg.From), st.msg.Value; have.Cmp(want) < 0 {
			return fmt.Errorf("%w: address %v have %v want %v (value transfer)",
				ErrInsufficientFunds, st.msg.From.Hex(), have, want)
		}
		return nil
	}

	balanceCheck := new(big.Int).Set(mgval)
	if st.msg.GasFeeCap != nil {
		balanceCheck.SetUint64(st.msg.GasLimit)
		balanceCheck = balanceCheck.Mul(balanceCheck, st.msg.GasFeeCap)
		balanceCheck.Add(balanceCheck, st.msg.Value)
	}
	if have, want := st.state.GetBalance(st.msg.From), balanceCheck; have.Cmp(want) < 0 {
		return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From.Hex(), have, want)
	}
	st.state.SubBalance(st.msg.From, mgval)
	return nil
}

func (st *StateTransition) preCheck() error {
	// Only check transactions that are not fake
	msg := st.msg
	if !msg.SkipAccountChecks {
		// Make sure this transaction's nonce is correct.
		stNonce := st.state.GetNonce(msg.From)
		if msgNonce := msg.Nonce; stNonce < msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooHigh,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce > msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooLow,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce+1 < stNonce {
			return fmt.Errorf("%w: address %v, nonce: %d", ErrNonceMax,
				msg.From.Hex(), stNonce)
		}
		// Make sure the sender is an EOA
		codeHash := st.state.GetCodeHash(msg.From)
		if codeHash != (common.Hash{}) && codeHash != types.EmptyCodeHash {
			return fmt.Errorf("%w: address %v, codehash: %s", ErrSenderNoEOA,
				msg.From.Hex(), codeHash)
		}
	}

	// Make sure that transaction gasFeeCap is greater than the baseFee (post london)
	// Skip the checks if gas fields are zero and baseFee was explicitly disabled (eth_call)
	if !st.qrvm.Config.NoBaseFee || msg.GasFeeCap.BitLen() > 0 || msg.GasTipCap.BitLen() > 0 {
		if l := msg.GasFeeCap.BitLen(); l > 256 {
			return fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
				msg.From.Hex(), l)
		}
		if l := msg.GasTipCap.BitLen(); l > 256 {
			return fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", ErrTipVeryHigh,
				msg.From.Hex(), l)
		}
		if msg.GasFeeCap.Cmp(msg.GasTipCap) < 0 {
			return fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", ErrTipAboveFeeCap,
				msg.From.Hex(), msg.GasTipCap, msg.GasFeeCap)
		}
		// This will panic if baseFee is nil, but basefee presence is verified
		// as part of header validation.
		//
		// Paymaster txs are exempt: their GasFeeCap is denominated in
		// the paymaster's chosen asset (iQRL for PayWithIQRL), not in
		// QRL, so a direct comparison against the QRL base fee is a
		// unit error. The paymaster path computes baseFeeIQRL via the
		// oracle and floors the tip at zero (post-execution split
		// can't put the validator into a negative position even when
		// the converted base fee exceeds the signed cap).
		if msg.Paymaster == nil && msg.GasFeeCap.Cmp(st.qrvm.Context.BaseFee) < 0 {
			return fmt.Errorf("%w: address %v, maxFeePerGas: %s baseFee: %s", ErrFeeCapTooLow,
				msg.From.Hex(), msg.GasFeeCap, st.qrvm.Context.BaseFee)
		}
	}

	return st.buyGas()
}

// TransitionDb will transition the state by applying the current message and
// returning the qrvm execution result with following fields.
//
//   - used gas: total gas used (including gas being refunded)
//   - returndata: the returned data from qrvm
//   - concrete execution error: various QRVM errors which abort the execution, e.g.
//     ErrOutOfGas, ErrExecutionReverted
//
// However if any consensus issue encountered, return the error directly with
// nil qrvm execution result.
func (st *StateTransition) TransitionDb() (*ExecutionResult, error) {
	// First check this message satisfies all consensus rules before
	// applying the message. The rules include these clauses
	//
	// 1. the nonce of the message caller is correct
	// 2. caller has enough balance to cover transaction fee(gaslimit * gasprice)
	// 3. the amount of gas required is available in the block
	// 4. the purchased gas is enough to cover intrinsic usage
	// 5. there is no overflow when calculating intrinsic gas
	// 6. caller has enough balance to cover asset transfer for **topmost** call

	// Check clauses 1-3, buy gas if everything is correct
	if err := st.preCheck(); err != nil {
		return nil, err
	}

	if tracer := st.qrvm.Config.Tracer; tracer != nil {
		tracer.CaptureTxStart(st.initialGas)
		defer func() {
			tracer.CaptureTxEnd(st.gasRemaining)
		}()
	}

	var (
		msg              = st.msg
		sender           = vm.AccountRef(msg.From)
		rules            = st.qrvm.ChainConfig().Rules(st.qrvm.Context.BlockNumber, st.qrvm.Context.Time)
		contractCreation = msg.To == nil
	)

	// Check clauses 4-5, subtract intrinsic gas if everything is correct
	gas, err := IntrinsicGas(msg.Data, msg.AccessList, contractCreation)
	if err != nil {
		return nil, err
	}
	if st.gasRemaining < gas {
		return nil, fmt.Errorf("%w: have %d, want %d", ErrIntrinsicGas, st.gasRemaining, gas)
	}
	st.gasRemaining -= gas

	// Check clause 6
	if msg.Value.Sign() > 0 && !st.qrvm.Context.CanTransfer(st.state, msg.From, msg.Value) {
		return nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From.Hex())
	}

	// Check whether the init code size has been exceeded.
	if contractCreation && len(msg.Data) > params.MaxInitCodeSize {
		return nil, fmt.Errorf("%w: code size %v limit %v", ErrMaxInitCodeSizeExceeded, len(msg.Data), params.MaxInitCodeSize)
	}

	// Execute the preparatory steps for state transition which includes:
	// - prepare accessList
	st.state.Prepare(rules, msg.From, st.qrvm.Context.Coinbase, msg.To, vm.ActivePrecompiles(rules), msg.AccessList)

	// Paymaster escrow: deferred from buyGas() to here so the EVM
	// call's access-list warming happens with the access list
	// already initialized by Prepare. mgval (gasLimit*gasPrice) is
	// the maxFee the paymaster pulls from the sender.
	if msg.Paymaster != nil {
		mgval := new(big.Int).Mul(new(big.Int).SetUint64(st.initialGas), msg.GasPrice)
		if err := st.paymasterEscrow(mgval); err != nil {
			return nil, err
		}
	}

	// Snapshot the free-vote eligibility BEFORE execution: the
	// votes[from].blockNumber slot will be overwritten by a
	// successful submitVote(), so any post-execution read would
	// always show "already voted this block" and grant nothing free.
	freeVoteCandidate := st.isFreeValidatorVoteTx()

	var (
		ret   []byte
		vmerr error // vm errors do not effect consensus and are therefore not assigned to err
	)
	if contractCreation {
		ret, _, st.gasRemaining, vmerr = st.qrvm.Create(sender, msg.Data, st.gasRemaining, msg.Value)
	} else {
		// Increment the nonce for the next transaction
		st.state.SetNonce(msg.From, st.state.GetNonce(sender.Address())+1)
		ret, st.gasRemaining, vmerr = st.qrvm.Call(sender, st.to(), msg.Data, st.gasRemaining, msg.Value)
	}

	// After EIP-3529: refunds are capped to gasUsed / 5. Apply the
	// refund counter to gasRemaining now; the actual return-of-funds
	// path forks below based on whether a paymaster is in play.
	st.applyRefundCounter(params.RefundQuotientEIP3529)

	if msg.Paymaster != nil {
		// Paymaster path: settle via the paymaster contract. Split
		// the user's fee into a burn portion (the EIP-1559 base
		// fee, converted from QRL to iQRL terms via the oracle)
		// and a tip portion (the rest, paid to coinbase). This
		// makes paymaster txs symmetric to native EIP-1559 txs:
		// both deflate their respective asset by gasUsed*baseFee
		// and tip the validator the remainder.
		baseFeeIQRL := paymasterBaseFeeIQRL(st.state, st.qrvm.Context.BaseFee, st.qrvm.Context.BlockNumber.Uint64())

		// Cap the burn at the user's signed gasFeeCap: if the
		// oracle-derived baseFeeIQRL has drifted above what the
		// user committed to, the burn just absorbs the entire
		// signed cap (no tip). Total fee never exceeds
		// gasUsed * gasFeeCap ≤ maxFee = gasLimit * gasFeeCap.
		burnPerGas := new(big.Int).Set(baseFeeIQRL)
		if burnPerGas.Cmp(msg.GasFeeCap) > 0 {
			burnPerGas.Set(msg.GasFeeCap)
		}
		burnAmount := new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), burnPerGas)

		// effGasPrice = min(GasFeeCap, baseFeeIQRL + GasTipCap).
		effGasPrice := new(big.Int).Add(baseFeeIQRL, msg.GasTipCap)
		if effGasPrice.Cmp(msg.GasFeeCap) > 0 {
			effGasPrice.Set(msg.GasFeeCap)
		}
		// tipPerGas = effGasPrice - burnPerGas, floored at zero.
		tipPerGas := new(big.Int).Sub(effGasPrice, burnPerGas)
		if tipPerGas.Sign() < 0 {
			tipPerGas.SetInt64(0)
		}
		tipAmount := new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), tipPerGas)

		maxFee := new(big.Int).Mul(new(big.Int).SetUint64(st.initialGas), msg.GasPrice)
		// Defensive cap: burn + tip must not exceed maxFee. The
		// per-gas caps above guarantee this; assert for safety.
		if new(big.Int).Add(burnAmount, tipAmount).Cmp(maxFee) > 0 {
			return nil, fmt.Errorf("paymaster settle: burn+tip %s exceeds maxFee %s",
				new(big.Int).Add(burnAmount, tipAmount), maxFee)
		}

		if err := st.paymasterSettle(maxFee, burnAmount, tipAmount); err != nil {
			// Settle should be infallible — escrow guarantees the
			// contract holds maxFee. If we land here it's a chain
			// invariant violation.
			return nil, fmt.Errorf("paymaster settle (post-exec): %w", err)
		}
		// Return remaining gas to the block gas counter so it is
		// available for the next transaction.
		st.gp.AddGas(st.gasRemaining)
		return &ExecutionResult{
			UsedGas:    st.gasUsed(),
			Err:        vmerr,
			ReturnData: ret,
		}, nil
	}

	// Free-validator-vote path: a successful submitVote() to the
	// ValidatorOracle from a registered validator who hasn't yet
	// voted in this block costs the validator zero — the entire
	// pre-charged buyGas debit is refunded and no tip is paid. The
	// candidate flag was captured pre-execution; we only honour it
	// if the EVM call also succeeded (no revert).
	//
	// Free votes also do not count against the block gas limit.
	// Return ALL initialGas to the gas pool (not just gasRemaining)
	// and report UsedGas = 0 so the state processor's running
	// totals and the receipt's GasUsed both stay flat. Without this,
	// a network with N active validators would burn ~N * voteGas of
	// the block's gas-limit budget every block before any user
	// transaction got a chance to land.
	if vmerr == nil && freeVoteCandidate {
		full := new(big.Int).Mul(new(big.Int).SetUint64(st.initialGas), msg.GasPrice)
		st.state.AddBalance(msg.From, full)
		st.gp.AddGas(st.initialGas)
		return &ExecutionResult{
			UsedGas:    0,
			Err:        vmerr,
			ReturnData: ret,
		}, nil
	}

	// Native path: refund unused gas to sender + tip the coinbase.
	st.refundNativeGas()
	effectiveTip := cmath.BigMin(msg.GasTipCap, new(big.Int).Sub(msg.GasFeeCap, st.qrvm.Context.BaseFee))

	if st.qrvm.Config.NoBaseFee && msg.GasFeeCap.Sign() == 0 && msg.GasTipCap.Sign() == 0 {
		// Skip fee payment when NoBaseFee is set and the fee fields
		// are 0. This avoids a negative effectiveTip being applied to
		// the coinbase when simulating calls.
	} else {
		fee := new(big.Int).SetUint64(st.gasUsed())
		fee.Mul(fee, effectiveTip)
		st.state.AddBalance(st.qrvm.Context.Coinbase, fee)
	}

	return &ExecutionResult{
		UsedGas:    st.gasUsed(),
		Err:        vmerr,
		ReturnData: ret,
	}, nil
}

// isFreeValidatorVoteTx reports whether the current message qualifies
// for free-tx treatment under the validator-vote rule. The four
// conditions, evaluated against the current chain state:
//
//  1. Not a paymaster tx (paymaster path takes precedence).
//  2. Target is the ValidatorOracle predeploy.
//  3. Calldata begins with the submitVote(uint256,uint256) selector.
//  4. Sender is a registered validator (validatorIndex non-zero) AND
//     has not already voted in the current block.
//
// The caller must additionally verify that the EVM execution itself
// did not revert before granting free status; a reverted submitVote
// (e.g. wrong forBlockNumber) pays normal gas as anti-spam.
func (st *StateTransition) isFreeValidatorVoteTx() bool {
	msg := st.msg
	if msg.Paymaster != nil {
		return false
	}
	if msg.To == nil || *msg.To != ValidatorOracleAddress {
		return false
	}
	if len(msg.Data) < 4 || !bytes.Equal(msg.Data[:4], SubmitVoteSelector) {
		return false
	}
	idxSlot := ValidatorIndexStorageSlot(msg.From)
	if st.state.GetState(ValidatorOracleAddress, idxSlot).Big().Sign() == 0 {
		return false
	}
	voteSlot := ValidatorVoteStorageSlot(msg.From)
	prevBlock := VoteBlockNumberFromSlot(st.state.GetState(ValidatorOracleAddress, voteSlot))
	if prevBlock == st.qrvm.Context.BlockNumber.Uint64() {
		return false
	}
	return true
}

// applyRefundCounter folds the EVM-accumulated refund counter
// (SSTORE refunds, capped at gasUsed/refundQuotient by EIP-3529)
// into gasRemaining. Split out from refundNativeGas because the
// paymaster path also needs the counter applied — settlement uses
// gasUsed = initialGas - gasRemaining, so under-applying the
// refund would over-bill the user.
func (st *StateTransition) applyRefundCounter(refundQuotient uint64) {
	refund := min(st.gasUsed()/refundQuotient, st.state.GetRefund())
	st.gasRemaining += refund
}

// refundNativeGas returns unused gas to the sender's native QRL
// balance. Only used on the non-paymaster path; paymaster txs settle
// via the paymaster contract instead.
func (st *StateTransition) refundNativeGas() {
	remaining := new(big.Int).Mul(new(big.Int).SetUint64(st.gasRemaining), st.msg.GasPrice)
	st.state.AddBalance(st.msg.From, remaining)
	st.gp.AddGas(st.gasRemaining)
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *StateTransition) gasUsed() uint64 {
	return st.initialGas - st.gasRemaining
}

// ----------------------------------------------------------------------------
// Paymaster system calls.
//
// When a transaction designates a Paymaster, the standard buyGas /
// refundGas / coinbase-tip flow is replaced with two QRVM calls:
//
//   1. paymasterEscrow() before execution — invokes
//      paymaster.escrow(sender, maxFee). The paymaster contract
//      pulls maxFee of whatever asset it accepts (typically iQRL)
//      from the sender into its own balance.
//
//   2. paymasterSettle() after execution — invokes
//      paymaster.settle(sender, maxFee, actualFee). The paymaster
//      forwards actualFee to block.coinbase and refunds any unused
//      portion to the sender.
//
// Both calls are issued from a fixed sentinel sender address (the
// "system caller") so the paymaster contract can verify that it is
// being driven by consensus rather than a user-issued tx. They use a
// generous fixed gas budget that does not draw from the tx's gas
// pool.
//
// Allowlist: only a curated set of paymasters is accepted. The chain
// hard-codes core.PayWithIQRLAddress as the only supported paymaster
// at this time; pre-execution rejection of unknown paymasters is the
// txpool's job (see TxPool admission), but state-transition validates
// here too to defend against block-builder bugs.
// ----------------------------------------------------------------------------

// SystemCallerAddress is the sentinel sender used for system calls
// to predeployed contracts (paymaster escrow/settle, beacon-root
// updates if added later, etc.). Mirrors the convention used by
// EIP-4788 and is hard-coded inside PayWithIQRL.
var SystemCallerAddress = common.BytesToAddress(common.FromHex("0xfffffffffffffffffffffffffffffffffffffffe"))

// PaymasterSystemGasLimit is the gas budget allocated to each
// paymaster system call. Generous enough for an ERC-20 transferFrom
// + a couple of refund transfers, but bounded so a buggy paymaster
// can't burn unbounded gas.
const PaymasterSystemGasLimit uint64 = 1_000_000

var (
	// keccak256("escrow(address,uint256)")[:4]
	paymasterEscrowSelector = crypto.Keccak256([]byte("escrow(address,uint256)"))[:4]
	// keccak256("settle(address,uint256,uint256,uint256)")[:4]
	//
	// settle takes (payer, maxFee, burnAmount, tipAmount). The
	// burn/tip split is the EIP-1559-equivalent base-fee burn vs
	// validator tip, computed by consensus from the QRL base fee
	// converted to iQRL terms via the oracle.
	paymasterSettleSelector = crypto.Keccak256([]byte("settle(address,uint256,uint256,uint256)"))[:4]
)

// IsAllowedPaymaster reports whether the given address is on the
// chain's paymaster allowlist. Currently a single-entry list; future
// paymasters (e.g. one that accepts QSD) would extend this.
func IsAllowedPaymaster(addr common.Address) bool {
	return addr == PayWithIQRLAddress
}

// paymasterEscrow invokes paymaster.escrow(sender, maxFee). Called
// from buyGas() in lieu of the native-balance debit when the message
// designates a paymaster.
func (st *StateTransition) paymasterEscrow(maxFee *big.Int) error {
	if !IsAllowedPaymaster(*st.msg.Paymaster) {
		return fmt.Errorf("paymaster %s not on chain allowlist", st.msg.Paymaster.Hex())
	}
	calldata := encodeAddressUint256(paymasterEscrowSelector, st.msg.From, maxFee)
	if err := st.systemCall(*st.msg.Paymaster, calldata); err != nil {
		return fmt.Errorf("paymaster escrow: %w", err)
	}
	return nil
}

// paymasterSettle invokes
// paymaster.settle(sender, maxFee, burnAmount, tipAmount). Called
// from TransitionDb after execution when the message designated a
// paymaster.
//
// burnAmount + tipAmount must equal gasUsed * effectiveGasPrice in
// the paymaster's native fee asset (iQRL for PayWithIQRL); the
// caller is responsible for that math.
func (st *StateTransition) paymasterSettle(maxFee, burnAmount, tipAmount *big.Int) error {
	calldata := encodeAddressUint256Uint256Uint256(
		paymasterSettleSelector, st.msg.From, maxFee, burnAmount, tipAmount,
	)
	if err := st.systemCall(*st.msg.Paymaster, calldata); err != nil {
		return fmt.Errorf("paymaster settle: %w", err)
	}
	return nil
}

// systemCall issues a synthetic call from SystemCallerAddress to
// `to` with the given calldata, using PaymasterSystemGasLimit out of
// band (no draw from the tx gas pool). Returns an error if the call
// reverted.
func (st *StateTransition) systemCall(to common.Address, calldata []byte) error {
	sender := vm.AccountRef(SystemCallerAddress)
	_, _, vmerr := st.qrvm.Call(sender, to, calldata, PaymasterSystemGasLimit, common.Big0)
	return vmerr
}

// paymasterBaseFeeIQRL converts the chain's per-gas QRL base fee
// to its iQRL equivalent using the oracle's current median price.
//
// 1 QRL = p^2 iQRL (since 1 iQRL = 1/p^2 QRL by construction). With
// p stored in 1e18 fixed-point, the conversion is:
//
//	baseFeeIQRL = baseFeeQRL * p_scaled^2 / 1e36
//
// Returns 0 if the oracle is unhealthy or has no fresh votes — that
// path leaves the entire fee as tip (no burn) until quorum is
// restored, rather than risking a wildly wrong baseFee.
func paymasterBaseFeeIQRL(state vm.StateDB, baseFeeQRL *big.Int, blockNumber uint64) *big.Int {
	if baseFeeQRL == nil || baseFeeQRL.Sign() == 0 {
		return new(big.Int)
	}
	p, healthy := ComputeOraclePrice(state, blockNumber)
	if !healthy || p.Sign() == 0 {
		return new(big.Int)
	}
	// baseFeeIQRL = baseFeeQRL * p^2 / 1e36
	pSquared := new(big.Int).Mul(p, p)
	out := new(big.Int).Mul(baseFeeQRL, pSquared)
	return out.Div(out, paymasterPriceScaleSquared)
}

// paymasterPriceScaleSquared is 1e36 = (1e18)^2, the factor that
// cancels the 1e18 fixed-point scaling on the squared oracle price.
var paymasterPriceScaleSquared = func() *big.Int {
	one := big.NewInt(1_000_000_000_000_000_000)
	return new(big.Int).Mul(one, one)
}()

// encodeAddressUint256 builds calldata for `f(address, uint256)`:
// 4-byte selector || 32-byte address (left-padded) || 32-byte uint256.
func encodeAddressUint256(selector []byte, addr common.Address, n *big.Int) []byte {
	out := make([]byte, 0, 4+32+32)
	out = append(out, selector...)
	var padded common.Hash
	copy(padded[12:], addr.Bytes())
	out = append(out, padded.Bytes()...)
	out = append(out, common.LeftPadBytes(n.Bytes(), 32)...)
	return out
}

// encodeAddressUint256Uint256 builds calldata for `f(address, uint256, uint256)`:
// 4-byte selector || 32-byte address || 32-byte n || 32-byte m.
func encodeAddressUint256Uint256(selector []byte, addr common.Address, n, m *big.Int) []byte {
	out := encodeAddressUint256(selector, addr, n)
	out = append(out, common.LeftPadBytes(m.Bytes(), 32)...)
	return out
}

// encodeAddressUint256Uint256Uint256 builds calldata for
// `f(address, uint256, uint256, uint256)`:
// 4-byte selector || 32-byte address || 32-byte n || 32-byte m || 32-byte k.
func encodeAddressUint256Uint256Uint256(selector []byte, addr common.Address, n, m, k *big.Int) []byte {
	out := encodeAddressUint256Uint256(selector, addr, n, m)
	out = append(out, common.LeftPadBytes(k.Bytes(), 32)...)
	return out
}
