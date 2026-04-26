# QSD Stability Layer

The QSD stability layer is a set of four on-chain primitives plus a
small set of consensus-layer rules that together implement a QRL-
native, dollar-denominated stablecoin and an iQRL-denominated fee
path.

The whole layer is gated behind a single fork flag (`QSDTime`). On
chains where the flag is unset or hasn't activated yet, the rules
below are no-ops and the predeploys do not exist. Networks that
schedule the flag mid-chain install the predeploys at the activation
block; networks that bake them in at genesis hit the same code path
on a fast no-op.

This document describes the layer at the level a node operator or
deploy-script author needs. The protocol-level rationale lives in the
QRL grant proposal. For a hands-on walkthrough that runs the full
flow against a dev node, see [`qsd-demo.md`](qsd-demo.md).

## Components

| # | Contract            | Reserved address                               | Role |
|---|---------------------|------------------------------------------------|------|
| 1 | `ValidatorOracle`   | `0x0000000000000000000000000000000000010000`   | Per-block median QRL/USD price from validator votes. |
| 2 | `InverseQRL` (iQRL) | `0x0000000000000000000000000000000000010001`   | ERC-20 whose canonical USD value tracks `1 / p`. Minted by burning native QRL. |
| 3 | `QSD`               | `0x0000000000000000000000000000000000010002`   | Dollar stablecoin. ERC-20. Backed by a symmetric (QRL, iQRL) pool. |
| 4 | `PayWithIQRL`       | `0x0000000000000000000000000000000000010003`   | Paymaster. Lets a tx settle gas in iQRL: escrow before exec, settle (forward to coinbase + refund unused) after. |

Addresses are sequential at the start of the application-reserved
band (`0x010000+`), clear of all precompile slots. They are exposed
as constants in `core/qsd_predeploy.go`:

```go
core.ValidatorOracleAddress
core.InverseQRLAddress
core.QSDAddress
core.PayWithIQRLAddress
```

Downstream tooling (deploy scripts, the `qsdfeeder` daemon, RPC
clients, wallets) should target these constants by name rather than
hard-coding the hex.

## Solvency invariant (QSD)

QSD is the LP token of a CPMM between native QRL and iQRL. Because
iQRL is inverse-priced (`USD(iQRL) = 1/p`), the pair is invariant
under price moves: there is no impermanent loss, and no swap fee is
charged.

Per-deposit solvency follows from AM-GM applied to the depositor's
contribution; aggregate solvency follows from Cauchy-Schwarz over
per-deposit contributions. Concretely:

```
2 · sqrt(k) ≥ totalSupply()
```

where `k` is the CPMM product of pool reserves. The contract asserts
this invariant on every state mutation.

## Consensus-layer rules

The stability layer adds four rules to the state-transition engine.
Each one is gated on the QSD fork flag (`config.IsQSD(header.Time)`)
so pre-fork blocks behave exactly like a vanilla post-Zond chain.
Within the gate, the per-rule checks themselves are cheap (one or
two storage SLOADs at most).

### 1. Free validator votes

A transaction qualifies for **zero-fee** treatment when **all** the
following hold at execution time:

  - it is a regular (non-paymaster) tx,
  - `tx.To == ValidatorOracleAddress`,
  - calldata begins with the `submitVote(uint256,uint256)` selector
    (`0x6f93bfb7`),
  - sender is registered in `validatorIndex` (non-zero entry), AND
    sender's last vote (`votes[from].blockNumber`) is for an earlier
    block than the current one,
  - the EVM call returned without revert.

When all five hold the entire pre-charged `gasLimit × gasPrice`
debit is refunded and no coinbase tip is paid — the validator's
net cost is zero. A reverted submitVote (e.g. wrong
`forBlockNumber`) or a second vote in the same block from the same
validator pays normal gas as anti-spam.

Free votes additionally **do not consume block gas**. The execution
returns its full `initialGas` allocation back to the block's
`GasPool` and reports `result.UsedGas == 0`, so a block packed with
N validator votes has the same effective gas budget for user txs as
a block with zero votes. Without this, a network with N active
validators would burn `~N × voteGas` of every block's gas-limit
budget on infrastructure before any user tx got a chance to land.

### 2. Vote ordering

