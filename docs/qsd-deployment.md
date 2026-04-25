# QSD Stability Layer — Deployment Runbook

This document covers what a node operator needs to do to bring up a
QRL chain with the QSD stability layer enabled, register validators,
run the price-feeder daemon, and operate the system day-to-day.

For the protocol-level architecture see
[`qsd-stability-layer.md`](qsd-stability-layer.md). For daemon-level
operation see [`cmd/qsdfeeder/README.md`](../cmd/qsdfeeder/README.md).

## 0. Prerequisites

  - go-qrl built from a commit that includes the stability layer
    (branch `qsd-stability-layer` or descendants). All four
    predeploys must be present in genesis.
  - A funded oracle-owner account. In dev-genesis this is the
    faucet; in production it should be a multisig or governance
    contract that can be rotated without redeploying the chain.
  - One or more validator accounts, each holding enough native QRL
    to cover the buyGas pre-charge for one vote tx. The free-vote
    rule refunds the entire fee, so the steady-state cost is zero —
    a few QRL per validator is plenty as a working buffer.

## 1. Genesis

Production chains construct their genesis from
`AddQSDStabilityLayer(alloc, params)`:

```go
import "github.com/theQRL/go-qrl/core"

alloc := core.GenesisAlloc{
    // ... your network's account allocations ...
}
core.AddQSDStabilityLayer(alloc, core.DefaultQSDPredeployParams(oracleOwner))

genesis := &core.Genesis{
    Config: chainConfig,
    Alloc:  alloc,
    // ... other genesis fields ...
}
```

`oracleOwner` is the address that will be authorised to add and
remove validators on `ValidatorOracle`. See section 5 for the
rotation procedure.

The voting parameters (`OracleVoteStalenessBlocks`,
`OracleMinQuorumNumerator`, `OracleMinQuorumDenominator`) are
**baked into the predeploy bytecode**. Default values come from
`qsd-contracts/script/Predeploy.s.sol` (currently 10, 2, 3). If you
need different values, regenerate the genesis dump:

```sh
cd qsd-contracts
# Edit script/Predeploy.s.sol VOTE_STALENESS_BLOCKS / MIN_QUORUM_*
OWNER=0x0000000000000000000000000000000000000001 \
    forge script script/Predeploy.s.sol --tc PredeployScript -vv
cp out/qsd-genesis-state.json ../go-qrl/core/qsd_predeploy_state.json
```

Then update the matching constants in `core/qsd_predeploy.go`
(`OracleVoteStalenessBlocks`, `OracleMinQuorumNumerator`,
`OracleMinQuorumDenominator`) and rebuild go-qrl.

## 2. Validator key management

Each validator runs a wallet with an ML-DSA-87 keypair. Generate
with `qrlkey`:

```sh
qrlkey generate --passwordfile=/etc/qrl/validator.pass \
    /etc/qrl/validator.json
```

Permissions:

```sh
chown qrl:qrl /etc/qrl/validator.{json,pass}
chmod 0400  /etc/qrl/validator.{json,pass}
```

The keystore JSON and the password file should be stored on
disk-encrypted volumes. The qsdfeeder reads both at startup and
keeps the decrypted wallet in memory for the daemon's lifetime —
restarts re-prompt for the password.

In production, hold the keystore in a secrets manager (Vault,
AWS Secrets Manager, etc.) and have the operator inject both files
at process-start time.

## 3. Validator registration

The oracle owner adds each validator to the active set via the
`addValidator(address)` call on `ValidatorOracle`:

```sh
cast send 0x0000000000000000000000000000000000010000 \
    "addValidator(address)" 0x<validator-address> \
    --private-key $OWNER_KEY --rpc-url http://localhost:8545
```

Removal is symmetric (`removeValidator(address)`); the oracle
swap-and-pops without a length-bound failure mode for sets up to
`MAX_VALIDATORS = 100`.

A validator becomes "fresh" — and starts contributing to the
oracle's median + healthy() quorum — as soon as it has submitted
its first vote within the last `voteStalenessBlocks` blocks.

## 4. Running the price feeder

Each validator runs one `qsdfeeder` process pointing at its local
go-qrl node:

```sh
qsdfeeder \
    --rpc=http://127.0.0.1:8545 \
    --keystore=/etc/qrl/validator.json \
    --password-file=/etc/qrl/validator.pass \
    --price-source=static \
    --static-price=1.00 \
    --interval=12s \
    --target-offset=1
```

