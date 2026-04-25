// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"math/big"
	"testing"
)

func bigStr(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad big string: " + s)
	}
	return v
}

func TestParseUSDPriceTo1e18(t *testing.T) {
	cases := []struct {
		in   string
		want *big.Int
		err  bool
	}{
		{"1", bigStr("1000000000000000000"), false},          // 1e18
		{"1.0", bigStr("1000000000000000000"), false},
		{"1.50", bigStr("1500000000000000000"), false},       // 1.5e18
		{"0.5", bigStr("500000000000000000"), false},         // 5e17
		{"$2.00", bigStr("2000000000000000000"), false},
		{"  3.14  ", bigStr("3140000000000000000"), false},
		{"0.000000000000000001", bigStr("1"), false},         // 1 wei == smallest USD unit
		// Truncates beyond 18 decimals.
		{"1.123456789012345678999", bigStr("1123456789012345678"), false},
		{"100", bigStr("100000000000000000000"), false},      // 100e18
		// Errors.
		{"", nil, true},
		{".", nil, true},
		{"abc", nil, true},
		{"-1.0", nil, true},
		{"0", nil, true},
		{"0.0", nil, true},
	}
	for _, c := range cases {
		got, err := parseUSDPriceTo1e18(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseUSDPriceTo1e18(%q): expected error, got %s", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUSDPriceTo1e18(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got.Cmp(c.want) != 0 {
			t.Errorf("parseUSDPriceTo1e18(%q): got %s, want %s", c.in, got, c.want)
		}
	}
}

func TestStaticPriceSourceIsImmutable(t *testing.T) {
	src, err := NewStaticPriceSource("1.00")
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := src.FetchUSDPerQRL(context.Background())
	p1.SetUint64(0xdeadbeef) // try to mutate
	p2, _ := src.FetchUSDPerQRL(context.Background())
	if p2.Cmp(bigStr("1000000000000000000")) != 0 {
		t.Fatalf("static source mutated: got %s", p2)
	}
}

func TestEncodeSubmitVoteCalldata(t *testing.T) {
	calldata, err := EncodeSubmitVoteCalldata(bigStr("1500000000000000000"))
	if err != nil {
		t.Fatal(err)
	}
	// Selector for submitVote(uint256) is 0x2844328f.
	if len(calldata) != 36 {
		t.Fatalf("calldata length: got %d, want 36", len(calldata))
	}
	wantSel := []byte{0x28, 0x44, 0x32, 0x8f}
	for i, b := range wantSel {
		if calldata[i] != b {
			t.Fatalf("selector byte %d: got %02x, want %02x", i, calldata[i], b)
		}
	}
	// uint256 portion is the value left-padded to 32 bytes, so byte 23
	// onward is the actual significant bytes of 1.5e18.
	wantArg := bigStr("1500000000000000000").Bytes()
	gotArg := calldata[4+32-len(wantArg):]
	if string(gotArg) != string(wantArg) {
		t.Fatalf("uint256 arg: got %x, want %x", gotArg, wantArg)
	}
}

func TestEncodeSubmitVoteCalldata_RejectsZeroAndNegative(t *testing.T) {
	if _, err := EncodeSubmitVoteCalldata(big.NewInt(0)); err == nil {
		t.Error("expected error for zero")
	}
	if _, err := EncodeSubmitVoteCalldata(big.NewInt(-1)); err == nil {
		t.Error("expected error for negative")
	}
	if _, err := EncodeSubmitVoteCalldata(nil); err == nil {
		t.Error("expected error for nil")
	}
}
