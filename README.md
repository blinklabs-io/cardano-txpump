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
  model to unlock outputs. With `TXPUMP_USERS` set, each process unlocks only
  script outputs it created during that run. Without a selection, the process
  adopts visible script outputs so a restart can continue unlocking them.
- `peer_payment` pays ADA from one genesis-keyed user to another, so outputs
  change owner. It does nothing when the wallet holds a single key.
- `escrow` has two users trade ADA through the cardano-templates escrow
  validator: one initiates, another deposits, then both complete the trade or
  either cancels. It needs at least two keys and the Plutus V3 cost model, and
  runs from epoch 3. Reference-script mode reserves an additional publisher
  key, so escrow requires three keys in that mode. Every script transaction is
  planned from measured execution units and checked offline against the Conway
  ledger rules and the Plutus evaluator; a transaction that fails that check is
  not submitted.
- `vesting` has one user lock ADA at the cardano-templates vesting validator
  for another. The beneficiary withdraws once the lock time has passed, using a
  validity range that starts strictly after it, and the owner may claw the
  value back at any time. It needs at least two keys, the Plutus V3 cost model
  and the genesis system start (`systemStartUnix` or Shelley genesis), because
  the lock time is POSIX milliseconds. It runs from epoch 3 and its
  transactions are verified offline like escrow's.
- `simple-transfer` has one user lock ADA for another through the
  cardano-templates simple-transfer validator, which only a transaction signed
  by its receiver can spend. The validator takes the receiver's key hash as a
  compile-time parameter, so the app applies it to the blueprint's program for
  each receiver it meets, which gives that receiver its own script hash and
  address; the receiver then claims the whole position. No extra service is
  needed: receivers are the other simulated users. It needs at least two keys
  and the Plutus V3 cost model, runs from epoch 3, and its transactions are
  verified offline like escrow's.

Each genesis signing key is one user. Registered apps such as `peer_payment`
run as a randomly chosen user each time; `payment` and the other core types
continue to draw from the whole wallet.

`TXPUMP_USERS` limits an instance to some of the loaded keys, so several
instances sharing one devnet can use disjoint keys and never spend the same
UTxOs. It takes zero-based positions and inclusive ranges, such as `0,2-3`,
into the `genesis.<n>.skey` files sorted by file name (so `genesis.10` sorts
before `genesis.2`). UTxOs of unselected keys are ignored for every type.
Startup fails on a malformed value, a position past the last key, or a
range ending past the last key. Ranges are kept compact until they are checked
against the loaded key count. Apps that need several users, such as
`peer_payment`, count only the selected ones. `plutus` outputs share a script
address, so selected instances track only outputs they created during their
current run; the default unselected mode adopts visible outputs for restart
recovery.

Every submission is reported as a JSON line appended to
`$TXPUMP_LOG_DIR/txpump.log` (default `/logs/txpump.log`). Its `status` is
`submitted` when the node accepted the transaction, `rejected` when the ledger
rejected it, and `error` when the connection failed before the node answered.
An `error` submission may still have reached the node, so with signing keys its
inputs stay reserved until wallet reconciliation finds the transaction absent.

