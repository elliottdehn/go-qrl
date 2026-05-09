// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"math/big"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/crypto"
)

// VoteSubmitter dispatches a price vote to the on-chain
// ValidatorOracle contract.
//
// forBlockNumber is the block in which the caller intends the vote
// to land. The contract enforces strict equality: a tx that mines in
// any other block reverts with WrongBlockNumber. Callers should set
// forBlockNumber = currentHead + N for some small N matching their
// expected propagation latency.
type VoteSubmitter interface {
	SubmitVote(ctx context.Context, forBlockNumber, priceUsd1e18 *big.Int) error
	Address() common.Address
}

// submitVoteSelector is the 4-byte function selector for
//
//	submitVote(uint256,uint256)
//
// computed once at init. Verified against `cast sig` in tests.
var submitVoteSelector = crypto.Keccak256([]byte("submitVote(uint256,uint256)"))[:4]

// EncodeSubmitVoteCalldata produces the calldata for a single
// `submitVote(uint256,uint256)` invocation.
//
// Layout: 4-byte selector || 32-byte big-endian forBlockNumber ||
// 32-byte big-endian priceUsd1e18.
func EncodeSubmitVoteCalldata(forBlockNumber, priceUsd1e18 *big.Int) ([]byte, error) {
	if forBlockNumber == nil || forBlockNumber.Sign() < 0 {
		return nil, fmt.Errorf("forBlockNumber must be non-negative")
	}
	if priceUsd1e18 == nil || priceUsd1e18.Sign() <= 0 {
		return nil, fmt.Errorf("price must be positive")
	}
	if len(priceUsd1e18.Bytes()) > 32 {
		return nil, fmt.Errorf("price exceeds uint256")
	}
	if len(forBlockNumber.Bytes()) > 32 {
		return nil, fmt.Errorf("forBlockNumber exceeds uint256")
	}

	out := make([]byte, 0, 4+32+32)
	out = append(out, submitVoteSelector...)
	out = append(out, padTo32(forBlockNumber)...)
	out = append(out, padTo32(priceUsd1e18)...)
	return out, nil
}

// padTo32 left-pads a non-negative big.Int to 32 bytes.
func padTo32(v *big.Int) []byte {
	asBytes := v.Bytes()
	padded := make([]byte, 32)
	copy(padded[32-len(asBytes):], asBytes)
	return padded
}

// stubSubmitter is a no-op VoteSubmitter that only logs what it
// would have submitted. Used until full wallet signing is wired in.
type stubSubmitter struct {
	oracleAddress common.Address
	logger        func(format string, args ...any)
}

func (s *stubSubmitter) SubmitVote(_ context.Context, forBlockNumber, priceUsd1e18 *big.Int) error {
	calldata, err := EncodeSubmitVoteCalldata(forBlockNumber, priceUsd1e18)
	if err != nil {
		return err
	}
	s.logger(
		"[stub] would submitVote oracle=%s forBlock=%s price=%s calldata=0x%x",
		s.oracleAddress.Hex(),
		forBlockNumber.String(),
		priceUsd1e18.String(),
		calldata,
	)
	return nil
}

func (s *stubSubmitter) Address() common.Address {
	return s.oracleAddress
}

// NewStubSubmitter returns a submitter that only logs. Real RPC +
// wallet signing lands in the next commit.
func NewStubSubmitter(oracle common.Address, logger func(format string, args ...any)) VoteSubmitter {
	return &stubSubmitter{oracleAddress: oracle, logger: logger}
}