Within a single block, every `submitVote` tx must come before every
non-vote tx. `core.validateVoteOrdering` enforces it in
`ValidateBody`; the miner's `partitionVoteTxs` arranges the same
order during block construction.

This guarantees the on-chain oracle's per-block median is finalized
by the time any non-vote tx in the same block runs. Paymaster fee
splits, `QSD.redeem`, `InverseQRL.mint`, and any other code that
reads `oracle.price()` / `oracle.healthy()` see the post-vote
median, never a stale one mid-block.

### 3. Proposer-vote rule

If a block's coinbase is registered as a validator (per parent
state), the block must contain at least one tx satisfying:

  - `sender == coinbase`,
  - `IsSubmitVoteTx(tx) == true`, and
  - the decoded `forBlockNumber` argument equals the block's number.

The proposer is free to also include other validators' votes — those
are regular user-signed txs that pass the rest of the body checks
on their own. The rule only fires when a registered validator is
proposing AND no submitVote of their own is in the body.

Why: a non-voting active validator could otherwise propose blocks
indefinitely without contributing to the oracle median. The rule
guarantees one fresh price every block from the producer themselves,
even if other validators are silent or offline. The miner mirrors
the rule (`miner.ensureProposerVote`) and refuses to assemble a
block that would fail it, so an operator running `qsdfeeder` never
accidentally produces a body their peers will reject.

### 4. iQRL paymaster

A transaction with type `0x04` (`PaymasterDynamicFeeTx`) carries a
`Paymaster` field naming a contract that handles fees. When set:

  - native-balance debit is skipped in `buyGas`;
  - consensus system-calls `paymaster.escrow(sender, maxFee)` from
    sentinel address `0xff..fe` before EVM execution; the contract
    pulls `maxFee` of its accepted asset (iQRL) from the sender;
  - after execution, consensus system-calls
    `paymaster.settle(sender, maxFee, burnAmount, tipAmount)`. The
    `(burn, tip)` split mirrors EIP-1559 base-fee economics:
      - `burnAmount = gasUsed × baseFeeIQRL` is destroyed via
        `iqrl.burn`, where `baseFeeIQRL = baseFeeQRL × p²` (oracle-
        derived; falls to zero when the oracle is unhealthy, leaving
        the entire fee as tip);
      - `tipAmount = gasUsed × (min(gasFeeCap, baseFeeIQRL +
        gasTipCap) - baseFeeIQRL)` is forwarded to `block.coinbase`;
      - the unused `maxFee - burn - tip` is refunded to the sender.
  - the txpool admits these txs only if `Paymaster` is on the chain
    allowlist (currently a one-element list: `PayWithIQRLAddress`).

The user keeps a buffer of native QRL only for `msg.Value` transfers;
gas itself can be paid in iQRL indefinitely. Because base-fee burns
flow into the iQRL side too, iQRL supply responds symmetrically to
chain activity — paymaster txs are no more or less inflationary
than native EIP-1559 txs.

## Fork activation

`params.ChainConfig.QSDTime *uint64` schedules the layer's
activation at the first block whose `header.Time >= *QSDTime`. Set
to `nil` (the default for mainnet, testnet, betanet) the entire
layer is dormant: no predeploys exist, no rules fire, type-0x04
paymaster txs are rejected at admission and at block validation,
and `IsQSD(t)` returns false unconditionally.

Two activation paths are supported, both byte-for-byte equivalent
post-activation:

  - **Genesis activation** (`QSDTime: newUint64(0)`): the genesis
    `GenesisAlloc` already carries the four predeploys, so the
    activation install is a no-op fast path. Used by
    `AllDevChainProtocolChanges` and dev-mode genesis.
  - **Mid-chain activation** (`QSDTime: newUint64(<future ts>)`):
    the chain runs vanilla until the timestamp crosses, at which
    point `state_processor.Process` calls
    `InstallQSDPredeploysIfMissing` and seeds the four contracts'
    bytecode and storage from the same embedded JSON dump that
    genesis would have used. Subsequent blocks short-circuit on a
    single `GetCodeSize` probe.

The mid-chain path is the one real networks will use: it lets a
chain ship the layer without rolling a new genesis.

## Genesis pre-deploy

