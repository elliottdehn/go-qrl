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
	forBlock := big.NewInt(0xabcd)
	price := bigStr("1500000000000000000")
	calldata, err := EncodeSubmitVoteCalldata(forBlock, price)
	if err != nil {
		t.Fatal(err)
	}
	// Selector for submitVote(uint256,uint256) is 0x6f93bfb7.
	if len(calldata) != 4+32+32 {
		t.Fatalf("calldata length: got %d, want %d", len(calldata), 4+32+32)
	}
	wantSel := []byte{0x6f, 0x93, 0xbf, 0xb7}
	for i, b := range wantSel {
		if calldata[i] != b {
			t.Fatalf("selector byte %d: got %02x, want %02x", i, calldata[i], b)
		}
	}
	// First arg: forBlockNumber, left-padded to 32 bytes at offset 4.
	wantBlock := forBlock.Bytes()
	gotBlock := calldata[4+32-len(wantBlock) : 4+32]
	if string(gotBlock) != string(wantBlock) {
		t.Fatalf("forBlockNumber arg: got %x, want %x", gotBlock, wantBlock)
	}
	// Second arg: price, at offset 4+32.
	wantPrice := price.Bytes()
	gotPrice := calldata[4+64-len(wantPrice):]
	if string(gotPrice) != string(wantPrice) {
		t.Fatalf("price arg: got %x, want %x", gotPrice, wantPrice)
	}
}

func TestEncodeSubmitVoteCalldata_RejectsBadInputs(t *testing.T) {
	one := big.NewInt(1)
	cases := []struct {
		name     string
		forBlock *big.Int
		price    *big.Int
	}{
		{"nil price", one, nil},
		{"zero price", one, big.NewInt(0)},
		{"negative price", one, big.NewInt(-1)},
		{"nil block", nil, one},
		{"negative block", big.NewInt(-1), one},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := EncodeSubmitVoteCalldata(c.forBlock, c.price); err == nil {
				t.Errorf("expected error")
			}
		})
	}

	// forBlock = 0 is allowed: a chain that hasn't produced its first
	// block yet might still receive a submission targeted at block 0.
	if _, err := EncodeSubmitVoteCalldata(big.NewInt(0), one); err != nil {
		t.Errorf("forBlock=0 should encode, got %v", err)
	}
}
