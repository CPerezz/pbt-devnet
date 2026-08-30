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

Everything above runs the tree **from genesis**. The `args/migration*.yaml` profiles test the
other half of the story — [EIP-8347](https://eips.ethereum.org/EIPS/eip-8347)'s live switch: the
chain STARTS on the merkle-patricia trie with `binaryTrieTime` scheduled in the future, every geth
converts and imports a binary snapshot of the (empty) starting state at first boot, a follower
builds the binary tree in the background while spamoor loads the chain, and at the fork the header
root swaps — after which a reverse direction shadows the merkle side until finality closes the
window and the node reports the migration done.

```
kurtosis run . --enclave pbt --args-file args/migration.yaml --privileged
kurtosis service logs pbt migration-monitor -f     # phases, roots, findings as JSONL
make verify-migration ENCLAVE=pbt LOGS_DIR=... BINARY_TRIE_TIME=...
```

| profile | shape | takes |
|---|---|---|
| `migration-smoke-fast` | 1 node, fork at ~block 40 | ~14 min |
| `migration-smoke` | 1 node, fork at ~block 100 | ~20 min |
| `migration-chaos-smoke` | 4 nodes, one 3-minute isolation, fork far away | ~15 min |
| `migration-straddle-smoke` | 4 nodes, one partition spanning the fork | ~30 min |
| `migration-composite-smoke` | 4 nodes, all three phases: before, across, after | ~35 min |
| `migration-quiet` | 4 nodes, no chaos, full acceptance timeline | ~40 min |
| `migration` | 4 nodes, full pre-fork chaos schedule | ~50 min |
| `migration-composite` | 4 nodes, the whole lifecycle incl. the at-genesis suite after the switchover | ~65 min |

`make lap` runs one profile end to end: it starts the devnet, prints one line per change from
the monitor's live view, restarts a node in the gap the schedule leaves for it, asks for reorg
scenarios once the switchover is done, dumps every service log and runs the acceptance checks.

`migration-monitor` watches every client's migration progress, cross-checks shadow roots
between nodes, and serves a live view (`make ui` prints its URL) with a fork panel and a matrix
of roots per node. `migration-chaos` drives partitions on a fixed schedule that refuses any
window still open when the network has to be whole again. `migration-gate` holds the
at-genesis reorg service back until every client has finished migrating, runs one partition of
its own on the far side of the boundary, then hands disruptoor over — so exactly one thing
disrupts the network at a time. `verify-migration` judges a finished run and exits with the
number of failed checks, listing separately any check whose precondition never occurred.

**The reorg that matters most** is the one spanning the activation: each side of a partition
crosses the fork on its own block, and when it heals the losing side has to rewind *across the
header-root format swap* and re-cross. Nothing else exercises that path. Finding it wedged a
node for 30 seconds and failed the import, which is fixed in the geth revision this pins.

Validator stake is deliberately uneven — the deep victim holds 40%, so isolating it stalls
finality below the two-thirds threshold for the window. With uniform stake the majority
finalizes past the victim and the survivors ban its consensus client for good: measured over
several runs, not theorised, and the reason a 12-minute partition never healed while a
3-minute one always did.

## Adding another execution client

The migration profiles are geth-only today — `main.star` refuses a migration run with any
other client, so a half-onboarded participant fails at plan time instead of coming up
merkle-forever and quietly thinning the evidence. Everything above is written so that a
second client is an addition rather than a rewrite. It needs five things:

1. **A migration bootstrap** — convert the merkle genesis, import the artifacts, mark the
   datadir so restarts skip the work. For geth this is `scripts/geth-shim.sh`, wrapped into
   the image so the launcher's own command line is untouched.
2. **An introspection adapter** — migration progress and the shadow root of a block, behind
   `migmon.Client`. Without one the client is still watched over standard RPC and its events
   say the surface is unavailable; the monitor never reads silence as agreement.
3. **A registry entry** (`internal/migmon/registry.go`) — the client's evidence contract in
   one place: whether it introspects, whether its bootstrap logs artifact digests, its reorg
   log pattern, whether it serves orphaned blocks by hash, and the log phrase that would
   betray a configured migration window. The monitor and the acceptance checks scope
   themselves from this entry, so a client that cannot produce some evidence degrades to a
   named INCONCLUSIVE instead of a false FAIL — and `MIGRATION_READY_CLIENTS` in `main.star`
   is the plan-time face of the same list.
4. **A reorg log pattern**, so a heal can be corroborated from the client's own log when the
   monitor did not sample the reorged height itself.
5. **An args participant block**, and optionally `pbt_migration.heavy_node` pointed at it to
   put that client in the victim seat.

**Erigon is not ready for this yet**, and the gap is in the client, not here: it parses
`binaryTrieTime` but its own genesis validation rejects any value later than the genesis
timestamp (`this node can only commit through the binary tree from block 0`), and the
commitment variant is a whole-datadir property chosen at `erigon init` from an environment
variable. It has no follower building one tree while executing on the other, no snapshot
import, and no migration introspection. It participates fully in the tree-at-genesis devnet
(`args/devnet.yaml`), which is where it belongs until in-place migration exists.
