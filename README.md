# cardano-txpump

`cardano-txpump` generates and submits configurable transaction workloads to
Cardano nodes over Ouroboros node-to-client. It supports payment, delegation,
governance, and Plutus workloads for DevNet and Antithesis runs.

## Build

```sh
go build ./cmd/txpump
```

## Run

The container entrypoint reads transaction pump settings from environment
variables. For a local run, set at least `TXPUMP_NODE_ADDR` and
`TXPUMP_NETWORK_MAGIC`. Workload, fallback, genesis, funding-key, and timeout
settings are documented on the fields and environment parsing in
[`internal/txpump/config.go`](internal/txpump/config.go).

`TXPUMP_GENESIS_UTXO_FILE` accepts a directory of generated JSON files or a
single Shelley genesis JSON file. The matching `genesis.<n>.skey`, `.vkey`, and
`.addr.info` files are loaded from that directory or the JSON file's parent.
Explicit UTxO arrays must include an `address` field matching a loaded key to
be spendable by the command.

The published image is `ghcr.io/blinklabs-io/cardano-txpump:main`.
