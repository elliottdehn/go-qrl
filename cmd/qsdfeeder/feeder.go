// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"
)

// BlockNumberSource resolves the current chain head block number.
// Implementations: rpcBlockSource (wraps qrlclient) for real
// submission, counterBlockSource for stub-only smoke tests.
type BlockNumberSource interface {
	BlockNumber(ctx context.Context) (uint64, error)
}

// Feeder runs the periodic price-fetch + vote-submit loop.
type Feeder struct {
	source       PriceSource
	blocks       BlockNumberSource
	submitter    VoteSubmitter
	interval     time.Duration
	targetOffset uint64
	logger       func(format string, args ...any)
}

// NewFeeder builds a Feeder. interval must be positive; targetOffset
// is the number of blocks ahead of the current head that submitted
// votes target (typically 1, sometimes 2 for high-latency networks).
func NewFeeder(
	source PriceSource,
	blocks BlockNumberSource,
	submitter VoteSubmitter,
	interval time.Duration,
	targetOffset uint64,
	logger func(format string, args ...any),
) (*Feeder, error) {
	if source == nil || submitter == nil || blocks == nil {
		return nil, fmt.Errorf("source, blocks, and submitter must be non-nil")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be > 0, got %v", interval)
	}
	if targetOffset == 0 {
		return nil, fmt.Errorf("targetOffset must be >= 1")
	}
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Feeder{
		source:       source,
		blocks:       blocks,
		submitter:    submitter,
		interval:     interval,
		targetOffset: targetOffset,
		logger:       logger,
	}, nil
}

// Run executes the feeder loop until ctx is cancelled. Each tick
// fetches a fresh price + the chain head, then dispatches a vote
// targeted at head + targetOffset; transient errors are logged and
// the loop continues.
func (f *Feeder) Run(ctx context.Context) error {
	f.logger(
		"qsdfeeder starting: source=%s oracle=%s interval=%s target_offset=%d",
		f.source.Name(),
		f.submitter.Address().Hex(),
		f.interval,
		f.targetOffset,
	)

	if err := f.cycle(ctx); err != nil && ctx.Err() == nil {
		f.logger("first cycle: %v", err)
	}

	t := time.NewTicker(f.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			f.logger("qsdfeeder shutting down: %v", ctx.Err())
			return nil
		case <-t.C:
			if err := f.cycle(ctx); err != nil {
				f.logger("cycle: %v", err)
			}
		}
	}
}

// cycle is one head-lookup + price-fetch + submit triple.
func (f *Feeder) cycle(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	head, err := f.blocks.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("fetch head: %w", err)
	}
	price, err := f.source.FetchUSDPerQRL(ctx)
	if err != nil {
		return fmt.Errorf("fetch price: %w", err)
	}
	forBlock := new(big.Int).SetUint64(head + f.targetOffset)
	if err := f.submitter.SubmitVote(ctx, forBlock, price); err != nil {
		return fmt.Errorf("submit vote: %w", err)
	}
	return nil
}

// counterBlockSource hands out monotonically increasing block
// numbers, starting at 1. Used by the stub submission path in main()
// when there is no RPC endpoint to query — useful for smoke tests
// that exercise the wire-format path without bringing up a node.
type counterBlockSource struct {
	n atomic.Uint64
}

func (c *counterBlockSource) BlockNumber(_ context.Context) (uint64, error) {
	return c.n.Add(1), nil
}
