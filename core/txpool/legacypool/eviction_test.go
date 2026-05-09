// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package legacypool

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
	"github.com/theQRL/go-qrl/params"
)

func TestSubmitVoteTargetBlock_DecodesForBlock(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)

	// Build calldata: selector || forBlock=42 || price=1e18.
	data := make([]byte, 0, 4+32+32)
	data = append(data, core.SubmitVoteSelector...)
	data = append(data, common.LeftPadBytes(big.NewInt(42).Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(big.NewInt(1_000_000_000_000_000_000).Bytes(), 32)...)

	tx, err := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       100_000,
		To:        &core.ValidatorOracleAddress,
		Value:     big.NewInt(0),
		Data:      data,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := submitVoteTargetBlock(tx)
	if !ok {
		t.Fatal("expected match for submitVote tx")
	}
	if got != 42 {
		t.Errorf("forBlock: got %d, want 42", got)
	}
}

func TestSubmitVoteTargetBlock_RejectsNonOracleTarget(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	other := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000beef"))

	data := append([]byte{}, core.SubmitVoteSelector...)
	data = append(data, make([]byte, 64)...)

	tx, _ := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1),
		Gas: 100_000, To: &other, Value: big.NewInt(0), Data: data,
	})

	if _, ok := submitVoteTargetBlock(tx); ok {
		t.Error("non-oracle target should not match")
	}
}

func TestSubmitVoteTargetBlock_RejectsWrongSelector(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)

	// Wrong selector + valid arg layout.
	data := []byte{0xde, 0xad, 0xbe, 0xef}
	data = append(data, make([]byte, 64)...)

	tx, _ := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1),
		Gas: 100_000, To: &core.ValidatorOracleAddress, Value: big.NewInt(0), Data: data,
	})

	if _, ok := submitVoteTargetBlock(tx); ok {
		t.Error("wrong selector should not match")
	}
}

func TestSubmitVoteTargetBlock_RejectsTruncatedCalldata(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)

	// Selector but no args.
	tx, _ := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1),
		Gas: 100_000, To: &core.ValidatorOracleAddress, Value: big.NewInt(0),
		Data: append([]byte{}, core.SubmitVoteSelector...),
	})

	if _, ok := submitVoteTargetBlock(tx); ok {
		t.Error("calldata-too-short should not match")
	}
}
