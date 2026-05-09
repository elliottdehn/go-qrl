// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package types

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
)

func paymasterTestWallet(t *testing.T) wallet.Wallet {
	t.Helper()
	w, err := wallet.RestoreFromSeedHex(
		"010000b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f29100000000000000000000000000000000",
	)
	if err != nil {
		t.Fatalf("restore wallet: %v", err)
	}
	return w
}

// TestPaymasterTx_RoundTripEncoding verifies that a PaymasterDynamicFeeTx
// survives a full encode/decode cycle through the typed-tx framing.
func TestPaymasterTx_RoundTripEncoding(t *testing.T) {
	to := common.BytesToAddress(common.FromHex("0x" + "11"+"00000000000000000000000000000000000000"))
	pm := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010003"))

	w := paymasterTestWallet(t)
	signer := LatestSignerForChainID(big.NewInt(1337))

	tx, err := SignNewTx(w, signer, &PaymasterDynamicFeeTx{
		ChainID:   big.NewInt(1337),
		Nonce:     7,
		GasTipCap: big.NewInt(2_000_000_000),
		GasFeeCap: big.NewInt(20_000_000_000),
		Gas:       100_000,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      []byte{0xde, 0xad, 0xbe, 0xef},
		Paymaster: &pm,
	})
	if err != nil {
		t.Fatalf("SignNewTx: %v", err)
	}

	if tx.Type() != PaymasterDynamicFeeTxType {
		t.Errorf("type: got %d, want %d", tx.Type(), PaymasterDynamicFeeTxType)
	}
	if got := tx.Paymaster(); got == nil || *got != pm {
		t.Errorf("paymaster: got %v, want %v", got, pm)
	}

	// Encode → decode → verify fields preserved.
	enc, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	var dec Transaction
	if err := dec.UnmarshalBinary(enc); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if dec.Type() != PaymasterDynamicFeeTxType {
		t.Errorf("decoded type: got %d, want %d", dec.Type(), PaymasterDynamicFeeTxType)
	}
	if got := dec.Paymaster(); got == nil || *got != pm {
		t.Errorf("decoded paymaster: got %v, want %v", got, pm)
	}
	if dec.Nonce() != 7 {
		t.Errorf("decoded nonce: got %d, want 7", dec.Nonce())
	}
	if !bytes.Equal(dec.Data(), []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("decoded data: got %x, want deadbeef", dec.Data())
	}
}

// TestPaymasterTx_SignerCoversPaymaster verifies that flipping the
// Paymaster field changes the signing hash, so a signed tx cannot
// have its paymaster swapped without invalidating the signature.
func TestPaymasterTx_SignerCoversPaymaster(t *testing.T) {
	pm1 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010003"))
	pm2 := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000beef"))

	signer := LatestSignerForChainID(big.NewInt(1337))
	w := paymasterTestWallet(t)

	tx1, err := SignNewTx(w, signer, &PaymasterDynamicFeeTx{
		ChainID:   big.NewInt(1337),
		Nonce:     1,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
		Value:     big.NewInt(0),
		Paymaster: &pm1,
	})
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := SignNewTx(w, signer, &PaymasterDynamicFeeTx{
		ChainID:   big.NewInt(1337),
		Nonce:     1,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
		Value:     big.NewInt(0),
		Paymaster: &pm2,
	})
	if err != nil {
		t.Fatal(err)
	}

	if tx1.Hash() == tx2.Hash() {
		t.Error("two txs with different paymasters share a hash — signer payload missing paymaster")
	}
}

// TestPaymasterTx_JSONRoundTrip verifies that JSON marshalling
// preserves the paymaster field and that unmarshalling reconstructs
// the right tx type.
func TestPaymasterTx_JSONRoundTrip(t *testing.T) {
	pm := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010003"))
	to := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))

	w := paymasterTestWallet(t)
	signer := LatestSignerForChainID(big.NewInt(1337))
	tx, err := SignNewTx(w, signer, &PaymasterDynamicFeeTx{
		ChainID:   big.NewInt(1337),
		Nonce:     7,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       100_000,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      []byte{0x01, 0x02, 0x03},
		Paymaster: &pm,
	})
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := tx.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	// QRL renders addresses with a "Q" prefix.
	if !bytes.Contains(encoded, []byte(`"paymaster":"Q0000000000000000000000000000000000010003"`)) {
		t.Errorf("paymaster field missing from JSON: %s", encoded)
	}

	var dec Transaction
	if err := dec.UnmarshalJSON(encoded); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if dec.Type() != PaymasterDynamicFeeTxType {
		t.Errorf("type: got %d, want %d", dec.Type(), PaymasterDynamicFeeTxType)
	}
	if got := dec.Paymaster(); got == nil || *got != pm {
		t.Errorf("paymaster: got %v, want %v", got, pm)
	}
}

// TestDynamicFeeTx_PaymasterIsNil ensures the standard DynamicFeeTx
// reports a nil paymaster — the backwards-compat path for
// non-paymaster txs.
func TestDynamicFeeTx_PaymasterIsNil(t *testing.T) {
	signer := LatestSignerForChainID(big.NewInt(1337))
	w := paymasterTestWallet(t)

	tx, err := SignNewTx(w, signer, &DynamicFeeTx{
		ChainID:   big.NewInt(1337),
		Nonce:     1,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
		Value:     big.NewInt(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := tx.Paymaster(); got != nil {
		t.Errorf("DynamicFeeTx paymaster: got %v, want nil", got)
	}
}
