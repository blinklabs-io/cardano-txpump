# cardano-txpump

`cardano-txpump` generates and submits configurable transaction workloads to a
Cardano node over Ouroboros node-to-client. It is used to exercise payment,
delegation, governance, and Plutus transaction paths in DevNet and Antithesis
runs.

## What it does

At startup, txpump loads funded UTxOs and their signing keys into an in-memory
wallet. It repeats these steps until it receives SIGINT or SIGTERM:

1. Choose a random batch size and connect over N2C, trying the fallback address
   if the primary connection fails.
2. Reconcile the wallet with the node's UTxOs and pending transactions when
   supported.
3. Choose an epoch-enabled workload type at random for each transaction and
   attempt the batch.
4. Close the connection and wait for a random cooldown.

New outputs stay out of coin selection for the configured confirmation
window. Random choices use `crypto/rand`, allowing Antithesis to substitute a
replayable deterministic source.

The workload types exercise different ledger paths:

- `payment` sends ADA back to a wallet-controlled address, creating new UTxOs.
- `delegation` registers a stake credential and delegates it to the configured
  pool; it becomes eligible from epoch 1 when a pool key hash is set.
- `governance` registers or updates a DRep credential, starting in epoch 2.
- `plutus` locks ADA at an always-succeeds Plutus V3 script and can later spend
  that output; it starts in epoch 3 and needs the network's Plutus V3 cost
  model to unlock outputs.

Submitted and rejected transactions are reported as JSON logs and appended to
`$TXPUMP_LOG_DIR/txpump.log` (default `/logs/txpump.log`).

## Run locally

The txpump process reads its configuration from environment variables. It
needs spendable initial UTxOs and their matching signing keys, plus an N2C
connection to a node. To build and run a payment-only workload against a node
that exposes N2C on `127.0.0.1:3002`:

```sh
go build -o ./cardano-txpump ./cmd/txpump

TXPUMP_NODE_ADDR=127.0.0.1:3002 \
TXPUMP_NETWORK_MAGIC=42 \
TXPUMP_GENESIS_UTXO_FILE=./config/utxos.json \
TXPUMP_LOG_DIR=./logs \
TXPUMP_TYPES=payment \
./cardano-txpump
```

`TXPUMP_NODE_ADDR` can be a TCP `host:port` address or a Unix socket path such
as `/ipc/node.socket`. Set the address and network magic for the node you are
using.

## Run the container

The published image is `ghcr.io/blinklabs-io/cardano-txpump:main`. This example
uses host networking for a node listening on `127.0.0.1:3002` and mounts the
UTxO data, signing keys, and log directory:

```sh
docker run --rm --network host \
  -v "$PWD/config:/config:ro" \
  -v "$PWD/logs:/logs" \
  -e TXPUMP_NODE_ADDR=127.0.0.1:3002 \
  -e TXPUMP_NETWORK_MAGIC=42 \
  -e TXPUMP_GENESIS_UTXO_FILE=/config/utxos.json \
  -e TXPUMP_TYPES=payment \
  ghcr.io/blinklabs-io/cardano-txpump:main
```

For a node that exposes a Unix socket, mount the socket into the container and
set `TXPUMP_NODE_ADDR` to its container path, for example
`/ipc/node.socket`.

## Funding data

`TXPUMP_GENESIS_UTXO_FILE` points to a JSON file or a directory containing JSON
files produced by the testnet-generation tool. It also accepts a Shelley
genesis JSON file with `initialFunds`. For an explicit UTxO list, a spendable
entry can include an `address` matching a loaded signing key. If `address` is
omitted, `txHash` must match the Blake2b-256 hash of a loaded signing key's
address. The key files sit beside the JSON file (or in the UTxO directory) and
use the names `genesis.<n>.skey`, `genesis.<n>.vkey`, and
`genesis.<n>.addr.info`.
Each explicit record has a 32-byte hex `txHash`, an output `index`, an `amount`
in lovelace, and optionally a hex `address`; the JSON field names are `txHash`,
`index`, `amount`, and `address`.

For example, the mounted config directory can contain:

```text
config/
├── utxos.json
├── genesis.1.skey
├── genesis.1.vkey
└── genesis.1.addr.info
```

Keep signing key files out of source control. The txpump exits at startup if it
cannot find spendable UTxOs matched to signing keys.

## Configuration

| Variable | Default | Purpose |
|---|---:|---|
| `TXPUMP_NODE_ADDR` | `/ipc/node.socket` | N2C Unix socket path or TCP `host:port`. |
| `TXPUMP_NETWORK_MAGIC` | `42` | Network magic; read from `TXPUMP_GENESIS_FILE` when unset. |
| `TXPUMP_GENESIS_UTXO_FILE` | unset | Initial UTxOs and the directory containing their signing keys. Required for a funded run. |
| `TXPUMP_GENESIS_FILE` | unset | Testnet spec or Shelley genesis JSON for epoch, slot, and system-start parameters. YAML specs need a resolved `systemStartUnix`. |
| `TXPUMP_TYPES` | `payment,delegation,governance,plutus` | Comma-separated workload types. |
| `TXPUMP_TRANSACTION_ERA` | `conway` | `conway` supports all workload types; `dijkstra` supports payments only. |
| `TXPUMP_TX_COUNT_MIN` / `TXPUMP_TX_COUNT_MAX` | `1` / `10` | Minimum and maximum transactions per batch. |
| `TXPUMP_COOLDOWN_MIN` / `TXPUMP_COOLDOWN_MAX` | `500` / `2000` | Minimum and maximum delay between batches, in milliseconds. |
| `TXPUMP_CONFIRMATION_SLOTS` | `30` | Slots to wait before reusing newly created outputs. |
| `TXPUMP_STARTUP_TIMEOUT` | `60` | Seconds allowed for startup, including waiting for genesis, establishing a connection, and submitting the first transaction; `0` disables the deadline. |
| `TXPUMP_LOG_DIR` | `/logs` | Directory for structured transaction logs. |

Delegation workloads need a pool key hash. The container entrypoint can derive
it from `/configs/keys/cold.vkey`; otherwise set
`TXPUMP_DELEGATION_POOL_KEY_HASH`. Plutus workloads use the cost model from
`TXPUMP_CONWAY_GENESIS_FILE`; Plutus unlock transactions are skipped when no
model is loaded. If this file is configured but unreadable or invalid, startup
fails. `TXPUMP_FALLBACK_ADDR` can name a second N2C address to try when the
primary address fails.

See [`internal/txpump/config.go`](internal/txpump/config.go) for all supported
variables and validation rules.
