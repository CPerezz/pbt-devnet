# pbt-devnet

A differential devnet for the **EIP-8297 partitioned binary tree (PBT)**: two geth, two besu and
two erigon nodes under test on the same Amsterdam-at-genesis chain, driven by real lighthouse
consensus clients, with every execution client required to agree on every state root — and reorged
on purpose to check they still agree afterwards.

The point is cross-implementation. Two instances of one binary agree by construction and prove
nothing; three implementations agreeing is evidence the specification is unambiguous enough to
implement three times. So the pairs are deliberately configured differently:

| node | client | what makes it different |
|---|---|---|
| 1 | geth | **the bootnode** — never partitioned, and not one of the nodes under test |
| 2 | geth | `--state.size-tracking` |
| 3 | geth | archive, `--syncmode=full` |
| 4 | besu | `--data-storage-format=BINARY` |
| 5 | besu | also `--bonsai-limit-trie-logs-enabled=false` — keeps every trie log |
| 6 | erigon | the tree, taken from the genesis |
| 7 | erigon | also `--prune.include-commitment-history` — keeps the commitment history |

Erigon needs no trie configuration: it reads `binaryTrieTime` out of the genesis and records the
tree, with blake3, when `erigon init` creates the datadir. That matters because its launcher runs
`erigon init <genesis.json> && erigon ...` in one shell while `el_extra_params` extends only the
second half — a flag would miss `init` entirely. `--prune.include-commitment-history` on node 7 is
a flag rather than a genesis key because it is read at node start and not by `init`; it is a
whole-datadir property from then on, and the node refuses to restart without it once the datadir
carries it.

Node 1 exists because ethereum-package launches the **first** participant with no `--boot-nodes`
of its own and hands its ENR to everyone else. Partitioning that node strands it permanently — it
returns with no peers and nothing to rediscover through, then sits at zero peers while every later
scenario measures a starved node instead of a reorg. Giving the role to a node that is never
disrupted (`pbt_chaos.protect_nodes`) keeps all six clients under test eligible.

