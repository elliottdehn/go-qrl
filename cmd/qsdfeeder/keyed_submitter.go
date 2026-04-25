// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"math/big"

	"github.com/theQRL/go-qrl"
	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
	"github.com/theQRL/go-qrl/qrlclient"
)

// rpcClient is the subset of qrlclient.Client used by keyedSubmitter.
// Pulling it out behind an interface keeps the submitter unit-testable
// without spinning up a full RPC mock; the unit tests cover calldata
// encoding paths, while end-to-end submission is exercised by
// integration tests against a dev node.
type rpcClient interface {
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, msg qrl.CallMsg) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
}

// keyedSubmitter signs and broadcasts submitVote transactions to a
// running go-qrl node.
type keyedSubmitter struct {
	client        rpcClient
	wallet        wallet.Wallet
	chainID       *big.Int
	oracleAddress common.Address
	from          common.Address
	logger        func(format string, args ...any)
}

// NewKeyedSubmitter constructs a real on-chain VoteSubmitter. The
// caller owns the qrlclient.Client; this submitter does not close it.
func NewKeyedSubmitter(
	client *qrlclient.Client,
	w wallet.Wallet,
	chainID *big.Int,
	oracle common.Address,
	logger func(format string, args ...any),
) (VoteSubmitter, error) {
	if client == nil {
		return nil, fmt.Errorf("client must be non-nil")
	}
	if w == nil {
		return nil, fmt.Errorf("wallet must be non-nil")
	}
	if chainID == nil || chainID.Sign() <= 0 {
		return nil, fmt.Errorf("chainID must be positive")
	}
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &keyedSubmitter{
		client:        client,
		wallet:        w,
		chainID:       new(big.Int).Set(chainID),
		oracleAddress: oracle,
		from:          w.GetAddress(),
		logger:        logger,
	}, nil
}

// SubmitVote builds, signs, and broadcasts a submitVote(uint256) tx.
// Errors at any stage are returned verbatim so the run loop can log
// and retry on the next tick.
func (s *keyedSubmitter) SubmitVote(ctx context.Context, priceUsd1e18 *big.Int) error {
	calldata, err := EncodeSubmitVoteCalldata(priceUsd1e18)
	if err != nil {
		return fmt.Errorf("encode calldata: %w", err)
	}

	nonce, err := s.client.PendingNonceAt(ctx, s.from)
	if err != nil {
		return fmt.Errorf("fetch nonce: %w", err)
	}

	tipCap, err := s.client.SuggestGasTipCap(ctx)
	if err != nil {
		return fmt.Errorf("suggest tip cap: %w", err)
	}

	header, err := s.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("fetch header: %w", err)
	}
	if header.BaseFee == nil {
		return fmt.Errorf("chain head has no base fee; EIP-1559 not active")
	}
	// feeCap = 2 * baseFee + tipCap is the conventional headroom for
	// one block of base-fee growth (12.5%/block max), well-padded.
	feeCap := new(big.Int).Mul(header.BaseFee, big.NewInt(2))
	feeCap.Add(feeCap, tipCap)

	to := s.oracleAddress
	gas, err := s.client.EstimateGas(ctx, qrl.CallMsg{
		From:      s.from,
		To:        &to,
		GasFeeCap: feeCap,
		GasTipCap: tipCap,
		Data:      calldata,
	})
	if err != nil {
		return fmt.Errorf("estimate gas: %w", err)
	}
	// 20% headroom — submitVote() touches dynamic state (median
	// recompute on quorum-completing votes) and EstimateGas can
	// under-shoot in those branches.
	gas = gas + gas/5

	tx, err := types.SignNewTx(
		s.wallet,
		types.LatestSignerForChainID(s.chainID),
		&types.DynamicFeeTx{
			ChainID:   new(big.Int).Set(s.chainID),
			Nonce:     nonce,
			GasTipCap: tipCap,
			GasFeeCap: feeCap,
			Gas:       gas,
			To:        &to,
			Value:     big.NewInt(0),
			Data:      calldata,
		},
	)
	if err != nil {
		return fmt.Errorf("sign tx: %w", err)
	}

	if err := s.client.SendTransaction(ctx, tx); err != nil {
		return fmt.Errorf("send tx: %w", err)
	}

	s.logger(
		"submitted vote: tx=%s from=%s nonce=%d price=%s gas=%d feeCap=%s tipCap=%s",
		tx.Hash().Hex(),
		s.from.Hex(),
		nonce,
		priceUsd1e18.String(),
		gas,
		feeCap.String(),
		tipCap.String(),
	)
	return nil
}

func (s *keyedSubmitter) Address() common.Address {
	return s.oracleAddress
}
