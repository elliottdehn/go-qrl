// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"testing"

	"github.com/theQRL/go-qrl/common"
)

// TestQSDAddressesDistinctAndNonZero ensures the reserved QSD-layer
// addresses don't collide with each other or with any current
// precompile slot.
func TestQSDAddressesDistinctAndNonZero(t *testing.T) {
	addrs := []common.Address{
		ValidatorOracleAddress,
		InverseQRLAddress,
		QSDAddress,
		PayWithIQRLAddress,
	}
	seen := make(map[common.Address]bool, len(addrs))
	for _, a := range addrs {
		if a == (common.Address{}) {
			t.Fatalf("zero address reserved: %s", a.Hex())
		}
		if seen[a] {
			t.Fatalf("duplicate reserved address: %s", a.Hex())
		}
		seen[a] = true
	}

	// Ensure they don't fall inside the precompile band [0x01, 0x09]
	// (precompiles have all leading bytes zero except the last byte
	// which is in the low single-digit range).
	for _, a := range addrs {
		bytes := a.Bytes()
		allZeroLeading := true
		for i := 0; i < len(bytes)-1; i++ {
			if bytes[i] != 0 {
				allZeroLeading = false
				break
			}
		}
		if allZeroLeading && bytes[len(bytes)-1] <= 0x09 {
			t.Fatalf("reserved address %s falls inside precompile band", a.Hex())
		}
	}
}

// TestAddQSDStabilityLayerInjectsAllReservedEntries verifies the
// helper adds exactly the four reserved addresses to a GenesisAlloc.
func TestAddQSDStabilityLayerInjectsAllReservedEntries(t *testing.T) {
	alloc := GenesisAlloc{}
	owner := common.BytesToAddress(common.FromHex("0xc0ffee0000000000000000000000000000000000"))
	AddQSDStabilityLayer(alloc, DefaultQSDPredeployParams(owner))

	if len(alloc) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(alloc))
	}
	for _, want := range []common.Address{
		ValidatorOracleAddress,
		InverseQRLAddress,
		QSDAddress,
		PayWithIQRLAddress,
		YieldQSDAddress,
	} {
		if _, ok := alloc[want]; !ok {
			t.Fatalf("missing reserved address %s", want.Hex())
		}
	}
}

// TestDeveloperGenesisIncludesQSDAddresses ensures the dev genesis
// wiring threads through to the resulting GenesisAlloc.
func TestDeveloperGenesisIncludesQSDAddresses(t *testing.T) {
	faucet := common.BytesToAddress(common.FromHex("0xfee10000000000000000000000000000000000ff"))
	g := DeveloperGenesisBlock(0x1c9c380, faucet)

	for _, want := range []common.Address{
		ValidatorOracleAddress,
		InverseQRLAddress,
		QSDAddress,
		PayWithIQRLAddress,
	} {
		if _, ok := g.Alloc[want]; !ok {
			t.Fatalf("dev genesis missing %s", want.Hex())
		}
	}
}

// TestPredeployBytecodeIsNonEmpty verifies the embedded state dump
// actually populates each reserved address with deployed bytecode.
// A regression here means the JSON regenerated from the Foundry
// script either failed to land or was committed in a stub state.
func TestPredeployBytecodeIsNonEmpty(t *testing.T) {
	alloc := GenesisAlloc{}
	owner := common.BytesToAddress(common.FromHex("0xc0ffee0000000000000000000000000000000000"))
	AddQSDStabilityLayer(alloc, DefaultQSDPredeployParams(owner))

	for _, addr := range []common.Address{
		ValidatorOracleAddress,
		InverseQRLAddress,
		QSDAddress,
		PayWithIQRLAddress,
	} {
		acct := alloc[addr]
		if len(acct.Code) == 0 {
			t.Errorf("%s: empty code", addr.Hex())
		}
		// Sanity floor: the smallest of the four (PayWithIQRL)
		// compiles to ~1.2 KB. Anything below 512 bytes indicates a
		// stub or truncated dump.
		if len(acct.Code) < 512 {
			t.Errorf("%s: code suspiciously short (%d bytes)", addr.Hex(), len(acct.Code))
		}
	}
}

// TestPredeployErc20MetadataPreserved verifies ERC-20 _name/_symbol
// slots survived the dump → load round-trip. Slot 3 (name) and slot
// 4 (symbol) are short strings stored as <data><len*2> for short
// strings (≤31 bytes), which is the case for both contracts.
func TestPredeployErc20MetadataPreserved(t *testing.T) {
	alloc := GenesisAlloc{}
	AddQSDStabilityLayer(alloc, DefaultQSDPredeployParams(common.Address{1}))

	cases := []struct {
		addr     common.Address
		name     string
		nameSlot string
	}{
		{InverseQRLAddress, "Inverse QRL", "0x0000000000000000000000000000000000000000000000000000000000000003"},
		{QSDAddress, "Quantum Stable Dollar", "0x0000000000000000000000000000000000000000000000000000000000000003"},
	}
	for _, c := range cases {
		slotVal := alloc[c.addr].Storage[common.HexToHash(c.nameSlot)]
		// Short-string layout: first len(name) bytes hold the name,
		// last byte holds (len * 2). We just check the prefix.
		got := slotVal.Bytes()[:len(c.name)]
		if string(got) != c.name {
			t.Errorf("%s: name slot got %q, want %q", c.addr.Hex(), string(got), c.name)
		}
	}
}