It composes [`ethpandaops/ethereum-package`](https://github.com/ethpandaops/ethereum-package)
rather than launching clients itself, with **no patches to that package** — the binary tree is
reached entirely through supported configuration.

## What you need

Docker, [Kurtosis](https://docs.kurtosis.com/install) **1.20 or newer**, Python 3, and a JDK 25 for
the besu build (`brew install openjdk@25`; it is keg-only and will not become your default java).
Kurtosis 1.15.1 fails to interpret `ethereum-package` with `undefined: GpuConfig`; 1.20.0
works. The first good version in between was not bisected. Erigon needs
nothing beyond docker — `make build` clones and builds it like geth.

## Run

```bash
make besu     # once: besu-stateless -> mavenLocal, then besu installDist, then the image
make up       # build the rest, start the devnet, follow the monitor
make down     # stop and remove
```

`make besu` is separate because it is a two-stage Gradle build, not a `docker build`:

```
chain number=<n> hash=<block> state_root=<root> clients=4
reorg observed client=el-2-geth-lighthouse height=<n> before=<hash> after=<hash>
```

**The devnet is not quiet by default** — chaos is on and reorgs happen without asking. Add a
`pbt_chaos:` block with `enabled: false` to `args/devnet.yaml` for a baseline run; the file ships
with no overrides, so every knob comes from `main.star`. Bare `make` lists every
target; Ctrl-C detaches from the logs without stopping anything.

## The UIs

```bash
make ui       # prints the live URLs
```

| | default | what it is for |
|---|---|---|
| dora | http://127.0.0.1:36000 | block explorer — slots, epochs, the chain itself |
| spamoor | http://127.0.0.1:36002 | transaction spammer; its scenarios and throughput |
| assertoor | http://127.0.0.1:36004 | block-proposal and EOA-transaction checks, plus upstream's synchronized-check by URL |
| disruptoor | http://127.0.0.1:36006 | chaos control; `/containers` and `/events` |
| pbtchaos | *(dynamic)* | scenario control: `GET /status`, `POST /scenario/{name}` |

## Checking it works

```bash
make status     # every client's head and state root, side by side
make verify     # compare every client at the SAME block number  (BLOCKS=100)
make diagnose   # where the chain split, and what the peers were doing
make proposals  # who was due to propose each slot, and who missed
make forks      # competing heads, how deep each branch is, and who is on which
```

## Chaos

**Reorgs happen on their own.** `pbtchaos` forces one every 15-30 blocks by cutting the p2p of
whichever node proposes next, for the two slots around its duty. The node still builds its block —
only publication is cut — so its own execution client takes that block as head while everyone else
builds on the parent; when the isolation lifts, the loser unwinds. The doomed node **rotates**, so
reorgs land on geth, besu and erigon alike; `make chaos-status` reports the tally per client. Its own
jobs run through one queue, so two of them never overlap.

On top of that, scenarios strand **specific state** on a branch that is then abandoned. Each
partitions the network, waits until the two sides genuinely disagree, sends its transactions to the
minority's RPC only, holds for `DEPTH` blocks, heals, and verifies.

| scenario | on the doomed branch | must be true after the heal |
|---|---|---|
| `code-sole` | unique bytecode, deployed once | no code on any client |
| `code-shared` | the same bytecode a surviving account already holds | the survivor's copy still reads back |
| `delegate` | 7702 delegations on fresh authorities | no code: back to a plain EOA |
| `account` | fresh funded accounts | zero balance everywhere |
| `storage-add` | slots written below and above `HEADER_STORAGE_OFFSET` (64) | both zero again |
| `storage-del` | slots deleted that existed before the split | the values are back |

`code-sole` and `code-shared` are the pair from
[go-ethereum#30](https://github.com/CPerezz/go-ethereum/pull/30) — chunks go when the dead branch
was their only writer, and stay when a surviving account still holds them — lifted from unit test
to six live clients.

## The migration

The `args/migration*.yaml` profiles test EIP-8347's live switch: the chain starts on the merkle
trie, every geth converts and imports a binary snapshot at boot, and at `binaryTrieTime` the
header root swaps while a shadow tree cross-checks the merkle side until finality closes the
window.

```
kurtosis run . --enclave pbt --args-file args/migration-composite.yaml --privileged
kurtosis service logs pbt migration-monitor -f     # phases, roots, findings as JSONL
make verify-migration ENCLAVE=pbt LOGS_DIR=... BINARY_TRIE_TIME=...
```

| profile | shape | takes |
|---|---|---|
| `migration-composite-smoke` | 4 nodes, all three phases: before, across, after | ~35 min |
| `migration-composite` | 4 nodes, the whole lifecycle, before/across/after the fork | ~90 min |

`make lap` runs one profile end to end. `migration-monitor` cross-checks shadow roots between
nodes and serves a live view (`make ui`). `migration-chaos` schedules partitions around the
fork. `migration-gate` holds the at-genesis reorg service until every client finishes
migrating, then hands disruptoor over. `verify-migration` judges a finished run.

The bootnode (participant 1) anchors the network with 40% of the stake and is never
partitioned, so fork choice always brings the lights back onto its chain. Deep partitions
isolate a pair of lights (40% together) to stall finality without letting them win, and the
straddle isolates every light on its own island across I* and heals once each has crossed on
its own block, so every test client is forced to rewind below I* and re-cross - the anchor
never does.

## Adding another execution client

The migration profiles are geth-only today — `main.star` refuses any other client at plan
time. Onboarding a client needs:

1. **A scheduled-fork build**: the client must run from the merkle trie with `binaryTrieTime`
   in the future and build the binary tree itself (geth's `pbt` branch does; the image is a
   plain fork build).
2. **An introspection adapter** (`migmon.Client`) reporting migration progress and shadow roots.
3. **A registry entry** (`internal/migmon/registry.go`) describing its evidence contract.
4. **A reorg log pattern** to corroborate a heal from the client's own log.
5. **An args participant block**, optionally as `pbt_migration.anchor_node`.

Erigon is not ready: its genesis validation rejects a future `binaryTrieTime`, and it has no
follower or migration introspection. It runs only in `args/devnet.yaml`.
