// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"time"
)

// Feeder runs the periodic price-fetch + vote-submit loop.
type Feeder struct {
	source    PriceSource
	submitter VoteSubmitter
	interval  time.Duration
	logger    func(format string, args ...any)
}

// NewFeeder builds a Feeder with the given inputs. interval must be
// positive.
func NewFeeder(
	source PriceSource,
	submitter VoteSubmitter,
	interval time.Duration,
	logger func(format string, args ...any),
) (*Feeder, error) {
	if source == nil || submitter == nil {
		return nil, fmt.Errorf("source and submitter must be non-nil")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be > 0, got %v", interval)
	}
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Feeder{
		source:    source,
		submitter: submitter,
		interval:  interval,
		logger:    logger,
	}, nil
}

// Run executes the feeder loop until ctx is cancelled. Each tick
// fetches a fresh price and dispatches a vote; transient errors are
// logged and the loop continues.
func (f *Feeder) Run(ctx context.Context) error {
	f.logger(
		"qsdfeeder starting: source=%s oracle=%s interval=%s",
		f.source.Name(),
		f.submitter.Address().Hex(),
		f.interval,
	)

	// Tick immediately on start so the first vote happens without
	// waiting a full interval.
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

// cycle is one fetch + submit pair. Errors are returned for the
// caller to log; ctx cancellation halts immediately without error.
func (f *Feeder) cycle(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	price, err := f.source.FetchUSDPerQRL(ctx)
	if err != nil {
		return fmt.Errorf("fetch price: %w", err)
	}
	if err := f.submitter.SubmitVote(ctx, price); err != nil {
		return fmt.Errorf("submit vote: %w", err)
	}
	f.logger("submitted vote: price=%s (1e18-scaled)", price.String())
	return nil
}
