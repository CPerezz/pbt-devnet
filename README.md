# pbt-devnet

A differential devnet for the **EIP-8297 partitioned binary tree (PBT)**: geth and besu running
the same Amsterdam chain, driven by real lighthouse consensus clients, with every execution client
required to agree on every state root.

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

```bash
make split    # partition participants {1,2} | {3} on both el and cl
make heal     # clear every partition and shaping rule
make chaos    # split, hold, heal, and report whether the branches actually diverged
```

Driven through disruptoor's HTTP API. Note that a partition **applying** is not the same as a
partition **biting**: `make chaos` checks that two clients actually sat at the same height on
different hashes, because that is the only proof the split did anything.

Reorgs are the interesting case for a binary tree. Geth handles them by replacing layers rather
than reversing them — an abandoned branch is dropped from the layer tree, and anything it wrote
went with it — so shared, content-addressed code chunks never need reference counting. It refuses
only a fork at or below the persisted disk layer, and re-executes forward instead. See
`core/pbt_reorg_code_test.go` in the geth branch.

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
merkle-patricia genesis. This is how the first besu bug was isolated, and a single-client enclave
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

The two clients disagree on how PBT is spelled, so the generator emits both keys into one
`genesis.json` and each ignores the other's:

| | genesis key | runtime flag | model |
|---|---|---|---|
| geth | `"pbt": true` | *(none — read from genesis)* | chain property, fixed from genesis |
| besu | `"binaryTrieTime": 0` | `--data-storage-format=BINARY` | fork activation timestamp |

That is a real difference in interpretation, not just spelling: besu's shape implies a chain could
switch to the tree mid-flight, which geth's model forbids. They coincide at genesis, so it does not
affect this devnet.

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

## Known issues

- **The consensus clients do not peer, so the devnet runs as three separate chains.** This is the
  current blocker and it is not yet explained. `/eth/v1/node/peer_count` reports `connected: 0` on
  every beacon node from startup, with no chaos applied, so each lighthouse drives its own
  execution client down its own chain from the shared genesis. `make verify` reports it correctly;
  `make status` will show three different roots.

  What is known: the execution clients agree perfectly for the first ~35 blocks and then diverge
  permanently, which is consistent with peering never being established rather than being lost.
  An earlier note here blamed a partition smoke test — that was wrong, a clean run with zero
  disruptoor events shows the same thing.

  The leading hypothesis is data-availability sampling. `fulu_fork_epoch: 0` puts the chain in
  PeerDAS from genesis, and on a three-node devnet a non-supernode may never obtain or reconstruct
  the data columns it needs. Karim's working setup sets `supernode: true` on every participant and
  uses `PRESET_BASE: mainnet` rather than `preset: minimal`; this one sets neither. Those are the
  two things to try first.

  Until it is resolved, treat cross-client agreement as verified only over the first ~35 blocks,
  and treat `make chaos` as destructive.
- `make up` does not rebuild besu. Run `make besu` after changing the besu checkout.
- Do not pass `--image-download always`; these are local tags with no registry behind them.

## Layout

```
main.star            composes ethereum-package, adds pbtmonitor and pbthammer
args/devnet.yaml     the whole configuration
monitor/             the differential observer and its oracles
hammer/              the eleven PBT-shaped transaction workloads
gengenesis/          standalone geth-format PBT genesis, for single-client debugging
scripts/             build, status, verify, chaos
patches/             the besu fix, for anyone building besu by hand
```