Recommendations:

  - **Cadence**: one tick per block. With 12-second slots the
    daemon's `--interval=12s` keeps the validator's vote
    no-more-than-one-block stale. A larger interval works (the
    free-vote rule still applies on every fresh vote) but reduces
    the validator's contribution to the median.
  - **Target offset**: `1` is right for normal network conditions
    (vote claims block N+1, mines in N+1, free path engages).
    Bump to `2` if the pool sees frequent miss-by-one-slot due to
    propagation latency; the trade-off is that a vote claiming
    block N+2 won't mine until block N+2 actually arrives.
  - **Real price source**: the `static` source is a placeholder.
    Production deployments need a real off-chain price oracle
    (Chainlink, Pyth, exchange API aggregator). The daemon's
    `PriceSource` interface (one method, `FetchUSDPerQRL`) is the
    integration point.

When `--keystore` is empty the daemon falls back to a stub
submitter that only logs what it would have sent — useful for
smoke-testing infrastructure plumbing without provisioning a key.

## 5. Owner rotation

Transferring oracle ownership uses OpenZeppelin Ownable's
`transferOwnership(address)`:

```sh
cast send 0x0000000000000000000000000000000000010000 \
    "transferOwnership(address)" 0x<new-owner> \
    --private-key $CURRENT_OWNER_KEY --rpc-url http://localhost:8545
```

Production deployments should rotate the genesis-time owner (often
a single-signer faucet) to a multisig or a governance contract
within the first epoch. Until that happens, a single key compromise
is sufficient to rewrite the validator set.

## 6. Monitoring

Metrics to track per validator:

  - **Vote inclusion rate** — count blocks where this validator's
    submitVote was included divided by total blocks. A drop below
    ~95% indicates either daemon misconfiguration (too-fast tick,
    target offset too aggressive) or network problems.
  - **Last-vote block** — `votes[v].blockNumber` from the on-chain
    state. Should stay within `voteStalenessBlocks` of the head.
  - **Native balance** — should remain roughly constant under the
    free-vote rule. A monotonically-decreasing balance indicates
    that the daemon's votes are reverting (wrong forBlockNumber,
    not in validator set, etc.) and paying normal gas.

Metrics to track per oracle:

  - **healthy()** returning false — quorum has dropped below the
    minQuorumNumerator/Denominator threshold. Page someone.
  - **price()** changing more than X% block-to-block — can indicate
    a single validator submitting wildly off-base prices, or a
    real market move. Cross-check against off-chain feeds.
  - **iQRL balance of any address** — informational. Validators
    receiving paymaster fees in iQRL accumulate it; monitoring
    this gives a feel for paymaster usage.

## 7. The first user transaction

A new account on the chain can use the paymaster only after it has
acquired iQRL **and** approved the paymaster contract. The
practical first-tx flow for any user:

  1. Receive native QRL (faucet / OTC / whatever bootstrapping
     method the network uses).
  2. Mint iQRL by calling `InverseQRL.mint{value: ...}(amount)` —
     paid in native QRL. This is the chicken-and-egg tx that can't
     itself be a paymaster tx.
  3. Approve the paymaster:
     `iqrl.approve(0x...010003, type(uint256).max)`. Paid in
     native QRL (one-time).
  4. From here on, all transactions can specify
     `Paymaster = 0x...010003` and the gas cost flows through iQRL.

A clean wallet UX bundles steps 2 and 3 into the user's onboarding
flow so it feels like one interaction.

## 8. Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `qsdfeeder` logs "load keyed submitter: --password-file is required" | `--keystore` set without `--password-file` | Provide both, or omit both for stub mode |
| Vote tx reverts with `WrongBlockNumber(forBlock, current)` | `--target-offset` too large; tx didn't mine in claimed block | Reduce offset to 1; check network propagation |
| Vote tx reverts with `NotValidator` | Validator not yet registered, or removed | Have the oracle owner call `addValidator` |
| Oracle `healthy()` returns false | Below quorum (default 2/3 of the active set has fresh votes) | Bring more validator daemons online |
| Paymaster tx admitted but reverts at execution | Allowance dropped between admission and execution | Re-approve, or set unlimited approval |
| Paymaster contract balance non-zero between blocks | Settle path failed mid-execution; investigate | File a bug — should never happen |

## 9. Open work

These known gaps are tracked in
[`qsd-stability-layer.md`](qsd-stability-layer.md) §"Open work":

  - Richer paymaster auction (basic USD-comparable ordering shipped;
    fuller priority-fee semantics are open work).
  - Wallet/SDK support for constructing type-0x04 txs (the chain
    accepts them; client tooling is incomplete).
