// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package types

import (
	"bytes"
	"math/big"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/crypto/pqcrypto"
	"github.com/theQRL/go-qrl/rlp"
)

// PaymasterDynamicFeeTx is an EIP-1559 transaction whose fees are
// settled by a paymaster contract instead of debited directly from
// the sender. The paymaster's escrow() entrypoint is invoked before
// execution and settle() after; the contract enforces whatever fee
// asset and accounting it implements (e.g. PayWithIQRL: pull iQRL
// from sender, forward to block.coinbase).
//
// The wire schema mirrors DynamicFeeTx with one extra field:
// Paymaster (the contract that handles fees). Type byte distinguishes
// the two on the wire (0x04 vs 0x02), so consensus, txpool, and
// signer logic can branch cleanly.
type PaymasterDynamicFeeTx struct {
	ChainID    *big.Int
	Nonce      uint64
	GasTipCap  *big.Int
	GasFeeCap  *big.Int
	Gas        uint64
	To         *common.Address `rlp:"nil"`
	Value      *big.Int
	Data       []byte
	AccessList AccessList

	// Paymaster contract that will be system-called to escrow + settle
	// this tx's fees. Must be non-nil for this tx type; the consensus
	// engine maintains an allowlist (currently: just PayWithIQRL).
	Paymaster *common.Address

	Descriptor  [3]byte
	ExtraParams []byte
	Signature   []byte
	PublicKey   []byte
}

// copy creates a deep copy of the transaction data and initializes all fields.
func (tx *PaymasterDynamicFeeTx) copy() TxData {
	cpy := &PaymasterDynamicFeeTx{
		Nonce:     tx.Nonce,
		To:        copyAddressPtr(tx.To),
		Paymaster: copyAddressPtr(tx.Paymaster),
		Data:      common.CopyBytes(tx.Data),
		Gas:       tx.Gas,
		AccessList:  make(AccessList, len(tx.AccessList)),
		Value:       new(big.Int),
		ChainID:     new(big.Int),
		GasTipCap:   new(big.Int),
		GasFeeCap:   new(big.Int),
		Descriptor:  tx.Descriptor,
		ExtraParams: common.CopyBytes(tx.ExtraParams),
		PublicKey:   make([]byte, pqcrypto.MLDSA87PublicKeyLength),
		Signature:   make([]byte, pqcrypto.MLDSA87SignatureLength),
	}
	copy(cpy.AccessList, tx.AccessList)
	if tx.Value != nil {
		cpy.Value.Set(tx.Value)
	}
	if tx.ChainID != nil {
		cpy.ChainID.Set(tx.ChainID)
	}
	if tx.GasTipCap != nil {
		cpy.GasTipCap.Set(tx.GasTipCap)
	}
	if tx.GasFeeCap != nil {
		cpy.GasFeeCap.Set(tx.GasFeeCap)
	}
	if tx.PublicKey != nil {
		copy(cpy.PublicKey[:pqcrypto.MLDSA87PublicKeyLength], tx.PublicKey)
	}
	if tx.Signature != nil {
		copy(cpy.Signature[:pqcrypto.MLDSA87SignatureLength], tx.Signature)
	}
	return cpy
}

func (tx *PaymasterDynamicFeeTx) txType() byte           { return PaymasterDynamicFeeTxType }
func (tx *PaymasterDynamicFeeTx) chainID() *big.Int      { return tx.ChainID }
func (tx *PaymasterDynamicFeeTx) accessList() AccessList { return tx.AccessList }
func (tx *PaymasterDynamicFeeTx) data() []byte           { return tx.Data }
func (tx *PaymasterDynamicFeeTx) gas() uint64            { return tx.Gas }
func (tx *PaymasterDynamicFeeTx) gasFeeCap() *big.Int    { return tx.GasFeeCap }
func (tx *PaymasterDynamicFeeTx) gasTipCap() *big.Int    { return tx.GasTipCap }
func (tx *PaymasterDynamicFeeTx) gasPrice() *big.Int     { return tx.GasFeeCap }
func (tx *PaymasterDynamicFeeTx) value() *big.Int        { return tx.Value }
func (tx *PaymasterDynamicFeeTx) nonce() uint64          { return tx.Nonce }
func (tx *PaymasterDynamicFeeTx) to() *common.Address    { return tx.To }
func (tx *PaymasterDynamicFeeTx) paymaster() *common.Address {
	return copyAddressPtr(tx.Paymaster)
}

func (tx *PaymasterDynamicFeeTx) effectiveGasPrice(dst *big.Int, baseFee *big.Int) *big.Int {
	if baseFee == nil {
		return dst.Set(tx.GasFeeCap)
	}
	tip := dst.Sub(tx.GasFeeCap, baseFee)
	if tip.Cmp(tx.GasTipCap) > 0 {
		tip.Set(tx.GasTipCap)
	}
	return tip.Add(tip, baseFee)
}

func (tx *PaymasterDynamicFeeTx) rawSignatureValue() []byte {
	return tx.Signature
}

func (tx *PaymasterDynamicFeeTx) rawPublicKeyValue() []byte {
	return tx.PublicKey
}

func (tx *PaymasterDynamicFeeTx) descriptor() []byte {
	return tx.Descriptor[:]
}

func (tx *PaymasterDynamicFeeTx) extraParams() []byte {
	return tx.ExtraParams
}

func (tx *PaymasterDynamicFeeTx) setAuthValues(chainID *big.Int, sig, pk, desc, extraParams []byte) {
	tx.ChainID = chainID
	copy(tx.Descriptor[:], desc)
	tx.ExtraParams = extraParams
	tx.PublicKey = pk
	tx.Signature = sig
}

func (tx *PaymasterDynamicFeeTx) encode(b *bytes.Buffer) error {
	return rlp.Encode(b, tx)
}

func (tx *PaymasterDynamicFeeTx) decode(input []byte) error {
	return rlp.DecodeBytes(input, tx)
}
