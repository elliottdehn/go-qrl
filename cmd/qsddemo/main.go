// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

// qsddemo walks the QSD stability layer end-to-end against a running
// gqrl --dev node: mint iQRL, deposit into the QSD pool, swap on the
// pool with fees paid in iQRL via a type-0x04 paymaster transaction,
// and redeem QSD back to native + iQRL. Prints state at each step.
//
// See docs/qsd-demo.md for the operator-facing walkthrough.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theQRL/go-qrl"
	"github.com/theQRL/go-qrl/accounts/keystore"
	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/core"
	"github.com/theQRL/go-qrl/core/types"
	"github.com/theQRL/go-qrl/crypto"
	"github.com/theQRL/go-qrl/crypto/pqcrypto/wallet"
	"github.com/theQRL/go-qrl/qrlclient"
)

// addressOf normalizes wallet.GetAddress (which returns a value
// [20]byte) into a common.Address without the "cannot slice
// unaddressable value" Go-version foot-gun.
func addressOf(w wallet.Wallet) common.Address {
	a := w.GetAddress()
	return common.BytesToAddress(a[:])
}

// One QRL / iQRL / QSD with the standard 18 decimals.
var oneE18 = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// Function selectors used by the demo. Computed at init so a typo
// surfaces at startup, not mid-flow.
var (
	selErc20Approve   = sel("approve(address,uint256)")
	selErc20BalanceOf = sel("balanceOf(address)")
	selIqrlMint       = sel("mint(uint256)")
	selOraclePrice    = sel("price()")
	selOracleHealthy  = sel("healthy()")
	selQsdDeposit     = sel("deposit(uint256,uint256)")
	selQsdSwapIqrl    = sel("swapIqrlForQrl(uint256,uint256)")
	selQsdRedeem      = sel("redeem(uint256)")
	selQsdPoolQRL     = sel("poolQRL()")
	selQsdPoolIQRL    = sel("poolIQRL()")
)

