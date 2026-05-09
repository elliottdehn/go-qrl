// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package core

import (
	"math/big"
	"testing"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core/rawdb"
	"github.com/theQRL/go-qrl/core/state"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/core/vm"
	"github.com/theQRL/go-qrl/crypto"
	"github.com/theQRL/go-qrl/params"
)

// ----------------------------------------------------------------------------
// Helpers shared across paymaster integration tests.
// ----------------------------------------------------------------------------

// erc20BalanceSlot returns the storage slot where OpenZeppelin v5
// ERC-20 stores _balances[holder]. Layout assumption: _balances is
// the first storage variable of the inheritance chain (slot 0).
func erc20BalanceSlot(holder common.Address) common.Hash {
	const balancesMapSlot = 0
	return crypto.Keccak256Hash(
		common.LeftPadBytes(holder.Bytes(), 32),
		common.LeftPadBytes(big.NewInt(balancesMapSlot).Bytes(), 32),
	)
}

// erc20AllowanceSlot returns the storage slot where OpenZeppelin v5
// ERC-20 stores _allowances[owner][spender]. Slot 1 of the contract.
func erc20AllowanceSlot(owner, spender common.Address) common.Hash {
	const allowancesMapSlot = 1
	inner := crypto.Keccak256Hash(
		common.LeftPadBytes(owner.Bytes(), 32),
		common.LeftPadBytes(big.NewInt(allowancesMapSlot).Bytes(), 32),
	)
	return crypto.Keccak256Hash(
		common.LeftPadBytes(spender.Bytes(), 32),
		inner.Bytes(),
	)
}

// uintToHash encodes a uint256 as a 32-byte right-aligned hash.
func uintToHash(n *big.Int) common.Hash {
	return common.BytesToHash(common.LeftPadBytes(n.Bytes(), 32))
}

// newPredeployedState builds a fresh in-memory state DB with the
// QSD stability-layer predeploys loaded and `owner` set as the
// oracle owner. The returned StateDB is committed to a memory rawdb
// so it can satisfy the QRVM's StateDB interface.
func newPredeployedState(t *testing.T, oracleOwner common.Address) *state.StateDB {
	t.Helper()
	alloc := GenesisAlloc{}
	AddQSDStabilityLayer(alloc, DefaultQSDPredeployParams(oracleOwner))

	db := rawdb.NewMemoryDatabase()
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabase(db), nil)
	if err != nil {
		t.Fatalf("new state: %v", err)
	}
	for addr, acc := range alloc {
		sdb.CreateAccount(addr)
		sdb.SetCode(addr, acc.Code)
		for slot, val := range acc.Storage {
			sdb.SetState(addr, slot, val)
		}
		if acc.Balance != nil && acc.Balance.Sign() > 0 {
			sdb.AddBalance(addr, acc.Balance)
		}
	}
	return sdb
}

// applyPaymasterMessage runs `msg` through the state-transition
// engine against `sdb`, using a minimal block context. Returns the
// execution result so callers can assert on gasUsed / vmerr.
func applyPaymasterMessage(t *testing.T, sdb *state.StateDB, msg *Message, baseFee *big.Int) *ExecutionResult {
	t.Helper()
	chainCfg := params.AllDevChainProtocolChanges
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b")),
		BlockNumber: big.NewInt(1),
		Time:        1,
		GasLimit:    30_000_000,
		BaseFee:     new(big.Int).Set(baseFee),
	}
	txCtx := vm.TxContext{
		Origin:   msg.From,
		GasPrice: msg.GasPrice,
	}
	qrvm := vm.NewQRVM(blockCtx, txCtx, sdb, chainCfg, vm.Config{})
	gp := new(GasPool).AddGas(blockCtx.GasLimit)

	result, err := ApplyMessage(qrvm, msg, gp)
	if err != nil {
		t.Fatalf("ApplyMessage: %v", err)
	}
	return result
}

// ----------------------------------------------------------------------------
// Tests.
// ----------------------------------------------------------------------------

