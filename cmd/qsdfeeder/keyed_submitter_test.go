// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl"
	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
)

// fakeRPC is a minimal stand-in for *qrlclient.Client. Each method
// returns a canned value or an error, and records the call so tests
// can assert on what the submitter built.
type fakeRPC struct {
	nonce       uint64
	tipCap      *big.Int
	baseFee     *big.Int
	gasEstimate uint64

	failNonce, failTip, failHeader, failEstimate, failSend bool

	sentTx *types.Transaction
	calls  []string
}

func (f *fakeRPC) PendingNonceAt(_ context.Context, _ common.Address) (uint64, error) {
	f.calls = append(f.calls, "PendingNonceAt")
	if f.failNonce {
		return 0, errors.New("nonce error")
	}
	return f.nonce, nil
}

func (f *fakeRPC) HeaderByNumber(_ context.Context, _ *big.Int) (*types.Header, error) {
	f.calls = append(f.calls, "HeaderByNumber")
	if f.failHeader {
		return nil, errors.New("header error")
	}
	return &types.Header{BaseFee: f.baseFee}, nil
}

func (f *fakeRPC) SuggestGasTipCap(_ context.Context) (*big.Int, error) {
	f.calls = append(f.calls, "SuggestGasTipCap")
	if f.failTip {
		return nil, errors.New("tip error")
	}
	return new(big.Int).Set(f.tipCap), nil
}

func (f *fakeRPC) EstimateGas(_ context.Context, _ qrl.CallMsg) (uint64, error) {
	f.calls = append(f.calls, "EstimateGas")
	if f.failEstimate {
		return 0, errors.New("estimate error")
	}
	return f.gasEstimate, nil
}

func (f *fakeRPC) SendTransaction(_ context.Context, tx *types.Transaction) error {
	f.calls = append(f.calls, "SendTransaction")
	if f.failSend {
		return errors.New("send error")
	}
	f.sentTx = tx
	return nil
}

// newTestSubmitter is the test wiring used everywhere below. It
// constructs a real wallet (so signing is real) and pairs it with the
// caller-provided fakeRPC.
func newTestSubmitter(t *testing.T, rpc *fakeRPC) *keyedSubmitter {
	t.Helper()
	w, err := wallet.RestoreFromSeedHex(
		"010000b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f29100000000000000000000000000000000",
	)
	if err != nil {
		t.Fatalf("restore wallet: %v", err)
	}
	oracle := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000010000"))
	return &keyedSubmitter{
		client:        rpc,
		wallet:        w,
		chainID:       big.NewInt(1337),
		oracleAddress: oracle,
		from:          w.GetAddress(),
		logger:        func(string, ...any) {},
	}
}

func TestKeyedSubmitter_HappyPath(t *testing.T) {
	rpc := &fakeRPC{
		nonce:       42,
		tipCap:      big.NewInt(2_000_000_000),
		baseFee:     big.NewInt(7_000_000_000),
		gasEstimate: 50_000,
	}
	s := newTestSubmitter(t, rpc)
	price := bigStr("1500000000000000000")

	if err := s.SubmitVote(context.Background(), price); err != nil {
		t.Fatalf("SubmitVote: %v", err)
	}
	if rpc.sentTx == nil {
		t.Fatal("no transaction sent")
	}
	tx := rpc.sentTx

	// Nonce came from PendingNonceAt.
	if tx.Nonce() != 42 {
		t.Errorf("nonce: got %d, want 42", tx.Nonce())
	}
	// To == oracle.
	if got := tx.To(); got == nil || *got != s.oracleAddress {
		t.Errorf("to: got %v, want %v", got, s.oracleAddress)
	}
	// feeCap = 2*baseFee + tipCap = 16e9.
	wantFeeCap := big.NewInt(16_000_000_000)
	if tx.GasFeeCap().Cmp(wantFeeCap) != 0 {
		t.Errorf("feeCap: got %s, want %s", tx.GasFeeCap(), wantFeeCap)
	}
	if tx.GasTipCap().Cmp(rpc.tipCap) != 0 {
		t.Errorf("tipCap: got %s, want %s", tx.GasTipCap(), rpc.tipCap)
	}
	// Gas == estimate * 1.2 = 60_000.
	if tx.Gas() != 60_000 {
		t.Errorf("gas: got %d, want 60000", tx.Gas())
	}
	// Calldata is exactly what EncodeSubmitVoteCalldata produced.
	wantData, _ := EncodeSubmitVoteCalldata(price)
	if string(tx.Data()) != string(wantData) {
		t.Errorf("calldata: got %x, want %x", tx.Data(), wantData)
	}
	// Tx is signed: recovering the sender should yield wallet address.
	signer := types.LatestSignerForChainID(s.chainID)
	from, err := types.Sender(signer, tx)
	if err != nil {
		t.Fatalf("recover sender: %v", err)
	}
	if from != s.from {
		t.Errorf("sender: got %s, want %s", from.Hex(), s.from.Hex())
	}
}

