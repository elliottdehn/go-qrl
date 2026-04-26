# QSD Stability Layer Contracts

Solidity sources, tests, and deployment scripts for the four
stability-layer predeploys baked into the go-qrl chain:

| Contract | Reserved address | Role |
|----------|------------------|------|
| `ValidatorOracle` | `0x0000000000000000000000000000000000010000` | Per-block median QRL/USD price from validator votes. |
| `InverseQRL` (iQRL) | `0x0000000000000000000000000000000000010001` | ERC-20 whose canonical USD value tracks `1/p`. Minted by burning native QRL. |
| `QSD` | `0x0000000000000000000000000000000000010002` | Dollar stablecoin. ERC-20. LP token of a CPMM between QRL and iQRL. |
| `PayWithIQRL` | `0x0000000000000000000000000000000000010003` | Paymaster that lets txs settle gas in iQRL. |

The Go side embeds the deployed bytecode and storage of all four
contracts as a JSON dump (`go-qrl/core/qsd_predeploy_state.json`),
which the chain installs at the QSD-fork activation block. See
[`docs/qsd-stability-layer.md`](../docs/qsd-stability-layer.md) for
the protocol-level architecture and
[`docs/qsd-deployment.md`](../docs/qsd-deployment.md) for operator
runbooks.

## Layout

```
contracts/
├── src/                       Solidity sources (compiled into the predeploy dump)
│   ├── ValidatorOracle.sol
│   ├── InverseQRL.sol
│   ├── QSD.sol
│   ├── PayWithIQRL.sol
│   └── interfaces/
├── test/                      Foundry unit + invariant tests
├── script/
│   └── Predeploy.s.sol        Deploys all four, etches them to reserved addresses,
│                              and dumps the resulting state for the Go embed.
├── lib/                       Submodules: openzeppelin-contracts, forge-std
└── foundry.toml
```

## Setup

The contracts depend on `openzeppelin-contracts` and `forge-std` as
git submodules. After cloning go-qrl:

```sh
git submodule update --init --recursive
```

You'll also need [Foundry](https://getfoundry.sh) on your `$PATH`.

## Build and test

```sh
cd contracts
forge build
forge test
```

The current suite is 85 tests across the four contracts plus a QSD
invariant fuzzer; all should pass.

## Regenerating the predeploy dump

The Go side embeds bytecode and storage produced by the deploy
script. Regenerate after any source change that affects deployed
state (constructor logic, `immutable` values, initial storage):

```sh
forge script script/Predeploy.s.sol --tc PredeployScript -vv
cp out/qsd-genesis-state.json ../core/qsd_predeploy_state.json
```

If you change `voteStalenessBlocks`, `minQuorumNumerator`, or
`minQuorumDenominator` in the deploy script, also update the
matching Go constants in `core/qsd_predeploy.go` and rebuild
`go-qrl`.