`AddQSDStabilityLayer(alloc, params)` registers the four addresses
in a `GenesisAlloc`, used by chains that activate QSD at genesis.
`QSDPredeployParams` currently has no fields: the validator set is
consensus-driven (see "Validator set source" below) and the voting
parameters are baked into bytecode.

Networks that activate later use `InstallQSDPredeploysIfMissing`,
which reads the same embedded dump but writes directly into the
state DB at the activation block. Either way, the post-activation
contract code and initial storage are identical.

Voting parameters (`voteStalenessBlocks`, `minQuorumNumerator`,
`minQuorumDenominator`) are immutable in the ValidatorOracle
bytecode and therefore frozen into the embedded state dump;
changing them requires regenerating that dump.

### Validator set source

The oracle's validator set is **the chain's PoS validator set**.
On every block, the consensus engine system-calls
`ValidatorOracle.setValidatorSet(addresses[])` from the sentinel
sender `0xff..fe`, mirroring the active beacon validators (by
withdrawal address) into EVM storage. The contract has no admin
or owner key — there's nothing to govern manually.

The diff is computed inside the contract: addresses present in
the new set but missing from `validators[]` are appended; current
validators not in the new set are removed via swap-and-pop with
their `votes[]` entry deleted; addresses common to both keep their
existing vote.

### State source

Bytecode and post-construction storage are baked into the embedded
`core/qsd_predeploy_state.json`, generated by
`contracts/script/Predeploy.s.sol`. The script:

  1. deploys all four contracts normally;
  2. relocates each to its reserved address via `vm.etch` + a
     per-slot `vm.store` copy, so dependent `immutable`s resolve to
     the on-chain instances;
  3. dumps the resulting state via `vm.dumpState`.

To regenerate (after a contract change):

```
cd contracts
forge script script/Predeploy.s.sol --tc PredeployScript -vv
cp out/qsd-genesis-state.json ../go-qrl/core/qsd_predeploy_state.json
```

## Validator daemon

Validators run `qsdfeeder` to post price votes to `ValidatorOracle`.
With consensus rule (1) those votes cost validators zero gas in
steady state. See [`cmd/qsdfeeder/README.md`](../cmd/qsdfeeder/README.md)
for build and operation.

The proposer-vote rule (rule 3 above) makes `qsdfeeder` mandatory
for an active validator: a block proposed without the proposer's
own submitVote in the body fails `ValidateBody` and gets rejected
by peers. The miner enforces the same rule pre-broadcast, so a
validator running without `qsdfeeder` doesn't propagate invalid
blocks; they just fail to produce blocks at all.

The vote calldata is `submitVote(uint256 forBlockNumber, uint256
priceUsd1e18)` — selector `0x6f93bfb7`, followed by two 32-byte
big-endian uint256s. Price is scaled by `1e18` (so `$1.00 / QRL` is
`1_000_000_000_000_000_000`); `forBlockNumber` must equal
`block.number` at execution time, or the contract reverts. Strict
block targeting prevents a stuck-mempool tx from overwriting a
fresher vote with a stale price when it eventually mines, and the
txpool evicts stale-target votes on every chain-head advance so
they don't waste block space.

## Demo

A self-contained walkthrough runs the entire layer end-to-end
against `gqrl --dev`: mint iQRL, deposit paired QRL+iQRL into the
QSD pool, swap on the pool with the transaction's fees paid in
iQRL via a type-0x04 paymaster, and redeem QSD back to its pro-rata
slice. See [`qsd-demo.md`](qsd-demo.md).

## Open work

  - **Wallet / SDK support** for constructing type-0x04 txs. The
    chain accepts them via `eth_sendRawTransaction` and the JSON
    wire format is documented; client-side tooling that builds and
    signs them is still missing.
  - **Fee-bearing paymasters.** PayWithIQRL is a pure escrow with
    no service fee — it forwards everything except the EIP-1559-
    equivalent burn to coinbase. Future paymasters that do real
    work (e.g. swap QSD → QRL via the QSD pool) can keep a margin
    by adjusting the (burn, tip) split they pass to settle; the
    framework already supports it. Open work is deploying such a
    paymaster, not extending the framework.

For day-to-day operations see
[`qsd-deployment.md`](qsd-deployment.md).