func sel(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

func main() {
	var (
		rpcURL  = flag.String("rpc", "http://127.0.0.1:8545", "go-qrl JSON-RPC endpoint")
		datadir = flag.String("datadir", "/tmp/qsd-demo/data", "datadir hosting the dev faucet keystore (--dev default: /tmp/qsd-demo/data)")
		passwd  = flag.String("password", "", "passphrase for the dev keystore (default: empty, matches --dev)")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := run(ctx, *rpcURL, *datadir, *passwd); err != nil {
		fmt.Fprintf(os.Stderr, "qsddemo: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, rpcURL, datadir, passwd string) error {
	cli, err := qrlclient.Dial(rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer cli.Close()

	chainID, err := cli.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("chain id: %w", err)
	}
	fmt.Printf("connected to %s, chainID=%s\n", rpcURL, chainID)

	faucet, err := loadDevWallet(datadir, passwd)
	if err != nil {
		return fmt.Errorf("load dev wallet: %w", err)
	}
	fmt.Printf("dev faucet: %s\n", addressOf(faucet))

	// The demo runs from a fresh ephemeral wallet, not the faucet.
	// The faucet is the chain's sole validator and is concurrently
	// signing submitVote txs via qsdfeeder; using it here would
	// create nonce collisions.
	w, err := wallet.Generate(wallet.ML_DSA_87)
	if err != nil {
		return fmt.Errorf("generate demo wallet: %w", err)
	}
	from := addressOf(w)
	fmt.Printf("demo wallet: %s\n", from)

	step("oracle status")
	if err := oracleStatus(ctx, cli); err != nil {
		return err
	}

	// Fund the demo wallet from the faucet. 5 QRL is enough for
	// mint + deposit + small swap + redeem and all the gas they
	// use.
	step("fund demo wallet from faucet (5 QRL)")
	to := from
	if err := txAndWaitWith(ctx, cli, faucet, chainID, &to,
		new(big.Int).Mul(big.NewInt(5), oneE18), nil,
	); err != nil {
		return fmt.Errorf("fund demo wallet: %w", err)
	}

	step("starting balances")
	if err := printBalances(ctx, cli, from, "before mint"); err != nil {
		return err
	}

	// 1. Mint iQRL by destroying QRL. The per-block mint cap is
	//    max(totalSupply * 25 / 1_000_000, 1 ether), so on a fresh
	//    chain we can mint at most 1 iQRL per block. Stay just
	//    under the cap.
	mintAmount := new(big.Int).Mul(big.NewInt(900_000_000_000_000_000), big.NewInt(1)) // 0.9 iQRL
	step("mint 0.9 iQRL (pays QRL)")
	if err := txAndWaitWith(ctx, cli, w, chainID, &core.InverseQRLAddress,
		// At p=$1.00 the cost is iqrlAmount/p^2 + 0.5% fee. Send 2 QRL
		// as a slack-padded msg.value; the contract refunds the surplus.
		new(big.Int).Mul(big.NewInt(2), oneE18),
		concat(selIqrlMint, leftPad32(mintAmount.Bytes())),
	); err != nil {
		return fmt.Errorf("mint iqrl: %w", err)
	}
	if err := printBalances(ctx, cli, from, "after mint"); err != nil {
		return err
	}

	// 2. Approve QSD pool to spend iQRL (for the deposit's transferFrom).
	step("approve QSD pool to spend iQRL")
	if err := txAndWaitWith(ctx, cli, w, chainID, &core.InverseQRLAddress, big.NewInt(0),
		concat(selErc20Approve, leftPad32(core.QSDAddress.Bytes()), leftPad32(maxUint256().Bytes())),
	); err != nil {
		return fmt.Errorf("approve QSD: %w", err)
	}

	// 3. Deposit 0.5 QRL + 0.5 iQRL into the QSD pool. Symmetric
	//    deposit at the pool's marginal price yields qsdMinted = 1
	//    (full USD value of the deposit at $1.00/QRL).
	depositQRL := new(big.Int).Mul(big.NewInt(500_000_000_000_000_000), big.NewInt(1)) // 0.5 QRL
	depositIQRL := new(big.Int).Mul(big.NewInt(500_000_000_000_000_000), big.NewInt(1)) // 0.5 iQRL
	step("deposit 0.5 QRL + 0.5 iQRL into QSD pool")
	if err := txAndWaitWith(ctx, cli, w, chainID, &core.QSDAddress, depositQRL,
		concat(selQsdDeposit, leftPad32(depositIQRL.Bytes()), leftPad32(big.NewInt(0).Bytes())),
	); err != nil {
		return fmt.Errorf("qsd deposit: %w", err)
	}
	if err := printPoolReserves(ctx, cli); err != nil {
		return err
	}
	if err := printBalances(ctx, cli, from, "after deposit"); err != nil {
		return err
	}

	// 4. Approvals so a paymaster swap works:
	//    - iQRL → QSD pool   (swapIqrlForQrl pulls iQRL via transferFrom)
	//    - iQRL → PayWithIQRL (paymaster pulls maxFee via transferFrom)
	step("approve QSD pool to spend iQRL for swap (already done in step 2)")
	step("approve PayWithIQRL to spend iQRL (paymaster escrow)")
	if err := txAndWaitWith(ctx, cli, w, chainID, &core.InverseQRLAddress, big.NewInt(0),
		concat(selErc20Approve, leftPad32(core.PayWithIQRLAddress.Bytes()), leftPad32(maxUint256().Bytes())),
	); err != nil {
		return fmt.Errorf("approve PayWithIQRL: %w", err)
	}

	// 5. Swap 0.05 iQRL → QRL with fees paid in iQRL via type-0x04
	//    paymaster.
	swapIn := new(big.Int).Mul(big.NewInt(50_000_000_000_000_000), big.NewInt(1)) // 0.05 iQRL
	step("swap 0.05 iQRL → QRL, fees paid in iQRL (type-0x04 paymaster tx)")
	if err := paymasterTxAndWaitWith(ctx, cli, w, chainID, &core.QSDAddress, big.NewInt(0),
		concat(selQsdSwapIqrl, leftPad32(swapIn.Bytes()), leftPad32(big.NewInt(0).Bytes())),
	); err != nil {
		return fmt.Errorf("swap with paymaster: %w", err)
	}
	if err := printPoolReserves(ctx, cli); err != nil {
		return err
	}
	if err := printBalances(ctx, cli, from, "after swap"); err != nil {
		return err
	}

	// 6. Redeem half the QSD position back to QRL + iQRL.
	qsdBal, err := erc20Balance(ctx, cli, core.QSDAddress, from)
	if err != nil {
		return fmt.Errorf("qsd balance: %w", err)
	}
	redeem := new(big.Int).Rsh(qsdBal, 1) // half
	step(fmt.Sprintf("redeem %s QSD back to QRL + iQRL", formatE18(redeem)))
	if err := txAndWaitWith(ctx, cli, w, chainID, &core.QSDAddress, big.NewInt(0),
		concat(selQsdRedeem, leftPad32(redeem.Bytes())),
	); err != nil {
		return fmt.Errorf("qsd redeem: %w", err)
	}
	if err := printPoolReserves(ctx, cli); err != nil {
		return err
	}
	if err := printBalances(ctx, cli, from, "after redeem"); err != nil {
		return err
	}

	step("done")
	return nil
}

// ---------------------------------------------------------------------
// Wallet loading
// ---------------------------------------------------------------------

func loadDevWallet(datadir, passwd string) (wallet.Wallet, error) {
	ksDir := filepath.Join(datadir, "keystore")
	entries, err := os.ReadDir(ksDir)
	if err != nil {
		return nil, fmt.Errorf("read keystore dir %s: %w", ksDir, err)
	}
	var keyfile string
	for _, e := range entries {
		if !e.IsDir() {
			keyfile = filepath.Join(ksDir, e.Name())
			break
		}
	}
	if keyfile == "" {
		return nil, fmt.Errorf("no keystore file found under %s", ksDir)
	}
	raw, err := os.ReadFile(keyfile)
	if err != nil {
		return nil, fmt.Errorf("read keyfile %s: %w", keyfile, err)
	}
	key, err := keystore.DecryptKey(raw, passwd)
	if err != nil {
		return nil, fmt.Errorf("decrypt keystore (try --password=PASSPHRASE): %w", err)
	}
	if key.Wallet == nil {
		return nil, fmt.Errorf("decrypted keystore has no wallet")
	}
	return key.Wallet, nil
}

// ---------------------------------------------------------------------
// Tx helpers
// ---------------------------------------------------------------------

// txAndWaitWith builds, signs, sends, and waits for the receipt of
// a regular DynamicFeeTx (type 0x02) — fees paid in native QRL,
// using the supplied wallet as sender.
func txAndWaitWith(
	ctx context.Context,
	cli *qrlclient.Client,
	w wallet.Wallet,
	chainID *big.Int,
	to *common.Address,
	value *big.Int,
	data []byte,
) error {
	from := addressOf(w)

	nonce, err := cli.PendingNonceAt(ctx, from)
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	tipCap, feeCap, err := suggestFees(ctx, cli)
	if err != nil {
		return err
	}
	gas, err := cli.EstimateGas(ctx, qrl.CallMsg{
		From: from, To: to, Value: value, GasFeeCap: feeCap, GasTipCap: tipCap, Data: data,
	})
	if err != nil {
		return fmt.Errorf("estimate gas: %w", err)
	}
	gas = gas + gas/4 // 25% headroom

	tx, err := types.SignNewTx(w, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       gas,
		To:        to,
		Value:     value,
		Data:      data,
	})
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	if err := cli.SendTransaction(ctx, tx); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return waitReceipt(ctx, cli, tx.Hash())
}

// paymasterTxAndWaitWith is the symmetric helper for type-0x04 txs
// whose fees are paid in iQRL via PayWithIQRL, using the supplied
// wallet as sender.
func paymasterTxAndWaitWith(
	ctx context.Context,
	cli *qrlclient.Client,
	w wallet.Wallet,
	chainID *big.Int,
	to *common.Address,
	value *big.Int,
	data []byte,
) error {
	from := addressOf(w)

	nonce, err := cli.PendingNonceAt(ctx, from)
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	tipCap, feeCap, err := suggestFees(ctx, cli)
	if err != nil {
		return err
	}
	// Estimating gas for a paymaster tx via eth_estimateGas does not
	// model the escrow/settle system calls, so just use a fixed
	// sufficient budget. 600k is plenty for a swap + paymaster hooks.
	const gas uint64 = 600_000

	pm := core.PayWithIQRLAddress
	tx, err := types.SignNewTx(w, types.LatestSignerForChainID(chainID), &types.PaymasterDynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       gas,
		To:        to,
		Value:     value,
		Data:      data,
		Paymaster: &pm,
	})
	if err != nil {
		return fmt.Errorf("sign paymaster: %w", err)
	}
	if err := cli.SendTransaction(ctx, tx); err != nil {
		return fmt.Errorf("send paymaster: %w", err)
	}
	return waitReceipt(ctx, cli, tx.Hash())
}

func suggestFees(ctx context.Context, cli *qrlclient.Client) (tipCap, feeCap *big.Int, err error) {
	tipCap, err = cli.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("tip cap: %w", err)
	}
	head, err := cli.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("head: %w", err)
	}
	if head.BaseFee == nil {
		return nil, nil, fmt.Errorf("chain head has no base fee")
	}
	feeCap = new(big.Int).Mul(head.BaseFee, big.NewInt(2))
	feeCap.Add(feeCap, tipCap)
	return tipCap, feeCap, nil
}

