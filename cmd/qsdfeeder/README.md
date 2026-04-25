qsdfeeder
=========

`qsdfeeder` is a validator-side daemon that periodically posts a
QRL/USD price vote to the on-chain `ValidatorOracle` contract.

For protocol context see [`docs/qsd-stability-layer.md`](../../docs/qsd-stability-layer.md).


# Status

Two submission modes:

  - **Keyed** — when `--keystore` is set, the daemon decrypts the wallet,
    dials the RPC endpoint, builds an EIP-1559 transaction calling
    `submitVote(uint256)` on the oracle, signs with the wallet's
    post-quantum key, and broadcasts via `qrl_sendRawTransaction`.
  - **Stub** — when `--keystore` is empty, the daemon only logs what it
    would have submitted. Useful for smoke-testing price discovery on
    a dev network without provisioning a validator key.


# Build

```
go build ./cmd/qsdfeeder
```


# Usage

Stub mode (no transactions, logs only):

```
qsdfeeder \
    --rpc=http://127.0.0.1:8545 \
    --interval=30s \
    --price-source=static \
    --static-price=1.00
```

Keyed mode (real signed transactions):

```
qsdfeeder \
    --rpc=http://127.0.0.1:8545 \
    --interval=30s \
    --price-source=static \
    --static-price=1.00 \
    --keystore=/etc/qrl/validator.json \
    --password-file=/etc/qrl/validator.pass
```

The default `--oracle` is `core.ValidatorOracleAddress`, so on a
dev-genesis node it can be omitted. Chain ID is auto-detected from
the RPC endpoint when `--chain-id=0` (the default).


## Flags

| Flag             | Default                  | Notes |
|------------------|--------------------------|-------|
| `--rpc`          | `http://127.0.0.1:8545`  | go-qrl JSON-RPC endpoint. |
| `--oracle`       | `ValidatorOracleAddress` | Accepts `0x` or `Q` prefix. |
| `--interval`     | `30s`                    | Vote cadence (wall-clock). Each tick targets `head + --target-offset`. |
| `--target-offset`| `1`                      | Block offset added to the current head when targeting a vote. `1` = "the next block". |
| `--price-source` | `static`                 | Only `static` is implemented. |
| `--static-price` | `1.00`                   | USD per QRL when source is `static`. |
| `--chain-id`     | `0`                      | `0` means auto-detect from RPC. |
| `--keystore`     | (empty)                  | Path to validator keystore JSON. Empty selects stub submitter. |
| `--password-file`| (empty)                  | Required when `--keystore` is set. Trailing newlines are stripped. |
| `-v`             | `false`                  | Verbose logging. |


## Run loop

The daemon ticks immediately on start (so the first vote does not
wait a full interval), then on the configured ticker. Each cycle:

1. Fetch the current chain head from the `BlockNumberSource`
   (`qrlclient.BlockNumber` in keyed mode, a synthetic counter in
   stub mode).
2. Fetch the current price from the configured `PriceSource`.
3. Encode `submitVote(uint256,uint256)` calldata with
   `forBlockNumber = head + --target-offset` and the price scaled
   to `1e18`.
4. Hand it to the configured `VoteSubmitter`.

In keyed mode each cycle additionally fetches the pending nonce, the
latest base fee, and a tip-cap suggestion from the RPC, and adds 20%
gas headroom on top of the `EstimateGas` result.

Transient errors are logged and the loop continues. `SIGINT` /
`SIGTERM` drains the loop and exits cleanly.


## Wire format

Vote calldata is the standard 4+32+32 layout:

```
0x6f93bfb7                                                          // submitVote(uint256,uint256) selector
0000000000000000000000000000000000000000000000000000000000000003   // forBlockNumber (here: 3)
00000000000000000000000000000000000000000000000014d1120d7b160000   // priceUsd1e18 (here: 1.5e18)
```

The contract enforces `forBlockNumber == block.number` strictly: a
tx that mines in any other block reverts. This eliminates the
stale-vote-overwrites-fresh-vote race that affects late-mining
mempool txs in the no-target design.

The selector is computed from
`keccak256("submitVote(uint256,uint256)")[:4]` at init and verified
against the Solidity ABI in tests.


# Tests

```
go test ./cmd/qsdfeeder/...
```

Covers price-string parsing, static-source immutability, and
calldata encoding (selector + uint256 layout, plus rejection of
zero / negative / nil inputs).
