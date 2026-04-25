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

// mkNativeTx builds an EIP-1559 (non-paymaster) tx for the test heap.
func mkNativeTx(t *testing.T, w wallet.Wallet, nonce uint64, gasFeeCap, gasTipCap *big.Int) *types.Transaction {
	t.Helper()
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	to := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	tx, err := types.SignNewTx(w, signer, &types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     nonce,
		GasTipCap: gasTipCap,
		GasFeeCap: gasFeeCap,
		Gas:       100_000,
		To:        &to,
		Value:     big.NewInt(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// mkPaymasterTx builds a paymaster (type 0x04) tx with the given
// gasPrice (denominated in iQRL).
func mkPaymasterTx(t *testing.T, w wallet.Wallet, nonce uint64, gasPrice *big.Int) *types.Transaction {
	t.Helper()
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	to := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	tx, err := types.SignNewTx(w, signer, &types.PaymasterDynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     nonce,
		GasTipCap: gasPrice,
		GasFeeCap: gasPrice,
		Gas:       100_000,
		To:        &to,
		Value:     big.NewInt(0),
		Paymaster: &core.PayWithIQRLAddress,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestPaymasterFactor_HealthyOracle(t *testing.T) {
	// p = $1.00 -> p_scaled = 1e18. 1/p^2 = 1, so factor reduces to
	// num/den = 1e36 / 1e36 = 1. Paymaster gas-price → equal QRL.
	p := big.NewInt(1_000_000_000_000_000_000) // 1e18
	num, den := paymasterFactor(p, true)
	if num == nil || den == nil {
		t.Fatal("healthy oracle returned nil factor")
	}
	want := new(big.Int).Mul(p, p)
	if den.Cmp(want) != 0 {
		t.Errorf("den: got %s, want %s", den, want)
	}
}

func TestPaymasterFactor_UnhealthyOracle(t *testing.T) {
	num, den := paymasterFactor(big.NewInt(0), true)
	if num != nil || den != nil {
		t.Errorf("zero price: got (%v, %v), want (nil, nil)", num, den)
	}
	num, den = paymasterFactor(big.NewInt(1e18), false)
	if num != nil || den != nil {
		t.Errorf("unhealthy: got (%v, %v), want (nil, nil)", num, den)
	}
	num, den = paymasterFactor(nil, true)
	if num != nil || den != nil {
		t.Errorf("nil price: got (%v, %v), want (nil, nil)", num, den)
	}
}

// TestPriceHeap_PaymasterAtPeg_EquivalentToNative: at p=$1.00, 1 iQRL
// = 1 QRL (since iQRL value = 1/p USD = 1 USD = 1 QRL at peg).
// A paymaster tx and a native tx with identical gasPrices should
// compare equal.
func TestPriceHeap_PaymasterAtPeg_EquivalentToNative(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	native := mkNativeTx(t, w, 0, big.NewInt(2_000_000_000), big.NewInt(2_000_000_000))
	paymaster := mkPaymasterTx(t, w, 1, big.NewInt(2_000_000_000))

	num, den := paymasterFactor(big.NewInt(1_000_000_000_000_000_000), true)
	h := &priceHeap{
		baseFee:            big.NewInt(0), // no base fee → effective tip == gasFeeCap
		paymasterFactorNum: num,
		paymasterFactorDen: den,
	}

	tipNative := h.effectiveQrlTip(native)
	tipPaymaster := h.effectiveQrlTip(paymaster)
	// At peg with no base fee, both should yield 2 gwei.
	want := big.NewInt(2_000_000_000)
	if tipNative.Cmp(want) != 0 {
		t.Errorf("native tip: got %s, want %s", tipNative, want)
	}
	if tipPaymaster.Cmp(want) != 0 {
		t.Errorf("paymaster tip: got %s, want %s", tipPaymaster, want)
	}
}

// TestPriceHeap_PaymasterAtHighPrice_DiscountedInQRL: p=$10 means
// 1 iQRL = 1/100 QRL. A paymaster tx with gasPrice = 100 wei iQRL
// is worth 1 wei QRL, which should be 100x cheaper than a native
// tx with gasPrice = 100 wei QRL.
func TestPriceHeap_PaymasterAtHighPrice_DiscountedInQRL(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	native := mkNativeTx(t, w, 0, big.NewInt(100), big.NewInt(100))
	paymaster := mkPaymasterTx(t, w, 1, big.NewInt(100))

	// p = $10 = 10e18 scaled.
	p, _ := new(big.Int).SetString("10000000000000000000", 10)
	num, den := paymasterFactor(p, true)
	h := &priceHeap{
		baseFee:            big.NewInt(0),
		paymasterFactorNum: num,
		paymasterFactorDen: den,
	}

	tipNative := h.effectiveQrlTip(native)
	tipPaymaster := h.effectiveQrlTip(paymaster)
	// paymaster QRL-equivalent = 100 * 1e36 / (10e18)^2 = 100 / 100 = 1.
	if tipNative.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("native tip: got %s, want 100", tipNative)
	}
	if tipPaymaster.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("paymaster tip: got %s, want 1", tipPaymaster)
	}
	// Comparison: native should sort higher.
	if c := h.cmp(native, paymaster); c <= 0 {
		t.Errorf("expected native > paymaster, got cmp=%d", c)
	}
}

// TestPriceHeap_PaymasterAtLowPrice_PremiumInQRL: p=$0.10 means
// 1 iQRL = 100 QRL. A paymaster tx with gasPrice = 1 wei iQRL is
// worth 100 wei QRL — should *outrank* a native tx at gasPrice=10.
func TestPriceHeap_PaymasterAtLowPrice_PremiumInQRL(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	native := mkNativeTx(t, w, 0, big.NewInt(10), big.NewInt(10))
	paymaster := mkPaymasterTx(t, w, 1, big.NewInt(1))

	// p = $0.10 = 0.1e18 = 1e17 scaled.
	num, den := paymasterFactor(big.NewInt(100_000_000_000_000_000), true)
	h := &priceHeap{
		baseFee:            big.NewInt(0),
		paymasterFactorNum: num,
		paymasterFactorDen: den,
	}

	tipPaymaster := h.effectiveQrlTip(paymaster)
	// 1 * 1e36 / (1e17)^2 = 1e36 / 1e34 = 100.
	if tipPaymaster.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("paymaster tip: got %s, want 100", tipPaymaster)
	}
	if c := h.cmp(paymaster, native); c <= 0 {
		t.Errorf("expected paymaster > native, got cmp=%d", c)
	}
}

// TestPriceHeap_OracleUnhealthy_PaymasterZeroPriority: when the
// oracle is unhealthy (factor cleared), paymaster txs sort below
// any native tx with positive tip — they don't get to squeeze out
// real bidders during an outage.
func TestPriceHeap_OracleUnhealthy_PaymasterZeroPriority(t *testing.T) {
	w, _ := wallet.Generate(wallet.ML_DSA_87)
	native := mkNativeTx(t, w, 0, big.NewInt(1), big.NewInt(1))
	huge, _ := new(big.Int).SetString("1000000000000000000", 10)
	paymaster := mkPaymasterTx(t, w, 1, huge) // 1 iQRL — huge

	h := &priceHeap{
		baseFee: big.NewInt(0),
		// factor unset
	}

	if got := h.effectiveQrlTip(paymaster); got.Sign() != 0 {
		t.Errorf("unhealthy oracle: paymaster tip should be 0, got %s", got)
	}
	if c := h.cmp(native, paymaster); c <= 0 {
		t.Errorf("expected native > paymaster under unhealthy oracle, cmp=%d", c)
	}
}
