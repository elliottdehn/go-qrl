// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package legacypool

import (
	"errors"
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
	"github.com/theQRL/go-qrl/params"
)

// signPaymasterTx returns a signed type-0x04 PaymasterDynamicFeeTx
// targeting PayWithIQRL. Used to exercise the txpool's fork-gated
// admission path.
func signPaymasterTx(t *testing.T, w wallet.Wallet, chainID *big.Int, nonce uint64) *types.Transaction {
	t.Helper()
	signer := types.LatestSignerForChainID(chainID)
	to := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000bbb"))
	tx, err := types.SignNewTx(w, signer, &types.PaymasterDynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1_000_000_000),
		Gas:       100_000,
		To:        &to,
		Value:     big.NewInt(0),
		Paymaster: &core.PayWithIQRLAddress,
	})
	if err != nil {
		t.Fatalf("sign paymaster tx: %v", err)
	}
	return tx
}

// TestPaymasterTx_PreFork_Rejected: type-0x04 txs must not be admitted
// to the pool on a chain whose head timestamp is before the QSD fork
// (or where QSDTime is unset entirely).
func TestPaymasterTx_PreFork_Rejected(t *testing.T) {
	pool, w := setupPoolWithConfig(params.TestChainConfig) // QSDTime nil
	defer pool.Close()

	tx := signPaymasterTx(t, w, params.TestChainConfig.ChainID, 0)
	err := pool.validateTxBasics(tx, true)
	if err == nil {
		t.Fatal("pre-fork pool admitted a paymaster tx")
	}
	if !errors.Is(err, core.ErrTxTypeNotSupported) {
		t.Errorf("pre-fork rejection should cite tx type, got %v", err)
	}
}

// TestPaymasterTx_PostFork_Admitted: with QSDTime activated by the
// head timestamp, type-0x04 txs are admitted (and only fail on
// state-dependent reasons like balance, which we don't gate here).
func TestPaymasterTx_PostFork_Admitted(t *testing.T) {
	pool, w := setupPoolWithConfig(params.AllDevChainProtocolChanges) // QSDTime = 0
	defer pool.Close()

	tx := signPaymasterTx(t, w, params.AllDevChainProtocolChanges.ChainID, 0)
	if err := pool.validateTxBasics(tx, true); err != nil {
		// Type-only screen must pass; any error here would indicate
		// the fork gate still blocks paymaster txs post-activation.
		// (State-dependent validation runs separately.)
		t.Errorf("post-fork pool refused a paymaster tx at the type screen: %v", err)
	}
}

