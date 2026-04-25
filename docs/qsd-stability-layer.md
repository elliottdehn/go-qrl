# QSD Stability Layer

The QSD stability layer is a set of four on-chain primitives plus
two consensus-layer rules that together implement a QRL-native,
dollar-denominated stablecoin and an iQRL-denominated fee path.

This document describes the layer at the level a node operator or
deploy-script author needs. The protocol-level rationale lives in the
QRL grant proposal.

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

The stability layer adds two narrow rules to the state-transition
engine. Both are gated on contract-storage SLOADs so they're cheap
and don't require a hard-fork flag beyond the genesis activation.

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

### 2. iQRL paymaster

A transaction with type `0x04` (`PaymasterDynamicFeeTx`) carries a
`Paymaster` field naming a contract that handles fees. When set:

  - native-balance debit is skipped in `buyGas`;
  - consensus system-calls `paymaster.escrow(sender, maxFee)` from
    sentinel address `0xff..fe` before EVM execution; the contract
    pulls `maxFee` of its accepted asset (iQRL) from the sender;
  - after execution, consensus system-calls
    `paymaster.settle(sender, maxFee, actualFee)`; the contract
    forwards `actualFee` to `block.coinbase` and refunds the rest;
  - the txpool admits these txs only if `Paymaster` is on the chain
    allowlist (currently a one-element list: `PayWithIQRLAddress`).

The user keeps a buffer of native QRL only for `msg.Value` transfers;
gas itself can be paid in iQRL indefinitely.

## Genesis pre-deploy

`AddQSDStabilityLayer(alloc, params)` registers the four addresses
in a `GenesisAlloc`. `QSDPredeployParams` currently has no fields:
the validator set is consensus-driven (see "Validator set source"
below) and the voting parameters are baked into bytecode.

Voting parameters (`voteStalenessBlocks`, `minQuorumNumerator`,
`minQuorumDenominator`) are immutable in the ValidatorOracle
bytecode and therefore frozen into the genesis-state dump; changing
them requires regenerating that dump.

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
`qsd-contracts/script/Predeploy.s.sol`. The script:

  1. deploys all four contracts normally;
  2. relocates each to its reserved address via `vm.etch` + a
     per-slot `vm.store` copy, so dependent `immutable`s resolve to
     the on-chain instances;
  3. dumps the resulting state via `vm.dumpState`.

To regenerate (after a contract change):

```
cd qsd-contracts
forge script script/Predeploy.s.sol --tc PredeployScript -vv
cp out/qsd-genesis-state.json ../go-qrl/core/qsd_predeploy_state.json
```

## Validator daemon

Validators run `qsdfeeder` to post price votes to `ValidatorOracle`.
With consensus rule (1) those votes cost validators zero gas in
steady state. See [`cmd/qsdfeeder/README.md`](../cmd/qsdfeeder/README.md)
for build and operation.

The vote calldata is `submitVote(uint256 forBlockNumber, uint256
priceUsd1e18)` — selector `0x6f93bfb7`, followed by two 32-byte
big-endian uint256s. Price is scaled by `1e18` (so `$1.00 / QRL` is
`1_000_000_000_000_000_000`); `forBlockNumber` must equal
`block.number` at execution time, or the contract reverts. Strict
block targeting prevents a stuck-mempool tx from overwriting a
fresher vote with a stale price when it eventually mines, and the
txpool evicts stale-target votes on every chain-head advance so
they don't waste block space.

## Status

Branch `qsd-stability-layer` lands the layer in nine additive commits:

1. Genesis pre-deploy — reserves addresses. ✅
2. Validator daemon scaffold — `cmd/qsdfeeder` stub. ✅
3. Real submitter — keystore-backed, EIP-1559, signed via `qrlclient`. ✅
4. Bytecode bake — populated `GenesisAccount{Code, Storage}` from Foundry. ✅
5. Strict block-targeted votes — `submitVote(forBlockNumber, ...)`. ✅
6. iQRL paymaster — type `0x04` tx + `PayWithIQRL` predeploy + state-transition hook + integration tests. ✅
7. Free validator votes at consensus + integration tests. ✅
8. Stale-vote eviction in the txpool. ✅

Each step is independently testable; nothing in earlier steps
depends on later ones.

## Open work

  - **`ValidatorsHash` header commitment.** The block body carries
    `Validators []common.Address`, the engine API plumbs it from
    PayloadAttributes through to `state_processor.Process`, and
    the system call fires at the start of every block. What's
    missing is a header-level Merkle commitment to the list — like
    `WithdrawalsHash` for withdrawals — so a block proposer can't
    serve different `Validators` to different peers. Until that
    lands, the validator set in a block is not consensus-bound.
  - **Wallet / SDK support** for constructing type-0x04 txs. The
    chain accepts them via `eth_sendRawTransaction` and the JSON
    wire format is documented; client-side tooling that builds and
    signs them is still missing.
  - **Richer paymaster auction.** Today the txpool ranks
    paymaster txs against native txs via oracle-derived QRL
    equivalence. A full auction (priority-fee for the paymaster,
    tip-per-gas split between coinbase and paymaster, etc.) is
    open work.

For day-to-day operations see
[`qsd-deployment.md`](qsd-deployment.md).