func TestPaymaster_HappyPath_FeeFlowsToCoinbase(t *testing.T) {
	var (
		alice    = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
		bob      = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000b0b"))
		coinbase = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))
	)

	sdb := newPredeployedState(t, alice)

	// Pre-populate alice's iQRL balance and allowance to PayWithIQRL.
	// 1000 iQRL is plenty to cover any plausible gas budget.
	aliceIQRL, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24 (1M iQRL @ 1e18 scale)
	sdb.SetState(InverseQRLAddress, erc20BalanceSlot(alice), uintToHash(aliceIQRL))
	sdb.SetState(InverseQRLAddress,
		erc20AllowanceSlot(alice, PayWithIQRLAddress),
		uintToHash(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))), // type(uint256).max
	)
	// totalSupply must include alice's balance for ERC-20 invariants
	// (transfer subtracts here too on transfers between accounts).
	sdb.SetState(InverseQRLAddress, common.HexToHash("0x"+
		"0000000000000000000000000000000000000000000000000000000000000002"), // slot 2 = _totalSupply
		uintToHash(aliceIQRL))

	// Pre-state snapshots.
	aliceIQRLBefore := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(alice)).Big()
	coinbaseIQRLBefore := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(coinbase)).Big()

	// Build the paymaster message: alice sends 0 native to bob.
	gasPrice := big.NewInt(1_000_000_000) // 1 gwei
	msg := &Message{
		From:      alice,
		To:        &bob,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  100_000,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Data:      nil,
		Paymaster: &PayWithIQRLAddress,
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))
	if result.Failed() {
		t.Fatalf("execution failed: %v", result.Err)
	}

	// Post-state assertions.
	aliceIQRLAfter := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(alice)).Big()
	coinbaseIQRLAfter := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(coinbase)).Big()
	pmIQRL := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(PayWithIQRLAddress)).Big()

	// 1. Alice's iQRL went down by exactly the actual fee.
	expectedFee := new(big.Int).Mul(new(big.Int).SetUint64(result.UsedGas), gasPrice)
	aliceDelta := new(big.Int).Sub(aliceIQRLBefore, aliceIQRLAfter)
	if aliceDelta.Cmp(expectedFee) != 0 {
		t.Errorf("alice iQRL delta: got %s, want %s (gasUsed=%d * gasPrice=%s)",
			aliceDelta, expectedFee, result.UsedGas, gasPrice)
	}
	// 2. Coinbase received exactly the actual fee.
	coinbaseDelta := new(big.Int).Sub(coinbaseIQRLAfter, coinbaseIQRLBefore)
	if coinbaseDelta.Cmp(expectedFee) != 0 {
		t.Errorf("coinbase iQRL delta: got %s, want %s", coinbaseDelta, expectedFee)
	}
	// 3. Paymaster contract holds zero iQRL afterwards (escrow fully
	//    settled). A non-zero residual indicates settle() didn't drain
	//    correctly.
	if pmIQRL.Sign() != 0 {
		t.Errorf("paymaster residual iQRL: got %s, want 0", pmIQRL)
	}
	// 4. Native balance of alice is unchanged (paymaster route doesn't
	//    touch it; value=0).
	if got := sdb.GetBalance(alice); got.Sign() != 0 {
		t.Errorf("alice native balance: got %s, want 0", got)
	}
}

func TestPaymaster_RejectsNonAllowlistedPaymaster(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	sdb := newPredeployedState(t, alice)

	// Allow alice some native QRL to satisfy any nonce/EOA prerequisites.
	sdb.SetNonce(alice, 0)

	bogus := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000beef"))
	gasPrice := big.NewInt(1)
	msg := &Message{
		From:      alice,
		To:        &alice,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  100_000,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Paymaster: &bogus, // not on allowlist
	}

	chainCfg := params.AllDevChainProtocolChanges
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		BlockNumber: big.NewInt(1),
		Time:        1,
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(0),
	}
	qrvm := vm.NewQRVM(blockCtx, vm.TxContext{Origin: alice, GasPrice: gasPrice}, sdb, chainCfg, vm.Config{})
	gp := new(GasPool).AddGas(blockCtx.GasLimit)

	if _, err := ApplyMessage(qrvm, msg, gp); err == nil {
		t.Fatal("expected error for non-allowlisted paymaster")
	}
}

