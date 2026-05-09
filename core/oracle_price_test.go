// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/crypto"
)

// fakeOracleState is a hand-rolled OracleStorageReader for unit
// testing. Only stores the slots we care about (validators length,
// validators[i], votes[v]); other reads return zero.
type fakeOracleState struct {
	slots map[common.Hash]common.Hash
}

func newFakeOracleState() *fakeOracleState {
	return &fakeOracleState{slots: map[common.Hash]common.Hash{}}
}

func (f *fakeOracleState) GetState(addr common.Address, slot common.Hash) common.Hash {
	if addr != ValidatorOracleAddress {
		return common.Hash{}
	}
	return f.slots[slot]
}

func (f *fakeOracleState) setValidators(addrs []common.Address) {
	// Length at slot 1.
	lenSlot := common.Hash{}
	lenSlot[31] = byte(validatorOracleValidatorsLengthSlot)
	f.slots[lenSlot] = common.BigToHash(big.NewInt(int64(len(addrs))))

	// Elements at keccak256(slot 1) + i.
	base := crypto.Keccak256Hash(common.LeftPadBytes(
		big.NewInt(validatorOracleValidatorsLengthSlot).Bytes(), 32))
	baseInt := new(big.Int).SetBytes(base.Bytes())
	for i, a := range addrs {
		idxInt := new(big.Int).Add(baseInt, big.NewInt(int64(i)))
		key := common.BytesToHash(common.LeftPadBytes(idxInt.Bytes(), 32))
		f.slots[key] = common.BytesToHash(common.LeftPadBytes(a.Bytes(), 32))
	}
}

func (f *fakeOracleState) setVote(v common.Address, price *big.Int, blockNumber uint64) {
	slot := ValidatorVoteStorageSlot(v)
	// Pack: bytes [16:32]=price, bytes [8:16]=blockNumber, [7]=padding.
	var packed common.Hash
	priceB := common.LeftPadBytes(price.Bytes(), 16)
	copy(packed[16:32], priceB)
	for i := 0; i < 8; i++ {
		packed[15-i] = byte(blockNumber >> (8 * i))
	}
	f.slots[slot] = packed
}

func TestComputeOraclePrice_NoValidators(t *testing.T) {
	s := newFakeOracleState()
	price, healthy := ComputeOraclePrice(s, 100)
	if price.Sign() != 0 {
		t.Errorf("price: got %s, want 0", price)
	}
	if healthy {
		t.Error("empty validator set should not be healthy")
	}
}

func TestComputeOraclePrice_AllFreshSamePrice(t *testing.T) {
	v1 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000001"))
	v2 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000002"))
	v3 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000003"))
	s := newFakeOracleState()
	s.setValidators([]common.Address{v1, v2, v3})
	p, _ := new(big.Int).SetString("1000000000000000000", 10) // 1e18 = $1
	s.setVote(v1, p, 100)
	s.setVote(v2, p, 100)
	s.setVote(v3, p, 100)

	price, healthy := ComputeOraclePrice(s, 100)
	if price.Cmp(p) != 0 {
		t.Errorf("price: got %s, want %s", price, p)
	}
	if !healthy {
		t.Error("3/3 fresh should be healthy")
	}
}

func TestComputeOraclePrice_StaleVotesDropped(t *testing.T) {
	v1 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000001"))
	v2 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000002"))
	v3 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000003"))
	s := newFakeOracleState()
	s.setValidators([]common.Address{v1, v2, v3})
	p, _ := new(big.Int).SetString("1000000000000000000", 10)
	// v1, v2 fresh; v3 stale (older than current - staleness window).
	s.setVote(v1, p, 100)
	s.setVote(v2, p, 100)
	s.setVote(v3, p, 50) // current=100, threshold=100-10=90; 50 < 90 → stale

	price, healthy := ComputeOraclePrice(s, 100)
	if price.Cmp(p) != 0 {
		t.Errorf("price: got %s, want %s", price, p)
	}
	// 2/3 = 0.666... ≥ 2/3 quorum → healthy
	if !healthy {
		t.Error("2/3 fresh at exact quorum should be healthy")
	}
}

func TestComputeOraclePrice_BelowQuorum_Unhealthy(t *testing.T) {
	v1 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000001"))
	v2 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000002"))
	v3 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000003"))
	s := newFakeOracleState()
	s.setValidators([]common.Address{v1, v2, v3})
	p, _ := new(big.Int).SetString("1000000000000000000", 10)
	// Only v1 fresh.
	s.setVote(v1, p, 100)
	s.setVote(v2, p, 50) // stale
	s.setVote(v3, p, 50) // stale

	_, healthy := ComputeOraclePrice(s, 100)
	if healthy {
		t.Error("1/3 fresh should be below 2/3 quorum")
	}
}

func TestComputeOraclePrice_MedianOdd(t *testing.T) {
	v := []common.Address{
		common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000001")),
		common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000002")),
		common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000003")),
	}
	s := newFakeOracleState()
	s.setValidators(v)
	s.setVote(v[0], big.NewInt(100), 100)
	s.setVote(v[1], big.NewInt(200), 100)
	s.setVote(v[2], big.NewInt(300), 100)

	price, _ := ComputeOraclePrice(s, 100)
	if price.Cmp(big.NewInt(200)) != 0 {
		t.Errorf("median: got %s, want 200", price)
	}
}

func TestComputeOraclePrice_MedianEven(t *testing.T) {
	v := []common.Address{
		common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000001")),
		common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000002")),
		common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000003")),
		common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000004")),
	}
	s := newFakeOracleState()
	s.setValidators(v)
	s.setVote(v[0], big.NewInt(100), 100)
	s.setVote(v[1], big.NewInt(200), 100)
	s.setVote(v[2], big.NewInt(300), 100)
	s.setVote(v[3], big.NewInt(400), 100)

	price, _ := ComputeOraclePrice(s, 100)
	// (200 + 300) / 2 = 250
	if price.Cmp(big.NewInt(250)) != 0 {
		t.Errorf("median: got %s, want 250", price)
	}
}

func TestComputeOraclePrice_EarlyChain_NoUnderflowOnThreshold(t *testing.T) {
	// At block 5 with staleness window 10, threshold underflows to 0
	// — every vote with blockNumber >= 0 (i.e. all of them) counts.
	v1 := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000001"))
	s := newFakeOracleState()
	s.setValidators([]common.Address{v1})
	s.setVote(v1, big.NewInt(42), 1)

	price, _ := ComputeOraclePrice(s, 5)
	if price.Cmp(big.NewInt(42)) != 0 {
		t.Errorf("price: got %s, want 42", price)
	}
}