Each batch that runs logs one `batch` line at the default `info` level;
per-transaction detail is in `txpump.log` and, with `TXPUMP_LOG_LEVEL=debug`, on
standard output. Connection and reconciliation problems are logged on their own. The level is one of `debug`, `info`, `warn` or `error`.

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
| `TXPUMP_TYPES` | `payment,delegation,governance,plutus` | Comma-separated workload types: the four core types plus registered apps (`peer_payment`, `escrow`, `vesting`, `simple-transfer`). |
| `TXPUMP_USERS` | unset (all keys) | Signing keys this instance uses, as positions or ranges such as `0,2-3`. |
| `TXPUMP_TRANSACTION_ERA` | `conway` | `conway` supports all workload types; `dijkstra` supports payments only. |
| `TXPUMP_TX_COUNT_MIN` / `TXPUMP_TX_COUNT_MAX` | `1` / `10` | Minimum and maximum transactions per batch. |
| `TXPUMP_COOLDOWN_MIN` / `TXPUMP_COOLDOWN_MAX` | `500` / `2000` | Minimum and maximum delay between batches, in milliseconds. |
| `TXPUMP_CONFIRMATION_SLOTS` | `30` | Slots to wait before reusing newly created outputs. |
| `TXPUMP_STARTUP_TIMEOUT` | `60` | Seconds allowed for startup, including waiting for genesis, establishing a connection, and submitting the first transaction; `0` disables the deadline. |
| `TXPUMP_LOG_DIR` | `/logs` | Directory for structured transaction logs. |
| `TXPUMP_CONWAY_GENESIS_FILE` | unset | Conway genesis supplying the Plutus V3 cost model. |
| `TXPUMP_ALONZO_GENESIS_FILE` | unset | Alonzo genesis supplying execution prices, the execution-unit limit and collateral settings for script apps. |
| `TXPUMP_REFERENCE_SCRIPTS` | `off` | Where script apps get their validator: `off` carries it in every witness set, `on` publishes it once and spends through a reference input, `mixed` chooses per spend. |

Delegation workloads need a pool key hash. The container entrypoint can derive
it from `/configs/keys/cold.vkey`; otherwise set
`TXPUMP_DELEGATION_POOL_KEY_HASH`. Plutus workloads use the cost model from
`TXPUMP_CONWAY_GENESIS_FILE`; Plutus unlock transactions are skipped when no
model is loaded. If this file is configured but unreadable or invalid, startup
fails. `TXPUMP_FALLBACK_ADDR` can name a second N2C address to try when the
primary address fails.

Script apps build and check their transactions against mainnet-like Conway
parameters. `TXPUMP_ALONZO_GENESIS_FILE`, when set, overrides the execution
prices, the per-transaction execution-unit limit and the collateral settings
with the network's Alonzo genesis values. The fee coefficients are not read
from genesis and stay at 44 lovelace per byte plus 155381.

## Reference scripts

With `TXPUMP_REFERENCE_SCRIPTS=on` or `mixed`, txpump publishes the validator
of each enabled script app (currently `escrow`) as a reference script, one
transaction per validator, and later script spends name the published output
as a reference input instead of carrying the validator in the witness set.
`off` keeps the witness-set path, so one testnet can run instances of both.

The wallet key that sorts last by address owns the published scripts. It is
not a user and no other workload selects its funds, so the outputs are never
spent by normal traffic. It needs funds of its own, a run needs at least two
keys, and every enabled workload needs its users besides the publisher
(three keys for `escrow` or `vesting`, which each need two users); txpump
refuses to start otherwise. In `on` mode a script spend waits until its
reference is on chain; in `mixed` mode it falls back to the witness set.

Nothing about published scripts is remembered between runs. Each round txpump
reads the publisher address's outputs from the node and keeps those whose
reference script hashes to an enabled app's validator, so a restart finds
earlier publications and publishes only what is missing. If the node's UTxO
query fails, nothing is published that round.

The ledger charges reference scripts a per-byte fee, tiered in 25600-byte
steps that each cost 1.2 times the last. The first tier's price is
`minFeeRefScriptCostPerByte` from `TXPUMP_CONWAY_GENESIS_FILE` and is 15 when
unset. The offline check applies that fee and the 200 KiB per-transaction
reference-script limit. It cannot apply the 1 MiB per-block limit, which
depends on the other transactions in a block, so a block that reaches it can
still reject a spend the check accepted.

See [`internal/txpump/config.go`](internal/txpump/config.go) for all supported
variables and validation rules.

## Template blueprints

Script apps embed compiled CIP-57 blueprints from
[cardano-templates](https://github.com/cardano-foundation/cardano-templates)
under `internal/txpump/blueprints/`. To regenerate them, put Aiken v1.1.22 on
`PATH`, check out cardano-templates at the commit pinned in the `Makefile`
(`TEMPLATES_COMMIT`), and run:

```sh
make blueprints TEMPLATES_DIR=<path to the cardano-templates checkout>
```

The loader rejects a blueprint whose stated hash does not match its code.
