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
type VoteSubmitter interface {
	SubmitVote(ctx context.Context, priceUsd1e18 *big.Int) error
	Address() common.Address
}

// submitVoteSelector is the 4-byte function selector for
//
//	submitVote(uint256)
//
// computed once at init.
var submitVoteSelector = crypto.Keccak256([]byte("submitVote(uint256)"))[:4]

// EncodeSubmitVoteCalldata produces the calldata for a single
// `submitVote(uint256)` invocation.
//
// Layout: 4-byte selector || 32-byte big-endian uint256.
func EncodeSubmitVoteCalldata(priceUsd1e18 *big.Int) ([]byte, error) {
	if priceUsd1e18 == nil || priceUsd1e18.Sign() <= 0 {
		return nil, fmt.Errorf("price must be positive")
	}
	// uint256 is at most 32 bytes; left-pad.
	asBytes := priceUsd1e18.Bytes()
	if len(asBytes) > 32 {
		return nil, fmt.Errorf("price exceeds uint256")
	}
	padded := make([]byte, 32)
	copy(padded[32-len(asBytes):], asBytes)

	out := make([]byte, 0, 4+32)
	out = append(out, submitVoteSelector...)
	out = append(out, padded...)
	return out, nil
}

// stubSubmitter is a no-op VoteSubmitter that only logs what it
// would have submitted. Used until full wallet signing is wired in.
type stubSubmitter struct {
	oracleAddress common.Address
	logger        func(format string, args ...any)
}

func (s *stubSubmitter) SubmitVote(_ context.Context, priceUsd1e18 *big.Int) error {
	calldata, err := EncodeSubmitVoteCalldata(priceUsd1e18)
	if err != nil {
		return err
	}
	s.logger(
		"[stub] would submitVote oracle=%s price=%s calldata=0x%x",
		s.oracleAddress.Hex(),
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