func TestPaymaster_RejectsWhenPayerLacksAllowance(t *testing.T) {
	alice := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
	sdb := newPredeployedState(t, alice)

	// alice has plenty of iQRL — but never approved PayWithIQRL.
	aliceIQRL, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24
	sdb.SetState(InverseQRLAddress, erc20BalanceSlot(alice), uintToHash(aliceIQRL))
	sdb.SetState(InverseQRLAddress, common.HexToHash("0x"+
		"0000000000000000000000000000000000000000000000000000000000000002"),
		uintToHash(aliceIQRL))

	bob := common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000b0b"))
	gasPrice := big.NewInt(1_000_000_000)
	msg := &Message{
		From:      alice,
		To:        &bob,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  100_000,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Paymaster: &PayWithIQRLAddress,
	}

	chainCfg := params.AllDevChainProtocolChanges
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		BlockNumber: big.NewInt(1),
		Time:        1,
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(0),
	}
	qrvm := vm.NewQRVM(blockCtx, vm.TxContext{Origin: alice, GasPrice: gasPrice}, sdb, chainCfg, vm.Config{})
	gp := new(GasPool).AddGas(blockCtx.GasLimit)

	_, err := ApplyMessage(qrvm, msg, gp)
	if err == nil {
		t.Fatal("expected escrow failure when allowance is missing")
	}
}

