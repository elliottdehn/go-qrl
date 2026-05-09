// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// PriceSource is anything that can supply a current QRL/USD price,
// scaled by 1e18 (so 1e18 == $1.00 / QRL). Implementations should
// honour ctx cancellation and surface their own errors verbatim.
type PriceSource interface {
	FetchUSDPerQRL(ctx context.Context) (*big.Int, error)
	Name() string
}

// staticPriceSource returns a fixed price every call. Useful for
// integration tests, dev networks, and smoke tests.
type staticPriceSource struct {
	price *big.Int
}

func (s *staticPriceSource) FetchUSDPerQRL(_ context.Context) (*big.Int, error) {
	// Return a fresh copy so callers can't mutate our state.
	return new(big.Int).Set(s.price), nil
}

func (s *staticPriceSource) Name() string { return "static" }

// parseUSDPriceTo1e18 converts a human price string ("1.50", "0.0001")
// into a 1e18-scaled integer. Whitespace is trimmed; a leading '$' is
// tolerated.
func parseUSDPriceTo1e18(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "$")
	if s == "" {
		return nil, fmt.Errorf("empty price")
	}

	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}

	// Split on the (optional) decimal point.
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}

	intVal, err := strconv.ParseUint(intPart, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("integer part %q: %w", intPart, err)
	}
	intBig := new(big.Int).SetUint64(intVal)
	scaled := new(big.Int).Mul(intBig, big.NewInt(1_000_000_000_000_000_000)) // 1e18

	if fracPart != "" {
		// Pad / truncate to 18 digits.
		if len(fracPart) > 18 {
			fracPart = fracPart[:18]
		}
		fracPart = fracPart + strings.Repeat("0", 18-len(fracPart))
		fracVal, err := strconv.ParseUint(fracPart, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("fractional part: %w", err)
		}
		scaled.Add(scaled, new(big.Int).SetUint64(fracVal))
	}

	if neg {
		return nil, fmt.Errorf("price must be positive")
	}
	if scaled.Sign() == 0 {
		return nil, fmt.Errorf("price must be > 0")
	}
	return scaled, nil
}

// NewStaticPriceSource builds a constant-price source. priceStr is a
// human USD figure ("1.00", "0.42", etc.).
func NewStaticPriceSource(priceStr string) (PriceSource, error) {
	scaled, err := parseUSDPriceTo1e18(priceStr)
	if err != nil {
		return nil, err
	}
	return &staticPriceSource{price: scaled}, nil
}
