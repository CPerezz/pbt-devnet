# pbt-devnet

A differential devnet for the **EIP-8297 partitioned binary tree (PBT)**: two geth and two besu
nodes running the same Amsterdam chain, driven by real lighthouse consensus clients, with every
execution client required to agree on every state root.

The point is cross-implementation. Two instances of one binary agree by construction and prove
nothing; geth agreeing with besu is evidence the specification is unambiguous enough to implement
twice. Everything here exists to make a disagreement visible and reproducible.

It composes [`ethpandaops/ethereum-package`](https://github.com/ethpandaops/ethereum-package)
rather than launching clients itself, with **no patches to that package** — the binary tree is
reached entirely through supported configuration.

## What you need

| | |
|---|---|
| Docker | running, with ~16 GB available to it |
| [`kurtosis`](https://docs.kurtosis.com/install) | `brew install kurtosis-tech/tap/kurtosis-cli` |
| `yq` | `brew install yq` |
| **JDK 25** | only for besu — `brew install openjdk@25` (keg-only, will not become your default `java`) |

`make check` reports whichever is missing.

## Sources it builds from

| image | from | branch |
|---|---|---|
| `pbt-geth:local` | [`CPerezz/go-ethereum`](https://github.com/CPerezz/go-ethereum) | `pbt` |
| `besu-pbt:local` | [`CPerezz/besu`](https://github.com/CPerezz/besu) | `fix/pbt-fcu-null-trie-node` |
| `pbt-egg:local` | [`CPerezz/ethereum-genesis-generator`](https://github.com/CPerezz/ethereum-genesis-generator) | `pbt` |

Clone them beside this repo, or point `PBT_GETH_SRC` / `PBT_BESU_ROOT` / `PBT_EGG_SRC` at them.

**The besu branch matters.** It is `matkt/besu@glamsterdam-devnet-8-pbt` plus
[matkt/besu#31](https://github.com/matkt/besu/pull/31). Without that fix besu computes the correct
state root, imports the block, and then refuses the forkchoice update that would make it the head —
so it sits at block 0 forever while geth advances. See "What this has found".

```bash
git clone -b pbt                        https://github.com/CPerezz/go-ethereum              ../go-ethereum
git clone -b pbt                        https://github.com/CPerezz/ethereum-genesis-generator ../egg-pbt
git clone -b fix/pbt-fcu-null-trie-node https://github.com/CPerezz/besu                     ../besu-pbt
git clone -b feat/partitioned-binary-trie https://github.com/besu-eth/besu-stateless        ../besu-stateless
```

## Run

```bash
make besu     # once: besu-stateless -> mavenLocal, then besu installDist, then the image
make up       # build the rest, start the devnet, follow the monitor
make down     # stop and remove
```

`make besu` is separate because it is a two-stage Gradle build, not a `docker build`:
`besu-stateless` must be published to the local Maven repository before besu will compile against
it. `make besu-image` rebuilds just the image from an existing `build/install/besu`.

Bare `make` lists every target. Ctrl-C detaches from the logs without stopping anything.

### What `make up` actually does

| stage | what happens | what you should see |
|---|---|---|
| `check`, `build` | builds `pbt-geth`, `pbt-egg`, `pbt-monitor`, `pbt-hammer`, `pbt-chaos`; besu comes from `make besu` | a warning if `besu-pbt:local` is missing |
| `kurtosis run --privileged` | `pbt-egg:local` writes a `binaryTrieTime` genesis, then 4 EL + 4 CL + 4 VC start | `--privileged` is for disruptoor alone, which enters other containers' network namespaces |
| first slots | the chain starts; `pbtmonitor` follows every client | `chain number=N … clients=4`, one state root shared by all four |
| ~2 epochs | attestations accumulate | finalized epoch advancing, in Dora or `make diagnose` |
| continuously | `pbthammer` cycles 11 PBT-shaped workloads; spamoor adds four more | blocks around 14% of the 200M gas limit, base fee near zero |
| every 15-30 blocks | `pbtchaos` isolates the next proposer for two slots | `reorg observed client=… height=… before=… after=…`, and the fork in forky |
| on demand | `make scenario NAME=… DEPTH=…` | `partitioned` → `healed` → `reconverged` → `scenario passed` |
| after a long split | a client can be left with no peers | `make repeer` restarts it; it rejoins in a slot or two |

The devnet is **not** quiet by default: chaos is on, and reorgs happen without asking. Set
`pbt_chaos.enabled: false` for a baseline run.

## The UIs

```bash
make ui       # prints the live URLs
```

| | default | what it is for |
|---|---|---|
| dora | http://127.0.0.1:36000 | block explorer — slots, epochs, the chain itself |
| spamoor | http://127.0.0.1:36002 | transaction spammer; the PBT scenarios and their throughput |
| assertoor | http://127.0.0.1:36004 | test playbooks, pass/fail |
| forky | http://127.0.0.1:36006 | fork-choice / reorg visualiser across all consensus clients |
| disruptoor | http://127.0.0.1:36008 | chaos control; `/containers` and `/events` |

Ports are `port_publisher.additional_services.public_port_start + 2×index` over
`additional_services`, so **reordering that list in `args/devnet.yaml` moves every UI**. `make ui`
reads the real ports rather than assuming these.

## Checking it works

```bash
make status   # every client's head and state root, side by side
make verify   # compare every client at the SAME block number  (BLOCKS=100)
```

`make verify` compares the most recent N blocks, not blocks 1..N. That distinction is
load-bearing: a divergence is permanent once it happens, so anchoring at block 1 lets early
agreeing blocks outvote a chain that has been split for twenty minutes. This script used to do
exactly that and reported a confident PASS on a devnet running three separate chains.

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

Two generators, deliberately:

**spamoor** provides volume through general scenarios (`eoatx`, `deploytx`, `setcodetx`,
`storagespam`), configured in `args/devnet.yaml`.

**`pbthammer`** provides the shapes spamoor has no scenario for — eleven workloads chosen for what
EIP-8297 changed rather than for throughput: fresh accounts, scattered storage, code shared between
accounts, self-destructing children, 7702 delegate/re-delegate/**clear-to-zero**, storage
zeroization (on this tree zero is an *absence*, so storing it deletes), `SLOAD` through a `CALL`,
`EXTCODESIZE`/`EXTCODECOPY` over a large contract, legacy and access-list envelopes, and a
transaction that reverts after writing.

Gas is two-dimensional here (`StateGas = bytes_of_new_state × 1530`), so a fresh account costs
207,391 and a fresh storage slot 111,234, while a transfer to an *existing* account is still
21,000. Nothing is hardcoded: every transaction is priced with `eth_estimateGas` on every client,
and a disagreement between clients is itself reported as a finding. The reverting workload is
priced from an identical non-reverting twin, because `eth_estimateGas` cannot price a call that
always reverts.

The revert workload is expected to produce `status=0` receipts. Everything else should be
`status=1`.

## Chaos

**Reorgs happen on their own.** `pbtchaos` runs continuously and forces a reorg every 15-30
blocks by cutting the p2p of whichever node proposes next, for the two slots around its duty.
The doomed node **rotates**, so reorgs land on geth and besu alike rather than always on the
same participant — `make chaos-status` reports the tally per client.
The node still builds its block — only publication is cut — so its own execution client takes
that block as head while everyone else builds on the parent; when the isolation lifts, the
loser unwinds. Watch them in forky and Dora. It owns disruptoor state exclusively and runs
everything through one queue, so two disruptions never overlap.

Delaying the proposer instead does **not** work, and the failure is silent: disruptoor v0
accepts only `scope: ["include_control"]` for shaping, which slows the engine API too, so the
proposer cannot assemble a payload before its deadline and skips the slot outright. A missed
slot reorgs nothing.

On top of that, scenarios strand **specific state** on a branch that is then reorged out, and
check every client agrees about that state afterwards. Each one partitions the network, sends
its transactions to the minority's RPC only, holds for `DEPTH` blocks, heals, and verifies.

| scenario | on the doomed branch | must be true after the heal |
|---|---|---|
| `code-sole` | unique bytecode, deployed once | no code on any client |
| `code-shared` | the same bytecode a surviving account already holds | the survivor's copy still reads back |
| `delegate` | 7702 delegations on fresh authorities | no code: back to a plain EOA |
| `account` | fresh funded accounts | zero balance everywhere |
| `storage-add` | slots written below and above `HEADER_STORAGE_OFFSET` (64) | both zero again |
| `storage-del` | slots deleted that existed before the split | the values are back |

`code-sole` and `code-shared` are the pair from
[go-ethereum#30](https://github.com/CPerezz/go-ethereum/pull/30) — chunks go when the dead
branch was their only writer, and stay when a surviving account still holds them — lifted from
unit test to two live clients.

Each run also names the branch that must survive: the majority's tip hash is recorded before
the heal and asserted afterwards, so a scenario cannot pass by reading its state assertions
the wrong way round if the doomed branch happens to win.

```bash
make scenario NAME=code-shared DEPTH=20   # next node in the rotation is the minority
make scenario NAME=code-sole MINORITY=3   # or pin which node gets stranded
make chaos-status                         # running, queued, per-client coverage, results
make split ; make heal                    # partition by hand, outside the queue
```

A scenario that cannot confirm the clients reconverged reports `inconclusive` rather than a
finding: state that differs across a network which never healed says nothing about anyone's
reorg handling.

**Seeing no forks at all?** Check `make chaos-status` first. A quiet chain usually means
pbtchaos is not running — it is stopped deliberately when taking a baseline, since
`make proposals` cannot measure a proposer's miss rate on a network that is being
partitioned. If the service is up and the chain is still quiet, the last few attempts will be
in its history as `no-reorg`, with the slot each one targeted.

Reorgs are the interesting case for a binary tree. Geth handles them by replacing layers rather
than reversing them — an abandoned branch is dropped from the layer tree, and anything it wrote
went with it — so shared, content-addressed code chunks never need reference counting. It refuses
only a fork at or below the persisted disk layer, and re-executes forward instead. See
`core/pbt_reorg_code_test.go` in the geth branch.

## What is covered, and what is not

Three layers, each answering a different question.

| layer | what it asks | what it would catch |
|---|---|---|
| `pbthammer`, 11 workloads | do the clients agree while writing every shape the tree changed? | a leaf, stem or chunk encoded differently by one client |
| spamoor, 4 spammers | does that hold under sustained mixed traffic? | ordering- or volume-dependent divergence |
| `pbtchaos`, isolation forks | does a client that has to abandon a block converge on the same state? | reorg handling: layer replacement vs trie-log reversal |
| `pbtchaos`, 6 scenarios | is specific state on an abandoned branch actually gone, everywhere? | a chunk, delegation or storage group kept or dropped wrongly |
| `pbtmonitor` | do all four agree on every root, and does the oracle still work? | silent agreement on a wrong root, or a dead check |

Deliberately not covered: stateless clients and `debug_executionWitness` (no longer a goal),
teku (its Gloas-at-genesis state is mutually exclusive with lighthouse's, see below), and any
builder or MEV path.

Known gaps, in rough order of how much they would be worth closing:

- **Reorg depth is barely exercised.** `DEPTH` accepts anything, but the useful boundary is
  geth's `Engine API maximum reorg depth depth=32` — below it geth re-executes forward, and
  PBT's `Recoverable()` is false either way. A sweep at 10 / 20 / 30 and one crossing 32 is
  the obvious next run.
- **Nothing tests a client rejoining from cold.** `make repeer` restarts a stranded node, but
  a client that has been down for many epochs takes a sync path none of this touches.
- **Single consensus client.** Every node runs lighthouse, so a consensus-side bug is
  invisible here by construction.

### Keep the chain under its gas target

Blocks must average below 50% of the gas limit, and it is worth checking after any change to
the hammer or spamoor. Above target the base fee rises 12.5% **per block** and compounds with
nothing to stop it: measured at the old settings, ten blocks ran 74.9% full and the base fee
went 3,600 → 11,579 gwei, at which point no scenario could pay for a transaction within the
node's 1 ether cap and five of six failed. At the current settings the same measurement is
14.3% full with the base fee at 0.1 gwei.

The hammer dominates, not spamoor: `interval` and `batch` set its rate, and `code_size`
multiplies by 1530 into state gas, so a 12,000-byte deploy alone costs ~18M.

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

Seeing `0x16f3bf8b…c70145` instead means the client ignored the tree and built a
merkle-patricia genesis — most likely a genesis still using the retired `"pbt": true` key. This is how the first besu bug was isolated, and a single-client enclave
is supported for the same reason — set `pbt_monitor.enabled: false` and run one participant.

## Configuration

`args/devnet.yaml` is ethereum-package's own schema apart from the two `pbt_*` blocks at the end.
It sanity-checks its input and fails on any key it does not recognise, so consult its README rather
than inventing fields.

Three settings there are load-bearing and easy to break:

- **`network: "kurtosis"`** — both execution launchers pick full sync *only* for that network name.
  Any custom name silently yields `--syncmode=snap` / `--sync-mode=SNAP`, which the binary tree
  refuses, and the engine API then goes quiet.
- **`gloas_fork_epoch: 0`** — PBT is a chain property requiring Amsterdam, so the execution layer
  is Amsterdam at block 0, which forces the consensus layer to be Gloas at slot 0. Almost nothing
  else starts *in* Gloas; devnets transition into it.
- **no `mev_type`** — a builder wires one shared payload source to participant 0's execution
  client, so every block would be built by that one node and the others would only ever import.
  Without it each proposer builds locally and both implementations are exercised.

Both clients now take the **same** genesis key, a fork activation timestamp:

| | genesis key | runtime flag |
|---|---|---|
| geth | `"binaryTrieTime": 0` | *(none — read from genesis)* |
| besu | `"binaryTrieTime": 0` | `--data-storage-format=BINARY` |

geth used to take a `"pbt": true` boolean and model the tree as a property of the chain rather
than a fork; [PR #26](https://github.com/CPerezz/go-ethereum/pull/26) made it a timestamp fork and
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

## Solved: besu appeared to orphan most of its blocks

Dora showed besu forking off constantly, and measuring bore it out: besu missed 27 of 29
proposal slots, 93%, against 0% for both geth nodes. That number was an artifact of the
harness. Every reorg scenario chose its minority as the *last* participant, which was always
besu, so besu spent the run partitioned — and the miss rate was measured by asking a majority
node whether besu's slots had blocks. A partitioned node's blocks do not reach the node being
asked, so this measured the partition, not the proposer.

Measured properly — four nodes, chaos stopped, three epochs, 81 proposal duties — every node
missed nothing at all:

```
proposer                         due  missed   miss %
el-1-geth-lighthouse              16       0     0.0%
el-2-geth-lighthouse              17       0     0.0%
el-3-besu-lighthouse              22       0     0.0%
el-4-besu-lighthouse              26       0     0.0%
```

All four publish at the same rate and about 30ms into their slot.

Two changes came out of it. The minority now rotates, so no client is permanently the doomed
one. And `make proposals` does this attribution properly, refusing to report a clean number
while disruptoor has any state applied.

## Solved: `preset: minimal` silently splits the devnet

Recorded because the symptom is so misleading. Under `preset: minimal` the consensus clients peer
normally, run for about three and a half minutes, then lose **every** peer within the same second
and never reconnect. Each then computes its own proposer schedule and drives its own execution
client down its own chain, so the devnet quietly becomes N separate chains that each look
completely healthy — every service `RUNNING`, blocks advancing, validators active.

Isolated to one variable:

| preset | nodes | result |
|---|---|---|
| minimal | 3 | diverges at block **35**, peers 2 → 0 after 3m30s |
| minimal | 2 | diverges at block **35**, peers 1 → 0 after 3m36s |
| mainnet | 2 | 67+ blocks agreeing, peers stable |
| mainnet | 3 | **100/100 blocks agreeing, peers stable** |

Block 35 exactly, at both node counts, so it is deterministic rather than a race. The mechanism
inside lighthouse is not identified — the ethpandaops `glamsterdam-devnet-8` images are built for
mainnet-preset devnets, and the working reference setups use that preset. Both args files now pin
`preset: mainnet` with a comment saying why.

`make diagnose` is the tool that found it: it prints the first divergent block next to the moment
each consensus client's peer count fell, and the two sitting at the same instant is the whole
finding.

## Known issues

- **Partitions cost peers.** Clearing a partition removes every network rule — the containers
  can reach each other again — but lighthouse does not reliably rebuild its peer set: it sits at
  the same slot as everyone else, so it never measures itself as behind and never range-syncs.
  A node can end up stranded after a run of scenarios. `make repeer` restarts any client
  with no peers, which rebuilds its discovery table from the bootnode and rejoins it in a slot or
  two. Scenarios report `inconclusive` rather than a finding when the clients have not
  reconverged, so a stranded node never masquerades as a client bug.
- `make up` does not rebuild besu. Run `make besu` after changing the besu checkout.
- Do not pass `--image-download always`; these are local tags with no registry behind them.

## Layout

```
main.star            composes ethereum-package, adds pbtmonitor, pbthammer and pbtchaos
args/devnet.yaml     the whole configuration
monitor/             the differential observer and its oracles
hammer/              the eleven PBT-shaped transaction workloads; txkit/ is shared with chaos
chaos/               forced reorgs: proposer isolation and the six state scenarios
gengenesis/          standalone geth-format PBT genesis, for single-client debugging
scripts/             build, status, verify, diagnose, chaos, repeer
patches/             the besu fix, for anyone building besu by hand
```