// TestPaymaster_BaseFeeBurn_ReducesTotalSupply exercises the
// EIP-1559-equivalent burn path: with a non-zero baseFee and a
// healthy oracle, gasUsed * baseFeeIQRL of the user's iQRL is
// destroyed via iqrl.burn rather than paid to coinbase. iqrl
// totalSupply drops by exactly that amount.
func TestPaymaster_BaseFeeBurn_ReducesTotalSupply(t *testing.T) {
	var (
		alice    = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
		bob      = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000b0b"))
		coinbase = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))
	)

	sdb := newPredeployedState(t, alice)

	// Make alice a validator and post her own submitVote so the
	// oracle is healthy with a known price. Use $1.00 (p_scaled =
	// 1e18) so baseFeeIQRL = baseFeeQRL exactly.
	registerValidator(t, sdb, alice)
	pegPrice, _ := new(big.Int).SetString("1000000000000000000", 10)
	// Pack the vote slot directly: bytes [16:32] = price,
	// bytes [8:16] = blockNumber.
	var voteSlot common.Hash
	priceB := common.LeftPadBytes(pegPrice.Bytes(), 16)
	copy(voteSlot[16:32], priceB)
	voteSlot[15] = 1 // blockNumber low byte
	sdb.SetState(InverseQRLAddress, common.HexToHash("0x"+
		"0000000000000000000000000000000000000000000000000000000000000002"),
		common.Hash{}) // unused, illustrative
	// Vote storage is in ValidatorOracle, not iQRL.
	sdb.SetState(ValidatorOracleAddress, ValidatorVoteStorageSlot(alice), voteSlot)

	// Confirm the off-chain price computation agrees.
	if got, healthy := ComputeOraclePrice(sdb, 1); !healthy || got.Cmp(pegPrice) != 0 {
		t.Fatalf("oracle pre-state: got (%s, healthy=%v), want ($1.00, true)", got, healthy)
	}

	// Fund alice's iQRL and approve the paymaster.
	aliceIQRL, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24
	sdb.SetState(InverseQRLAddress, erc20BalanceSlot(alice), uintToHash(aliceIQRL))
	sdb.SetState(InverseQRLAddress,
		erc20AllowanceSlot(alice, PayWithIQRLAddress),
		uintToHash(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))),
	)
	totalSupplyBefore := aliceIQRL
	sdb.SetState(InverseQRLAddress, common.HexToHash("0x"+
		"0000000000000000000000000000000000000000000000000000000000000002"),
		uintToHash(totalSupplyBefore))

	// Build the paymaster tx with a non-zero base fee.
	baseFee := big.NewInt(2_000_000_000) // 2 gwei
	gasPrice := big.NewInt(5_000_000_000) // 5 gwei (i.e. tipCap = 3, feeCap = 5)
	msg := &Message{
		From:      alice,
		To:        &bob,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  100_000,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: new(big.Int).Sub(gasPrice, baseFee), // tipCap = 3 gwei
		Paymaster: &PayWithIQRLAddress,
	}

	result := applyPaymasterMessage(t, sdb, msg, baseFee)
	if result.Failed() {
		t.Fatalf("execution failed: %v", result.Err)
	}

	// Expected split:
	//   burn = gasUsed * baseFeeIQRL = gasUsed * baseFeeQRL (at peg)
	//   tip  = gasUsed * (effGasPrice - baseFeeIQRL) = gasUsed * tipCap
	expectedBurn := new(big.Int).Mul(new(big.Int).SetUint64(result.UsedGas), baseFee)
	expectedTip := new(big.Int).Mul(new(big.Int).SetUint64(result.UsedGas), msg.GasTipCap)

	totalSupplyAfter := sdb.GetState(InverseQRLAddress, common.HexToHash("0x"+
		"0000000000000000000000000000000000000000000000000000000000000002")).Big()
	burned := new(big.Int).Sub(totalSupplyBefore, totalSupplyAfter)
	if burned.Cmp(expectedBurn) != 0 {
		t.Errorf("iqrl burned: got %s, want %s", burned, expectedBurn)
	}

	// Coinbase received exactly the tip portion.
	gotTip := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(coinbase)).Big()
	if gotTip.Cmp(expectedTip) != 0 {
		t.Errorf("coinbase tip: got %s, want %s", gotTip, expectedTip)
	}

	// Alice paid exactly burn+tip out of her iQRL balance.
	aliceAfter := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(alice)).Big()
	paid := new(big.Int).Sub(aliceIQRL, aliceAfter)
	expectedPaid := new(big.Int).Add(expectedBurn, expectedTip)
	if paid.Cmp(expectedPaid) != 0 {
		t.Errorf("alice paid: got %s, want %s", paid, expectedPaid)
	}

	// Paymaster contract drained.
	if pm := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(PayWithIQRLAddress)).Big(); pm.Sign() != 0 {
		t.Errorf("paymaster residual %s, want 0", pm)
	}
}

func TestPaymaster_VmRevert_StillSettles(t *testing.T) {
	// When the user's call reverts inside the EVM, the paymaster
	// must still settle correctly: alice still pays for the gas that
	// was actually consumed up to the revert; the rest is refunded.
	var (
		alice    = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
		coinbase = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))
	)

	sdb := newPredeployedState(t, alice)
	aliceIQRL, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	sdb.SetState(InverseQRLAddress, erc20BalanceSlot(alice), uintToHash(aliceIQRL))
	sdb.SetState(InverseQRLAddress,
		erc20AllowanceSlot(alice, PayWithIQRLAddress),
		uintToHash(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))),
	)
	sdb.SetState(InverseQRLAddress, common.HexToHash("0x"+
		"0000000000000000000000000000000000000000000000000000000000000002"),
		uintToHash(aliceIQRL))

	// Deploy a tiny contract whose only function is to revert.
	// Bytecode: PUSH1 0x00 PUSH1 0x00 REVERT  (0x60005ffd... → use 0x6000_6000_fd)
	revertCodeAddr := common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000dead"))
	sdb.SetCode(revertCodeAddr, []byte{0x60, 0x00, 0x60, 0x00, 0xfd})

	aliceIQRLBefore := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(alice)).Big()

	gasPrice := big.NewInt(1_000_000_000)
	msg := &Message{
		From:      alice,
		To:        &revertCodeAddr,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  100_000,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Paymaster: &PayWithIQRLAddress,
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))

	// EVM error is expected — the called code reverts.
	if !result.Failed() {
		t.Fatalf("expected vm revert, got nil err")
	}

	aliceIQRLAfter := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(alice)).Big()
	coinbaseIQRL := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(coinbase)).Big()
	pmIQRL := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(PayWithIQRLAddress)).Big()

	expectedFee := new(big.Int).Mul(new(big.Int).SetUint64(result.UsedGas), gasPrice)
	aliceDelta := new(big.Int).Sub(aliceIQRLBefore, aliceIQRLAfter)

	if aliceDelta.Cmp(expectedFee) != 0 {
		t.Errorf("alice paid %s, want %s on revert", aliceDelta, expectedFee)
	}
	if coinbaseIQRL.Cmp(expectedFee) != 0 {
		t.Errorf("coinbase received %s, want %s on revert", coinbaseIQRL, expectedFee)
	}
	if pmIQRL.Sign() != 0 {
		t.Errorf("paymaster residual %s, want 0", pmIQRL)
	}
}

