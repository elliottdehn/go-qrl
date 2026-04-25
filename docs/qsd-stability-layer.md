# QSD Stability Layer

The QSD stability layer is a set of three on-chain primitives that
together implement a QRL-native, dollar-denominated stablecoin backed
by QRL alone. It is deployed as plain Solidity at fixed addresses in
the application-reserved namespace; there are no consensus-layer
changes.

This document describes the layer at the level a node operator or
deploy-script author needs. The protocol-level rationale lives in the
QRL grant proposal.

## Components

| # | Contract           | Reserved address                               | Role |
|---|--------------------|------------------------------------------------|------|
| 1 | `ValidatorOracle`  | `0x0000000000000000000000000000000000010000`   | Per-block median QRL/USD price from validator votes. |
| 2 | `InverseQRL` (iQRL)| `0x0000000000000000000000000000000000010001`   | ERC-20 whose canonical USD value tracks `1 / p`. Minted by burning QRL. |
| 3 | `QSD`              | `0x0000000000000000000000000000000000010002`   | Dollar stablecoin. ERC-20 + EIP-2612. Backed by a symmetric (QRL, iQRL) pool. |

Addresses are sequential at the start of the application-reserved
band (`0x010000+`), clear of all precompile slots. They are exposed
as constants in `core/qsd_predeploy.go`:

```go
core.ValidatorOracleAddress
core.InverseQRLAddress
core.QSDAddress
```

Downstream tooling (deploy scripts, the `qsdfeeder` daemon, RPC
clients) should target these constants by name rather than
hard-coding the hex.

## Solvency invariant

QSD is the LP token of a CPMM between QRL and iQRL. Because iQRL is
inverse-priced (`USD(iQRL) = 1/p`), the pair is invariant under price
moves: there is no impermanent loss, and no swap fee is charged.

Per-deposit solvency follows from AM-GM applied to the depositor's
contribution; aggregate solvency follows from Cauchy-Schwarz over per-
deposit contributions. Concretely:

```
2 · sqrt(k) ≥ totalSupply()
```

where `k` is the CPMM product of pool reserves. This invariant is
asserted in the contract on every state mutation.

## Genesis pre-deploy

`AddQSDStabilityLayer(alloc, params)` registers the three addresses
in a `GenesisAlloc`. The dev-genesis path (`DeveloperGenesisBlock`)
calls this with `DefaultQSDPredeployParams(faucet)`:

```go
DefaultQSDPredeployParams(owner) = {
    OracleOwner:          owner,
    VoteStalenessBlocks:  10,
    MinQuorumNumerator:   2,
    MinQuorumDenominator: 3,
}
```

### Status

The current implementation **only reserves the addresses** (zero
balance, empty code). Bytecode + storage seeding from the Foundry
artifacts lands in a follow-up commit so the address scheme, helper
shape, and dev-genesis wiring can be validated independently.

For non-developer chains, deployment is the operator's responsibility
until the bytecode is baked in.

## Validator daemon

Validators run `qsdfeeder` to post price votes to `ValidatorOracle`
on a configurable cadence. See [`cmd/qsdfeeder/README.md`](../cmd/qsdfeeder/README.md)
for build and operation.

The vote calldata is `submitVote(uint256)` — selector `0x2844328f`,
followed by the price as a 32-byte big-endian uint256, scaled by
`1e18` (so `$1.00 / QRL` is `1_000_000_000_000_000_000`).

## Roadmap

The branch `qsd-stability-layer` lands the layer in three additive
commits:

1. **Genesis pre-deploy** — reserves the three addresses (this commit).
2. **Validator daemon** — `cmd/qsdfeeder` with a stub submitter.
3. **Real submitter + bytecode bake** — wallet-signed transactions and
   populated `GenesisAccount{Code, Storage}` from the Foundry build.

Each step is independently testable; nothing in earlier steps depends
on later ones.