func waitReceipt(ctx context.Context, cli *qrlclient.Client, hash common.Hash) error {
	deadline := time.Now().Add(45 * time.Second)
	for {
		r, err := cli.TransactionReceipt(ctx, hash)
		if err == nil && r != nil {
			if r.Status == types.ReceiptStatusFailed {
				return fmt.Errorf("tx %s reverted (gasUsed=%d)", hash, r.GasUsed)
			}
			fmt.Printf("    mined: tx=%s blk=%d gasUsed=%d\n", hash, r.BlockNumber.Uint64(), r.GasUsed)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tx %s not mined within deadline", hash)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------
// State queries
// ---------------------------------------------------------------------

func oracleStatus(ctx context.Context, cli *qrlclient.Client) error {
	priceRet, err := cli.CallContract(ctx, qrl.CallMsg{To: &core.ValidatorOracleAddress, Data: selOraclePrice}, nil)
	if err != nil {
		return fmt.Errorf("oracle price: %w", err)
	}
	healthyRet, err := cli.CallContract(ctx, qrl.CallMsg{To: &core.ValidatorOracleAddress, Data: selOracleHealthy}, nil)
	if err != nil {
		return fmt.Errorf("oracle healthy: %w", err)
	}
	fmt.Printf("    oracle: price=%s USD, healthy=%v\n",
		formatE18(new(big.Int).SetBytes(priceRet)),
		new(big.Int).SetBytes(healthyRet).Sign() != 0,
	)
	return nil
}

func printBalances(ctx context.Context, cli *qrlclient.Client, who common.Address, label string) error {
	qrlBal, err := cli.BalanceAt(ctx, who, nil)
	if err != nil {
		return fmt.Errorf("qrl balance: %w", err)
	}
	iqrlBal, err := erc20Balance(ctx, cli, core.InverseQRLAddress, who)
	if err != nil {
		return fmt.Errorf("iqrl balance: %w", err)
	}
	qsdBal, err := erc20Balance(ctx, cli, core.QSDAddress, who)
	if err != nil {
		return fmt.Errorf("qsd balance: %w", err)
	}
	fmt.Printf("    balances (%s): QRL=%s iQRL=%s QSD=%s\n",
		label, formatE18(qrlBal), formatE18(iqrlBal), formatE18(qsdBal))
	return nil
}

func printPoolReserves(ctx context.Context, cli *qrlclient.Client) error {
	qrlR, err := cli.CallContract(ctx, qrl.CallMsg{To: &core.QSDAddress, Data: selQsdPoolQRL}, nil)
	if err != nil {
		return fmt.Errorf("poolQRL: %w", err)
	}
	iqrlR, err := cli.CallContract(ctx, qrl.CallMsg{To: &core.QSDAddress, Data: selQsdPoolIQRL}, nil)
	if err != nil {
		return fmt.Errorf("poolIQRL: %w", err)
	}
	fmt.Printf("    QSD pool reserves: QRL=%s iQRL=%s\n",
		formatE18(new(big.Int).SetBytes(qrlR)),
		formatE18(new(big.Int).SetBytes(iqrlR)),
	)
	return nil
}

func erc20Balance(ctx context.Context, cli *qrlclient.Client, token, who common.Address) (*big.Int, error) {
	data := concat(selErc20BalanceOf, leftPad32(who.Bytes()))
	ret, err := cli.CallContract(ctx, qrl.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(ret), nil
}

// ---------------------------------------------------------------------
// Pretty-printing
// ---------------------------------------------------------------------

func step(label string) {
	fmt.Printf("\n=== %s ===\n", label)
}

// formatE18 turns a uint256 in 1e18 fixed-point into a "X.YYY" string.
func formatE18(v *big.Int) string {
	if v == nil {
		return "0"
	}
	whole := new(big.Int).Quo(v, oneE18)
	frac := new(big.Int).Mod(v, oneE18)
	s := fmt.Sprintf("%s.%018s", whole, frac)
	// Trim trailing zeros for readability.
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s += "0"
	}
	return s
}

func leftPad32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func maxUint256() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
}
