# pbt-devnet

A differential devnet for the **EIP-8297 partitioned binary tree (PBT)**: two geth and two besu
nodes on the same Amsterdam-at-genesis chain, driven by real lighthouse consensus clients, with
every execution client required to agree on every state root — and reorged on purpose to check
they still agree afterwards.

The point is cross-implementation. Two instances of one binary agree by construction and prove
nothing; geth agreeing with besu is evidence the specification is unambiguous enough to implement
twice. So the pairs are deliberately configured differently:

| node | client | what makes it different |
|---|---|---|
| 1 | geth | `--state.size-tracking` |
| 2 | geth | archive, `--syncmode=full` |
| 3 | besu | `--data-storage-format=BINARY` |
| 4 | besu | also `--bonsai-limit-trie-logs-enabled=false` — keeps every trie log |

The besu pair matters most for reorgs: besu unwinds a branch by **reversing trie logs** where geth
replaces layers, so if the pruning node fails a deep reorg and the retaining one survives it, the
difference names the cause. Four nodes rather than three is also what lets the network finalize
through a partition — isolating one of three leaves the majority at exactly 2/3, and finality needs
more than that.

It composes [`ethpandaops/ethereum-package`](https://github.com/ethpandaops/ethereum-package)
rather than launching clients itself, with **no patches to that package** — the binary tree is
reached entirely through supported configuration.

## What you need

Docker, [Kurtosis](https://docs.kurtosis.com/install), Python 3, and a JDK 25 for the besu build
(`brew install openjdk@25`; it is keg-only and will not become your default java).

## Sources it builds from

| image | from | branch |
|---|---|---|
| `pbt-geth:local` | [`CPerezz/go-ethereum`](https://github.com/CPerezz/go-ethereum) | `pbt` |
| `besu-pbt:local` | [`CPerezz/besu`](https://github.com/CPerezz/besu) | `fix/pbt-fcu-null-trie-node` |
| `pbt-egg:local` | [`CPerezz/ethereum-genesis-generator`](https://github.com/CPerezz/ethereum-genesis-generator) | `pbt` |
| *(library)* | [`besu-eth/besu-stateless`](https://github.com/besu-eth/besu-stateless) | `feat/partitioned-binary-trie` |

```bash
git clone -b pbt                          https://github.com/CPerezz/go-ethereum                ../go-ethereum
git clone -b pbt                          https://github.com/CPerezz/ethereum-genesis-generator ../egg-pbt
git clone -b fix/pbt-fcu-null-trie-node   https://github.com/CPerezz/besu                       ../besu-pbt
git clone -b feat/partitioned-binary-trie https://github.com/besu-eth/besu-stateless            ../besu-stateless
```

Clone them beside this repo, or point `PBT_GETH_SRC` / `PBT_BESU_ROOT` / `PBT_EGG_SRC` /
`PBT_BESU_STATELESS` at them. The besu branch is `matkt/besu@glamsterdam-devnet-8-pbt` plus
[matkt/besu#31](https://github.com/matkt/besu/pull/31); without that fix besu never leaves block 0
(see "What this has found").

## Run

```bash
make besu     # once: besu-stateless -> mavenLocal, then besu installDist, then the image
make up       # build the rest, start the devnet, follow the monitor
make fast     # a two-node, low-traffic profile for a four-minute debug loop
make down     # stop and remove
```

`make besu` is separate because it is a two-stage Gradle build, not a `docker build`:
`besu-stateless` must be published to the local Maven repository before besu will compile against
it. `make besu-image` rebuilds just the image from an existing `build/install/besu`.

Within a minute or so the monitor should be following a chain all four clients agree on, and before
long a reorg:

```
chain number=412 hash=9f59de..701857 state_root=422e45..74488b clients=4
reorg observed client=el-2-geth-lighthouse height=30 before=0xf3c58abc64 after=0x26cd600406
```

**The devnet is not quiet by default** — chaos is on and reorgs happen without asking. Set
`pbt_chaos.enabled: false` in `args/devnet.yaml` for a baseline run. Bare `make` lists every
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

Ports are `public_port_start + 2×index` over `additional_services`, so **reordering that list in
`args/devnet.yaml` moves every UI**. `make ui` reads the real ports rather than assuming these.

## Checking it works

```bash
make status     # every client's head and state root, side by side
make verify     # compare every client at the SAME block number  (BLOCKS=100)
make diagnose   # where the chain split, and what the peers were doing
make proposals  # who was due to propose each slot, and who missed
make forks      # competing heads, how deep each branch is, and who is on which
```

`make forks` exists because **forky cannot work on this chain**: under Gloas the beacon block
carries no execution payload, so lighthouse reports `validity: null` for every fork-choice
node and forky's parser discards the whole dump — it renders nothing and logs an error per
node per slot. The newest image is the one that fails and upstream has no fix, so it is not
installed. Everything else in that dump is intact, so `make forks` rebuilds the tree itself.
Dora's `/forks` page is the equivalent UI and has real history.

`make verify` compares the most recent N blocks, not blocks 1..N. That distinction is
load-bearing: a divergence is permanent once it happens, so anchoring at block 1 lets early
agreeing blocks outvote a chain that has been split for twenty minutes. This script used to do
exactly that and reported a confident PASS on a devnet running three separate chains.

`make proposals` refuses to report while disruptoor has any state applied — asking a majority node
whether a partitioned node's slots have blocks measures the partition, not the proposer. An early
93% miss rate for besu was exactly that mistake; measured quietly, every node misses nothing.

`pbtmonitor` runs the same comparison continuously and additionally **proves its own oracle** at
startup: it builds a payload, corrupts one byte of the state root, and requires every other client
to reject it. Without that, "0 findings" from a broken oracle looks exactly like "0 findings" from
a healthy chain. Both implementations reject it independently, with their own error text:

```
geth  invalid merkle root (remote: 6e9d20e5dc69… local: 6e9d20e5dcf8…)
besu  World State Root does not match expected value, header 0x6e9d20e5dc69… calculated 0x6e9d20e5dcf8…
```

The monitor observes only. It never proposes and never sets a head — a second thing driving
forkchoice alongside real consensus clients manufactures the very forks it would then report.

## Load

**spamoor** provides volume through general scenarios (`eoatx`, `deploytx`, `setcodetx`,
`storagespam`). **`pbthammer`** provides the shapes spamoor has no scenario for — eleven workloads
chosen for what EIP-8297 changed rather than for throughput: fresh accounts, scattered storage,
code shared between accounts, self-destructing children, 7702 delegate/re-delegate/**clear-to-zero**,
storage zeroization (on this tree zero is an *absence*, so storing it deletes), `SLOAD` through a
`CALL`, `EXTCODESIZE`/`EXTCODECOPY` over a large contract, legacy and access-list envelopes, and a
transaction that reverts after writing.

Gas is two-dimensional here (`StateGas = bytes_of_new_state × 1530`), so a fresh account costs
207,391 and a fresh storage slot 111,234, while a transfer to an *existing* account is still
21,000. Nothing is hardcoded: every transaction is priced with `eth_estimateGas` on every client,
and a disagreement between clients is itself reported as a finding.

Blocks must average **under 50% of the gas limit**. Above the target the base fee rises 12.5% per
block and compounds with nothing to stop it, until no transaction can be paid for within a node's
1-ether cap. The rates that hold it there, and the measurement behind them, are on `pbt_hammer` in
`args/devnet.yaml` — check block fullness after changing them.

The hammer samples receipts and reports a per-workload tally, so a shape that silently stops
working is a finding rather than invisible traffic. A healthy run looks like this — every
workload succeeding, and only `revert` reverting, which is its whole purpose:

```
receipts sampled (successful/total) by_workload="accesslist=19/19 callread=21/21 codedup=20/20
delegate=18/18 destruct=18/18 extcode=22/22 fanout=19/19 legacy=23/23 revert=0/18
storage=19/19 zeroize=20/20"
```

## Chaos

**Reorgs happen on their own.** `pbtchaos` forces one every 15-30 blocks by cutting the p2p of
whichever node proposes next, for the two slots around its duty. The node still builds its block —
only publication is cut — so its own execution client takes that block as head while everyone else
builds on the parent; when the isolation lifts, the loser unwinds. The doomed node **rotates**, so
reorgs land on geth and besu alike; `make chaos-status` reports the tally per client. It owns
disruptoor exclusively, so two disruptions never overlap.

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

Each scenario carries one predicate, *is the doomed change visible here*, checked three times:
**true** on the minority before the heal, **false** on the majority at that moment, **false** on
every client afterwards. The first catches a write that never landed, which would otherwise let the
scenario pass on an absence that was always there. The second catches a transaction that leaked
past the partition onto the surviving branch. Only the third is a finding.

That last check runs at a **fixed block, four below the majority's head, recorded before the heal**
— never at head. A reorged-out transaction is still valid, so it re-enters the mempool and is mined
again within a block or two, legitimately recreating the state; at the recorded block the doomed
branch was never canonical.

```bash
make scenario NAME=code-shared DEPTH=20   # next node in the rotation is the minority
make scenario NAME=code-sole MINORITY=3   # or pin which node gets stranded
make chaos-status                         # running, queued, per-client coverage, results
make split ; make heal                    # partition by hand, outside the queue
make repeer                               # restart any client left with no peers
```

`make chaos-status` reports `orphaned_blocks` per scenario — how much chain the partition
threw away, matching geth's own `Chain reorg detected … drop=N`. **It is roughly `DEPTH`
divided by the number of nodes**, because the stranded node holds only its share of the
proposers: `DEPTH=8` orphans 1-3 blocks on a four-node net, `DEPTH=30` orphaned 13. Ask for a
large `DEPTH` if you want a deep abandoned branch. The periodic isolation forks are 1 block by
construction. While a scenario runs, the minority node is *expected* to show as Synchronizing and to sit behind the
tip for the length of the partition — deeper `DEPTH` means longer.

A scenario that cannot confirm the clients reconverged reports `inconclusive` rather than a
finding: state that differs across a network which never healed says nothing about anyone's reorg
handling. **Seeing no forks at all?** Check `make chaos-status` first — a quiet chain usually means
pbtchaos is stopped, which is what a baseline run needs.

## What is covered, and what is not

Deliberately not covered: stateless clients and `debug_executionWitness` (no longer a goal), teku
(its Gloas-at-genesis state is mutually exclusive with lighthouse's — see below), and any builder
or MEV path.

Known gaps, in rough order of how much they would be worth closing:

- **Reorg depth is barely exercised.** `DEPTH` accepts anything, but the useful boundary is geth's
  `Engine API maximum reorg depth depth=32` — below it geth re-executes forward, and PBT's
  `Recoverable()` is false either way. A sweep at 10 / 20 / 30 and one crossing 32 is the obvious
  next run.
- **Nothing tests a client rejoining from cold.** `make repeer` restarts a stranded node, but a
  client down for many epochs takes a sync path none of this touches.
- **Single consensus client.** Every node runs lighthouse, so a consensus-side bug is invisible
  here by construction.
- **Partitions cost peers.** Clearing a partition removes every network rule, but lighthouse does
  not reliably rebuild its peer set: it sits at the same slot as everyone else, so it never
  measures itself as behind and never range-syncs. `make repeer` is the repair.

## Debugging one client on its own

The fastest way to isolate a client is to take the consensus layer out entirely:

```bash
make genesis                                    # regenerate genesis/genesis.json, print its root
docker run --rm -v $PWD/genesis:/g besu-pbt:local \
  --genesis-file=/g/genesis.json --data-storage-format=BINARY --sync-mode=FULL \
  --rpc-http-enabled --p2p-enabled=false
```

then compare `eth_getBlockByNumber(0)` against geth's. Both must produce:

```
state root  0x7e16e8798b8f3d27d4bea3c13cac4b459fdaa8e0baca8089ce1fb834f2b2820a
block hash  0x52327d2df655a26b98cf29f76232c9568f4ce10c3753e39729f1a8b43ae71c55
```

Seeing `0x16f3bf8b…c70145` instead means the client built a merkle-patricia genesis — see
Configuration. This is how the first besu bug was isolated, and a single-client enclave is
supported for the same reason: set `pbt_monitor.enabled: false` and run one participant.

## Configuration

`args/devnet.yaml` is ethereum-package's own schema apart from the three `pbt_*` blocks at the end.
It fails on any key it does not recognise, so consult its README rather than inventing fields.
Three settings there are load-bearing and easy to break — `network: "kurtosis"` (anything else
silently selects snap sync, which the tree refuses), `preset: mainnet` (`minimal` splits the devnet
into N healthy-looking chains at block 35), and `gloas_fork_epoch: 0` (Amsterdam at block 0 forces
Gloas at slot 0). Each carries its reasoning next to the value.

Both clients take the **same** genesis key, a fork activation timestamp:

| | genesis key | runtime flag |
|---|---|---|
| geth | `"binaryTrieTime": 0` | *(none — read from genesis)* |
| besu | `"binaryTrieTime": 0` | `--data-storage-format=BINARY` |

geth used to take a `"pbt": true` boolean and model the tree as a property of the chain rather than
a fork; [PR #26](https://github.com/CPerezz/go-ethereum/pull/26) made it a timestamp fork and
adopted besu's key, and mid-chain schedules are now accepted by both.

**A genesis still carrying `"pbt": true` is worse than one carrying nothing.** It decodes
fork-less, so the chain comes up on the merkle-patricia trie with no error anywhere — the genesis
hash is `0x16f3bf8b…c70145` instead of `0x52327d2d…ae71c55`, and nothing says why. The fork-order
check requires Amsterdam scheduled with `binaryTrieTime` no earlier than `amsterdamTime`.

## What this has found

- **besu: NPE on binary-trie node deletion** — [matkt/besu#31](https://github.com/matkt/besu/pull/31).
  The PBT commit visitor signals a deleted node as `store(location, null, null)` because
  `NodeUpdater` has no remove method; `BinaryStateRootCommitter` forwarded that to `putTrieNode`,
  which dereferences the hash. It fires only on the forkchoice path — `newPayload` validates with
  storage frozen, so the commit is skipped and the state root comes out correct — which is why the
  root matched geth while the chain never moved. Fixed by routing deletions to `removeTrieNode`.
- **besu: cross-fork world-state roll produces the wrong root** — `StateRootMismatchException` on
  a roll across a fork, after which the node is permanently wedged. Besu reverses via trie logs
  where geth replaces layers. Tracked in [#7](https://github.com/CPerezz/pbt-devnet/issues/7).
- **geth/besu disagree on `eth_estimateGas`** — 18,915,434 vs 19,491,192 on a 12,015-byte
  deployment, a 3.04% gap. Execution agrees, so it is estimation only. Unexplained.
- **lighthouse and teku need mutually exclusive Gloas-at-genesis states** —
  [#6](https://github.com/CPerezz/pbt-devnet/issues/6). Teku follows the specification; lighthouse
  does not; no single `genesis.ssz` satisfies both. This devnet targets lighthouse.
