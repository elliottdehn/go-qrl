// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

// qsdfeeder is a validator-side daemon that periodically posts a
// QRL/USD price vote to the on-chain ValidatorOracle contract.
//
// Status: scaffolding. The submission path is currently a stub that
// only logs what it would send. Real wallet signing + RPC dispatch
// will land in the next commit; this scaffold pins down flag/config
// surface, the price-source interface, and the run loop so the next
// change is purely additive.
//
// Usage:
//
//	qsdfeeder \
//	    --rpc=http://127.0.0.1:8545 \
//	    --oracle=Q0000000000000000000000000000000000010000 \
//	    --interval=30s \
//	    --price-source=static \
//	    --static-price=1.00
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core"
)

type config struct {
	rpcURL       string
	oracleAddr   string
	interval     time.Duration
	priceSource  string
	staticPrice  string
	chainID      int64
	keystorePath string
	verbose      bool
}

func parseFlags() *config {
	c := &config{}
	flag.StringVar(&c.rpcURL, "rpc", "http://127.0.0.1:8545",
		"go-qrl JSON-RPC endpoint")
	flag.StringVar(&c.oracleAddr, "oracle",
		core.ValidatorOracleAddress.Hex(),
		"ValidatorOracle contract address (default: reserved address)")
	flag.DurationVar(&c.interval, "interval", 30*time.Second,
		"vote-submission cadence")
	flag.StringVar(&c.priceSource, "price-source", "static",
		"price source: static (only one implemented for now)")
	flag.StringVar(&c.staticPrice, "static-price", "1.00",
		"USD price per QRL when --price-source=static")
	flag.Int64Var(&c.chainID, "chain-id", 0,
		"chain ID for tx signing (auto-detected from RPC when 0)")
	flag.StringVar(&c.keystorePath, "keystore", "",
		"path to validator keystore (TODO: not yet wired)")
	flag.BoolVar(&c.verbose, "v", false, "verbose logging")
	flag.Parse()
	return c
}

func buildPriceSource(c *config) (PriceSource, error) {
	switch c.priceSource {
	case "static":
		return NewStaticPriceSource(c.staticPrice)
	default:
		return nil, fmt.Errorf("unknown price source %q (supported: static)", c.priceSource)
	}
}

func parseAddress(s string) (common.Address, error) {
	// Tolerate both "0x" and QRL-style "Q" prefixes.
	switch {
	case strings.HasPrefix(s, "0x"), strings.HasPrefix(s, "0X"):
		s = s[2:]
	case strings.HasPrefix(s, "Q"):
		s = s[1:]
	}
	if len(s) != 2*common.AddressLength {
		return common.Address{}, fmt.Errorf("address must be %d hex chars, got %d", 2*common.AddressLength, len(s))
	}
	bytes := common.FromHex("0x" + s)
	if len(bytes) != common.AddressLength {
		return common.Address{}, fmt.Errorf("invalid hex address %q", s)
	}
	return common.BytesToAddress(bytes), nil
}

func main() {
	c := parseFlags()

	logger := log.New(os.Stderr, "qsdfeeder: ", log.LstdFlags|log.Lmicroseconds)
	logf := func(format string, args ...any) {
		logger.Printf(format, args...)
	}

	oracle, err := parseAddress(c.oracleAddr)
	if err != nil {
		logger.Fatalf("oracle address: %v", err)
	}

	source, err := buildPriceSource(c)
	if err != nil {
		logger.Fatalf("build price source: %v", err)
	}

	if c.keystorePath == "" {
		logger.Print("WARN keystore not configured — running with stub submitter (logs only)")
	}
	submitter := NewStubSubmitter(oracle, logf)

	feeder, err := NewFeeder(source, submitter, c.interval, logf)
	if err != nil {
		logger.Fatalf("build feeder: %v", err)
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := feeder.Run(ctx); err != nil {
		logger.Fatalf("feeder: %v", err)
	}
}
