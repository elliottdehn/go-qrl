# QSD Stability Layer — End-to-End Demo

A hands-on walkthrough that brings up a single dev node with the QSD
stability layer active, runs the full economic flow against it
(mint iQRL, deposit into the QSD pool, swap with fees paid in iQRL,
redeem QSD), and tears the whole thing down. No external consensus
client, no production keystores. Verified against the current
branch.

For protocol-level architecture see
[`qsd-stability-layer.md`](qsd-stability-layer.md). For a production
operator runbook see [`qsd-deployment.md`](qsd-deployment.md).

## What this demo proves

- The four QSD predeploys (`ValidatorOracle`, `InverseQRL`, `QSD`,
  `PayWithIQRL`) ship live in dev-mode genesis.
- The seeded dev validator's price vote is in storage; the chain
  exposes a healthy QRL/USD price right out of genesis.
- `qsdfeeder` running in keyed mode keeps the on-chain price
  recently-voted on every block (and free for the validator).
- The full QSD economic loop works: a user can mint iQRL, deposit
  paired QRL+iQRL into the QSD pool, swap on the pool with the
  transaction's fees paid in iQRL via a type-0x04 paymaster, and
  redeem QSD back to a pro-rata slice of the pool.

## 0. Prereqs

- Go 1.24+ on `$PATH`.
- `curl` (for one optional sanity check).
- ~50 MB of free disk for the ephemeral chaindata.

## 1. Build

From the repo root:

```sh
go build -o /tmp/gqrl       ./cmd/gqrl
go build -o /tmp/qsdfeeder  ./cmd/qsdfeeder
go build -o /tmp/qsddemo    ./cmd/qsddemo
```

All three should exit silently with `$? == 0`.

## 2. Start the dev node

`--dev.period=1` makes the node mine one block per second.

```sh
mkdir -p /tmp/qsd-demo
/tmp/gqrl \
    --dev \
    --dev.period=1 \
    --http \
    --http.api=qrl,debug,web3,net \
    --datadir=/tmp/qsd-demo/data \
    > /tmp/qsd-demo/gqrl.log 2>&1 &
echo $! > /tmp/qsd-demo/gqrl.pid

sleep 5
tail -3 /tmp/qsd-demo/gqrl.log
```

You should see lines like `Chain head was updated number=N ...`
ticking forward.

## 3. (Optional) Verify the oracle is live

The QRL fork uses the `qrl_` JSON-RPC namespace.

```sh
ORACLE=Q0000000000000000000000000000000000010000

# price()        -> keccak256("price()")[:4]         = 0xa035b1fe
# healthy()      -> keccak256("healthy()")[:4]       = 0x7560fdd1

curl -s -X POST http://127.0.0.1:8545 \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"qrl_call","params":[{"to":"'"$ORACLE"'","data":"0xa035b1fe"},"latest"],"id":1}'
echo
```

Expected: `0x...0de0b6b3a7640000` (= 1e18 = $1.00, the seeded vote).

## 4. Start the price-feeder daemon (keyed mode)

The proposer-vote rule requires every block produced by an active
validator to contain that validator's own `submitVote` tx. In a real
network this is the `qsdfeeder` daemon's job. In dev mode the dev
faucet is the sole validator, so we point the daemon at its
keystore.

```sh
KEYFILE=$(ls /tmp/qsd-demo/data/keystore/ | head -1)
touch /tmp/qsd-demo/empty.pass    # --dev creates the keystore with an empty password

/tmp/qsdfeeder \
    --rpc=http://127.0.0.1:8545 \
    --interval=1s \
    --price-source=static --static-price=1.00 \
    --keystore=/tmp/qsd-demo/data/keystore/$KEYFILE \
    --password-file=/tmp/qsd-demo/empty.pass \
    > /tmp/qsd-demo/feeder.log 2>&1 &
echo $! > /tmp/qsd-demo/feeder.pid

sleep 5
tail -3 /tmp/qsd-demo/feeder.log
```

Expected: lines like `submitted vote: tx=0x... forBlock=N ...` once
per second. The submitVote txs are free (entire fee refunded by
consensus) but cover the proposer-vote requirement.

## 5. Run the full QSD economic flow

