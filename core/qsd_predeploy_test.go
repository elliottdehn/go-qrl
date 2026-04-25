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

// TestAddQSDStabilityLayerInjectsThreeEntries verifies the helper
// adds exactly the three reserved addresses to a GenesisAlloc.
func TestAddQSDStabilityLayerInjectsThreeEntries(t *testing.T) {
	alloc := GenesisAlloc{}
	owner := common.BytesToAddress(common.FromHex("0xc0ffee0000000000000000000000000000000000"))
	AddQSDStabilityLayer(alloc, DefaultQSDPredeployParams(owner))

	if len(alloc) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(alloc))
	}
	for _, want := range []common.Address{
		ValidatorOracleAddress,
		InverseQRLAddress,
		QSDAddress,
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
	} {
		if _, ok := g.Alloc[want]; !ok {
			t.Fatalf("dev genesis missing %s", want.Hex())
		}
	}
}