func TestPaymaster_PartialRefund_GasUsedLessThanLimit(t *testing.T) {
	var (
		alice    = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000a11c"))
		bob      = common.BytesToAddress(common.FromHex("0x0000000000000000000000000000000000000b0b"))
		coinbase = common.BytesToAddress(common.FromHex("0x000000000000000000000000000000000000c01b"))
	)
	sdb := newPredeployedState(t, alice)

	aliceIQRL, _ := new(big.Int).SetString("1000000000000000000000000", 10) // 1e24
	sdb.SetState(InverseQRLAddress, erc20BalanceSlot(alice), uintToHash(aliceIQRL))
	sdb.SetState(InverseQRLAddress,
		erc20AllowanceSlot(alice, PayWithIQRLAddress),
		uintToHash(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))),
	)
	sdb.SetState(InverseQRLAddress, common.HexToHash("0x"+
		"0000000000000000000000000000000000000000000000000000000000000002"),
		uintToHash(aliceIQRL))

	aliceIQRLBefore := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(alice)).Big()

	// Generous gas limit — bare value-only call uses ~21000.
	const gasLimit = 500_000
	gasPrice := big.NewInt(1_000_000_000)
	msg := &Message{
		From:      alice,
		To:        &bob,
		Nonce:     0,
		Value:     big.NewInt(0),
		GasLimit:  gasLimit,
		GasPrice:  gasPrice,
		GasFeeCap: gasPrice,
		GasTipCap: gasPrice,
		Paymaster: &PayWithIQRLAddress,
	}

	result := applyPaymasterMessage(t, sdb, msg, big.NewInt(0))
	if result.Failed() {
		t.Fatalf("execution failed: %v", result.Err)
	}
	if result.UsedGas >= gasLimit {
		t.Fatalf("expected gasUsed < limit, got %d / %d", result.UsedGas, gasLimit)
	}

	aliceIQRLAfter := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(alice)).Big()
	coinbaseIQRL := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(coinbase)).Big()
	pmIQRL := sdb.GetState(InverseQRLAddress, erc20BalanceSlot(PayWithIQRLAddress)).Big()

	expectedFee := new(big.Int).Mul(new(big.Int).SetUint64(result.UsedGas), gasPrice)
	aliceDelta := new(big.Int).Sub(aliceIQRLBefore, aliceIQRLAfter)

	// Alice paid only the actual fee (refund landed): gasUsed << gasLimit.
	if aliceDelta.Cmp(expectedFee) != 0 {
		t.Errorf("alice paid %s, want %s (gasUsed=%d, limit=%d)",
			aliceDelta, expectedFee, result.UsedGas, gasLimit)
	}
	if coinbaseIQRL.Cmp(expectedFee) != 0 {
		t.Errorf("coinbase received %s, want %s", coinbaseIQRL, expectedFee)
	}
	if pmIQRL.Sign() != 0 {
		t.Errorf("paymaster residual %s, want 0", pmIQRL)
	}
}
