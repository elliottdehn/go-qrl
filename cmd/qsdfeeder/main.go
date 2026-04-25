// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

// qsdfeeder is a validator-side daemon that periodically posts a
// QRL/USD price vote to the on-chain ValidatorOracle contract.
//
// When --keystore is provided, the daemon decrypts the wallet,
// connects to the RPC endpoint, and broadcasts real signed
// transactions. When --keystore is empty, it falls back to a stub
// submitter that only logs what it would send — useful for smoke
// tests and dev networks where you only care about price discovery.
//
// Usage:
//
//	qsdfeeder \
//	    --rpc=http://127.0.0.1:8545 \
//	    --oracle=Q0000000000000000000000000000000000010000 \
//	    --interval=30s \
//	    --price-source=static \
//	    --static-price=1.00 \
//	    --keystore=/etc/qrl/validator.json \
//	    --password-file=/etc/qrl/validator.pass
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/theQRL/go-qrl/accounts/keystore"
	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core"
	"github.com/theQRL/go-qrl/qrlclient"
)

type config struct {
	rpcURL       string
	oracleAddr   string
	interval     time.Duration
	targetOffset uint64
	priceSource  string
	staticPrice  string
	chainID      int64
	keystorePath string
	passwordFile string
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
		"vote-submission cadence (wall-clock); each tick targets head + --target-offset")
	flag.Uint64Var(&c.targetOffset, "target-offset", 1,
		"submit votes targeted at chain-head + this many blocks; 1 means \"the next block\"")
	flag.StringVar(&c.priceSource, "price-source", "static",
		"price source: static (only one implemented for now)")
	flag.StringVar(&c.staticPrice, "static-price", "1.00",
		"USD price per QRL when --price-source=static")
	flag.Int64Var(&c.chainID, "chain-id", 0,
		"chain ID for tx signing (auto-detected from RPC when 0)")
	flag.StringVar(&c.keystorePath, "keystore", "",
		"path to validator keystore JSON; empty means stub-submitter (logs only)")
	flag.StringVar(&c.passwordFile, "password-file", "",
		"path to file containing the keystore passphrase (required when --keystore is set)")
	flag.BoolVar(&c.verbose, "v", false, "verbose logging")
	flag.Parse()
	return c
}

// rpcBlockSource adapts a *qrlclient.Client into a BlockNumberSource.
type rpcBlockSource struct{ c *qrlclient.Client }

func (r rpcBlockSource) BlockNumber(ctx context.Context) (uint64, error) {
	return r.c.BlockNumber(ctx)
}

// loadKeyedSubmitter handles the keystore-backed submission path:
// decrypt the wallet, dial the RPC, resolve the chain ID, and return
// a fully-wired VoteSubmitter + BlockNumberSource backed by the same
// RPC connection. The returned closeFn must be called by the caller
// on shutdown to release that connection.
func loadKeyedSubmitter(
	ctx context.Context,
	c *config,
	oracle common.Address,
	logf func(format string, args ...any),
) (VoteSubmitter, BlockNumberSource, func(), error) {
	if c.passwordFile == "" {
		return nil, nil, nil, fmt.Errorf("--password-file is required when --keystore is set")
	}
	keyJSON, err := os.ReadFile(c.keystorePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read keystore: %w", err)
	}
	password, err := os.ReadFile(c.passwordFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read password file: %w", err)
	}
	key, err := keystore.DecryptKey(keyJSON, strings.TrimRight(string(password), "\r\n"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decrypt keystore: %w", err)
	}

	client, err := qrlclient.DialContext(ctx, c.rpcURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial rpc: %w", err)
	}

	chainID := big.NewInt(c.chainID)
	if c.chainID == 0 {
		chainID, err = client.ChainID(ctx)
		if err != nil {
			client.Close()
			return nil, nil, nil, fmt.Errorf("auto-detect chain id: %w", err)
		}
	}
	logf("loaded validator key: address=%s chain_id=%s", key.Address.Hex(), chainID.String())

	sub, err := NewKeyedSubmitter(client, key.Wallet, chainID, oracle, logf)
	if err != nil {
		client.Close()
		return nil, nil, nil, fmt.Errorf("build keyed submitter: %w", err)
	}
	return sub, rpcBlockSource{client}, client.Close, nil
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

	// Graceful shutdown on SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var (
		submitter VoteSubmitter
		blocks    BlockNumberSource
	)
	if c.keystorePath == "" {
		logger.Print("WARN keystore not configured — running with stub submitter (logs only)")
		submitter = NewStubSubmitter(oracle, logf)
		blocks = &counterBlockSource{}
	} else {
		sub, src, closeFn, err := loadKeyedSubmitter(ctx, c, oracle, logf)
		if err != nil {
			logger.Fatalf("load keyed submitter: %v", err)
		}
		defer closeFn()
		submitter = sub
		blocks = src
	}

	feeder, err := NewFeeder(source, blocks, submitter, c.interval, c.targetOffset, logf)
	if err != nil {
		logger.Fatalf("build feeder: %v", err)
	}

	if err := feeder.Run(ctx); err != nil {
		logger.Fatalf("feeder: %v", err)
	}
}