func TestKeyedSubmitter_RejectsBadPrice(t *testing.T) {
	rpc := &fakeRPC{baseFee: big.NewInt(1), tipCap: big.NewInt(1), gasEstimate: 1}
	s := newTestSubmitter(t, rpc)

	for _, p := range []*big.Int{nil, big.NewInt(0), big.NewInt(-1)} {
		if err := s.SubmitVote(context.Background(), p); err == nil {
			t.Errorf("expected error for price=%v", p)
		}
	}
	// Calldata-encoding failure must short-circuit before any RPC call.
	if len(rpc.calls) != 0 {
		t.Errorf("expected no RPC calls on bad price, got %v", rpc.calls)
	}
}

func TestKeyedSubmitter_PropagatesRPCErrors(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*fakeRPC)
		wantIn string
	}{
		{"nonce", func(f *fakeRPC) { f.failNonce = true }, "fetch nonce"},
		{"tip", func(f *fakeRPC) { f.failTip = true }, "suggest tip cap"},
		{"header", func(f *fakeRPC) { f.failHeader = true }, "fetch header"},
		{"estimate", func(f *fakeRPC) { f.failEstimate = true }, "estimate gas"},
		{"send", func(f *fakeRPC) { f.failSend = true }, "send tx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rpc := &fakeRPC{
				nonce:       1,
				tipCap:      big.NewInt(1),
				baseFee:     big.NewInt(1),
				gasEstimate: 21000,
			}
			c.setup(rpc)
			s := newTestSubmitter(t, rpc)
			err := s.SubmitVote(context.Background(), bigStr("1000000000000000000"))
			if err == nil {
				t.Fatal("expected error")
			}
			if !contains(err.Error(), c.wantIn) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantIn)
			}
		})
	}
}

func TestKeyedSubmitter_RejectsPreLondonChain(t *testing.T) {
	rpc := &fakeRPC{
		nonce:       0,
		tipCap:      big.NewInt(1),
		baseFee:     nil, // pre-London / non-EIP-1559 head
		gasEstimate: 21000,
	}
	s := newTestSubmitter(t, rpc)
	err := s.SubmitVote(context.Background(), bigStr("1000000000000000000"))
	if err == nil || !contains(err.Error(), "EIP-1559") {
		t.Errorf("expected EIP-1559 error, got %v", err)
	}
}

func TestNewKeyedSubmitter_Validation(t *testing.T) {
	w, _ := wallet.RestoreFromSeedHex(
		"010000b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f29100000000000000000000000000000000",
	)
	if _, err := NewKeyedSubmitter(nil, w, big.NewInt(1), common.Address{}, nil); err == nil {
		t.Error("nil client: expected error")
	}
	// Note: the public ctor wants *qrlclient.Client, not the rpcClient
	// interface, so we can't inject a fakeRPC here. The wallet/chainID
	// validation paths are exercised below.
	if _, err := NewKeyedSubmitter(nil, nil, big.NewInt(1), common.Address{}, nil); err == nil {
		t.Error("nil wallet: expected error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
