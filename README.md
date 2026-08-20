# pbt-devnet

A differential devnet for the **EIP-8297 partitioned binary tree (PBT)**: two geth and two besu
nodes under test on the same Amsterdam-at-genesis chain, driven by real lighthouse consensus clients, with
every execution client required to agree on every state root — and reorged on purpose to check
they still agree afterwards.

The point is cross-implementation. Two instances of one binary agree by construction and prove
nothing; geth agreeing with besu is evidence the specification is unambiguous enough to implement
twice. So the pairs are deliberately configured differently:

| node | client | what makes it different |
|---|---|---|
| 1 | geth | **the bootnode** — never partitioned, and not one of the nodes under test |
| 2 | geth | `--state.size-tracking` |
| 3 | geth | archive, `--syncmode=full` |
| 4 | besu | `--data-storage-format=BINARY` |
| 5 | besu | also `--bonsai-limit-trie-logs-enabled=false` — keeps every trie log |

Node 1 exists because ethereum-package launches the **first** participant with no `--boot-nodes`
of its own and hands its ENR to everyone else. Partitioning that node strands it permanently — it
returns with no peers and nothing to rediscover through, then sits at zero peers while every later
scenario measures a starved node instead of a reorg. Giving the role to a node that is never
disrupted (`pbt_chaos.protect_nodes`) keeps all four clients under test eligible.

It composes [`ethpandaops/ethereum-package`](https://github.com/ethpandaops/ethereum-package)
rather than launching clients itself, with **no patches to that package** — the binary tree is
reached entirely through supported configuration.

## What you need

Docker, [Kurtosis](https://docs.kurtosis.com/install), Python 3, and a JDK 25 for the besu build
(`brew install openjdk@25`; it is keg-only and will not become your default java).

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
reorgs land on geth and besu alike; `make chaos-status` reports the tally per client. Its own
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
to four live clients.