`qsddemo` walks through the user-facing flow against the live node.
It generates a fresh ephemeral wallet (so it doesn't race with
`qsdfeeder` for the faucet's nonces), funds it from the dev faucet,
and runs each operation in sequence.

```sh
/tmp/qsddemo \
    --rpc=http://127.0.0.1:8545 \
    --datadir=/tmp/qsd-demo/data \
    --password=""
```

Expected output (timings and txhashes will differ):

```
connected to http://127.0.0.1:8545, chainID=1337
dev faucet: Q...
demo wallet: Q...

=== oracle status ===
    oracle: price=1.0 USD, healthy=true

=== fund demo wallet from faucet (5 QRL) ===
    mined: tx=0x... gasUsed=21000

=== mint 0.9 iQRL (pays QRL) ===
    mined: tx=0x... gasUsed=131601
    balances (after mint): QRL=4.09... iQRL=0.9 QSD=0.0

=== approve QSD pool to spend iQRL ===
    mined: tx=0x... gasUsed=46486

=== deposit 0.5 QRL + 0.5 iQRL into QSD pool ===
    mined: tx=0x... gasUsed=175033
    QSD pool reserves: QRL=0.5 iQRL=0.5
    balances (after deposit): QRL=3.59... iQRL=0.4 QSD=1.0

=== approve PayWithIQRL to spend iQRL (paymaster escrow) ===
    mined: tx=0x... gasUsed=46486

=== swap 0.05 iQRL → QRL, fees paid in iQRL (type-0x04 paymaster tx) ===
    mined: tx=0x... gasUsed=59092
    QSD pool reserves: QRL=0.454... iQRL=0.55
    balances (after swap): QRL=3.63... iQRL=0.34... QSD=1.0

=== redeem 0.5 QSD back to QRL + iQRL ===
    mined: tx=0x... gasUsed=82869
    QSD pool reserves: QRL=0.228... iQRL=0.276...
    balances (after redeem): QRL=3.85... iQRL=0.62... QSD=0.5

=== done ===
```

What just happened, step by step:

1. **Mint iQRL.** Sent 0.9045 QRL to `InverseQRL.mint(0.9 iQRL)`.
   The contract destroys the QRL (base cost 0.9 + 0.5% fee = 0.9045)
   and mints 0.9 iQRL to the caller. Per-block mint cap is
   max(totalSupply\*25/1e6, 1 ether), which is 1 iQRL on a fresh
   chain.
2. **Approve.** Standard ERC-20 approve so the QSD pool can pull
   iQRL during deposit.
3. **Deposit.** Sent 0.5 QRL + (transferFrom) 0.5 iQRL to
   `QSD.deposit`. CPMM mints 1 QSD (= 2 \* sqrt(0.5\*0.5)).
4. **Approve PayWithIQRL.** So the paymaster can pull the
   iQRL-denominated maxFee during escrow.
5. **Paymaster swap.** Type-0x04 transaction calling
   `QSD.swapIqrlForQrl(0.05 iQRL)`. The fee is paid in iQRL: the
   chain converts the EIP-1559 base fee from QRL to iQRL via the
   oracle, burns that portion of the iQRL, and tips the rest to the
   coinbase in iQRL.
6. **Redeem.** Burned 0.5 QSD on `QSD.redeem`. Got back a pro-rata
   slice of the pool (~0.226 QRL + ~0.274 iQRL, sized by the
   USD-value formula).

## 6. Tear down

```sh
kill $(cat /tmp/qsd-demo/feeder.pid)
kill $(cat /tmp/qsd-demo/gqrl.pid)
rm -rf /tmp/qsd-demo
```

## Troubleshooting

- **Oracle reports `healthy=false`.** `qsdfeeder` isn't successfully
  landing votes. Check `/tmp/qsd-demo/feeder.log` for
  `execution reverted` errors; common cause is the keyed submitter
  targeting a forBlock the chain has already passed (transient,
  resolves on the next tick).
- **`qsddemo` hangs at "tx not mined within deadline".** Either
  `qsdfeeder` stopped running (no proposer vote in the new block,
  miner rejects the body) or there's a paymaster-tx revert. Run
  `gqrl` with `--verbosity=4` and grep
  `/tmp/qsd-demo/gqrl.log` for `account skipped` to see the actual
  revert reason (the system call now reports the revert payload).
- **`mint iqrl: estimate gas: execution reverted`.** The per-block
  mint cap on a fresh chain is 1 iQRL. The demo asks for 0.9; if you
  edit the script to ask for more, expect this. Mint over multiple
  blocks instead.
