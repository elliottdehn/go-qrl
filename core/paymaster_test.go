// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
)

func TestEncodeAddressUint256_Layout(t *testing.T) {
	addr := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000beef"))
	n := big.NewInt(0x1234)
	sel := []byte{0xaa, 0xbb, 0xcc, 0xdd}

	got := encodeAddressUint256(sel, addr, n)
	if len(got) != 4+32+32 {
		t.Fatalf("length: got %d, want %d", len(got), 4+32+32)
	}
	if !bytes.Equal(got[:4], sel) {
		t.Errorf("selector: got %x, want %x", got[:4], sel)
	}
	// Address is right-aligned in slot 1 (bytes 4..36).
	if !bytes.Equal(got[4:36-20], make([]byte, 12)) {
		t.Errorf("address slot: leading 12 bytes not zeros: %x", got[4:16])
	}
	if !bytes.Equal(got[36-20:36], addr.Bytes()) {
		t.Errorf("address slot: tail mismatch")
	}
	// uint256 is right-aligned in slot 2 (bytes 36..68).
	want := common.LeftPadBytes(n.Bytes(), 32)
	if !bytes.Equal(got[36:], want) {
		t.Errorf("uint256 slot: got %x, want %x", got[36:], want)
	}
}

func TestEncodeAddressUint256Uint256_Layout(t *testing.T) {
	addr := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c0de"))
	n := big.NewInt(7)
	m := big.NewInt(11)
	sel := []byte{0x11, 0x22, 0x33, 0x44}

	got := encodeAddressUint256Uint256(sel, addr, n, m)
	if len(got) != 4+32+32+32 {
		t.Fatalf("length: got %d, want %d", len(got), 4+32+32+32)
	}
	wantN := common.LeftPadBytes(n.Bytes(), 32)
	wantM := common.LeftPadBytes(m.Bytes(), 32)
	if !bytes.Equal(got[36:68], wantN) {
		t.Errorf("first uint256: got %x, want %x", got[36:68], wantN)
	}
	if !bytes.Equal(got[68:100], wantM) {
		t.Errorf("second uint256: got %x, want %x", got[68:100], wantM)
	}
}

// TestPaymasterSelectors_MatchSolidityABI pins the function selectors
// to their Solidity ABI computations. If anyone changes the
// PayWithIQRL signatures without updating these constants, the chain
// silently calls the wrong functions; this test catches it.
func TestPaymasterSelectors_MatchSolidityABI(t *testing.T) {
	// `cast sig "escrow(address,uint256)"` -> 0x2efd5b06
	wantEscrow := []byte{0x2e, 0xfd, 0x5b, 0x06}
	if !bytes.Equal(paymasterEscrowSelector, wantEscrow) {
		t.Errorf("escrow selector: got %x, want %x", paymasterEscrowSelector, wantEscrow)
	}
	// `cast sig "settle(address,uint256,uint256,uint256)"` -> 0xc6a3d074
	wantSettle := []byte{0xc6, 0xa3, 0xd0, 0x74}
	if !bytes.Equal(paymasterSettleSelector, wantSettle) {
		t.Errorf("settle selector: got %x, want %x", paymasterSettleSelector, wantSettle)
	}
}

func TestIsAllowedPaymaster(t *testing.T) {
	if !IsAllowedPaymaster(PayWithIQRLAddress) {
		t.Error("PayWithIQRLAddress should be on the allowlist")
	}
	other := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000beef"))
	if IsAllowedPaymaster(other) {
		t.Errorf("%s should not be allowed", other.Hex())
	}
	if IsAllowedPaymaster(common.Address{}) {
		t.Error("zero address should not be allowed")
	}
}

// TestSystemCallerAddress_MatchesContract ensures the Go-side
// sentinel sender matches PayWithIQRL.SYSTEM_CALLER. If they
// disagree, every paymaster system call reverts with NotSystemCaller
// at runtime — silent gas waste, no funds lost, but the feature is
// dead. Keep them in lockstep.
func TestSystemCallerAddress_MatchesContract(t *testing.T) {
	want := common.BytesToAddress(common.FromHex("0xfffffffffffffffffffffffffffffffffffffffe"))
	if SystemCallerAddress != want {
		t.Errorf("SystemCallerAddress: got %s, want %s",
			SystemCallerAddress.Hex(), want.Hex())
	}
}
