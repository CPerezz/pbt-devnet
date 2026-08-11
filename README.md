# pbt-devnet

N Geth nodes on the **EIP-8297 partitioned binary tree**, post-Amsterdam, driven so that every
block one node builds must be accepted by all the others. Built to make them disagree about a
state root and capture the block that did it.

Geth: [`CPerezz/go-ethereum@pbt`](https://github.com/CPerezz/go-ethereum/tree/pbt)
(upstream PR [#35436](https://github.com/ethereum/go-ethereum/pull/35436)).

## How it works

**No consensus client.** The genesis state root is the binary-tree root, so the genesis block
hash differs from what `eth-beacon-genesis` derives under merkle-patricia rules — a real CL would
embed a hash the EL never produces. Working around that needs a patched two-pass genesis pipeline
(`--shadow-fork-rpc`); that is Phase 2.

What replaces it is a better oracle anyway: `engine_newPayloadV5` makes each importing node
re-execute the block and compare its own computed root against the root the payload commits to.
**A `VALID` from a node that did not build the block is the state-root assertion**, every block,
no polling.

**The nodes are deliberately different** — one pruning with small caches, one archive with large
caches and preimages — because instances of one binary with one config agree by construction and
prove nothing. The proposer rotates, so every node's *builder* output is checked by every other
node's *validator*.

They are **unpeered**; the generator submits every transaction to every RPC instead.

## Commands

Prerequisites: Docker running, and the Kurtosis CLI.

```bash
brew install kurtosis-tech/tap/kurtosis-cli     # macOS
```

Build the three images. `PBT_GETH_SRC` defaults to `../go-ethereum`:

```bash
scripts/build-images.sh                          # pbt-geth, pbt-driver, pbt-hammer
PBT_GETH_SRC=~/src/go-ethereum scripts/build-images.sh
```

Add a `.dockerignore` to the geth checkout first — the build context otherwise includes untracked
build artifacts and any nested worktrees. `build-images.sh` warns if it is missing.

Run in Kurtosis:

```bash
kurtosis run . --enclave pbt --args-file args/phase1.yaml   # load + periodic reorgs
kurtosis run . --enclave pbt --args-file args/quiet.yaml    # baseline: no load, no chaos

kurtosis service logs pbt pbtdriver -f          # the oracle
kurtosis service logs pbt geth-a -f
kurtosis port print pbt geth-a rpc              # -> http://127.0.0.1:<ephemeral>
kurtosis service stop  pbt geth-a               # chaos: outage mid-run
kurtosis service start pbt geth-a
kurtosis enclave dump  pbt ./dump               # everything, for a post-mortem
kurtosis enclave rm -f pbt
```

Do not pass `--image-download always`: these are local tags with no registry behind them.

Faster loop, no containers. `PBT_NODES` defaults to 2:

```bash
scripts/local-devnet.sh start
PBT_NODES=3 scripts/local-devnet.sh start

scripts/local-devnet.sh driver --self-test                          # run this first
scripts/local-devnet.sh driver --slot-time 3s --slots 20 --reorg-every 6 --reorg-depth 2
scripts/local-devnet.sh hammer --interval 400ms --batch 4 --for 120s
scripts/local-devnet.sh logs 60
scripts/local-devnet.sh stop
```

Driver flags: `--el name=engine,rpc` (repeatable, ≥2), `--slot-time`, `--slots` (0 = forever),
`--reorg-every`, `--reorg-depth`, `--probe-every`, `--self-test`, `-v`.
Hammer flags: `--rpc` (repeatable), `--only fanout|storage|codedup|destruct`, `--interval`,
`--batch`, `--slots-per-tx`, `--code-size`, `--for`, `--tip`.

Health checks — these catch the two silent-failure modes, both of which have bitten this setup:

```bash
RPC=$(kurtosis port print pbt geth-a rpc)
# queued must be ~0. A large queued count means transactions are being dropped, not executed.
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"txpool_status","params":[]}' $RPC
# every status must be 0x1. Out-of-gas transactions still occupy blocks and revert every write.
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockReceipts","params":["latest"]}' $RPC
```

Regenerate genesis, and cross-check it against the reference tooling:

```bash
cd gengenesis && go build -o /tmp/gengenesis .
/tmp/gengenesis --out ../genesis/genesis.json --gaslimit 200000000
evm pbt state-root --input.alloc=<alloc.json>     # must equal the chain's genesis state root
```

## Adding an execution client

One list. `el_nodes()` in `main.star` is where clients are declared; the driver's `--el` flags and
the hammer's `--rpc` flags are both generated from it.

```python
def el_nodes(cfg):
    return [
        geth_node(cfg, "geth-a", ["--cache=512",  "--cache.trie=10"]),
        geth_node(cfg, "geth-b", ["--cache=3072", "--cache.trie=40", "--gcmode=archive", "--cache.preimages"]),
        geth_node(cfg, "geth-c", ["--cache=1024", "--cache.trie=25"]),   # appended
    ]
```

A non-geth client does not use the `geth_node` helper — every client has its own CLI — so it
supplies the whole entry:

```python
{
    "name": "myclient",
    "image": "myorg/myclient:pbt",
    "cmd": ["--datadir", "/data", "--genesis", "/network-configs/genesis.json", ...],
    "rpc_port": 8545,
    "engine_port": 8551,
}
```

The genesis files artifact is mounted at `/network-configs` and the JWT at `/jwt/jwtsecret`.

For the container-free loop, pass one `--el name=engineURL,rpcURL` per client (or use
`PBT_NODES=N` for extra geth nodes).

**What the harness requires of any EL:**

- engine API `forkchoiceUpdatedV4`, `getPayloadV6`, `newPayloadV5`. Note **V6** — `getPayloadV5`
  is gated to Osaka+BPO and refuses an Amsterdam payload.
- payload attributes carrying `slotNumber` and `targetGasLimit`, and a non-nil `blockAccessList`
  in the payload (EIP-7928).
- a genesis with `"pbt": true` and `"amsterdamTime": 0`, identical across all clients — the driver
  refuses to start if chain id, genesis hash or head differ.
- for geth-family clients: path state scheme, full sync, and never `--dev` or `--vmwitnessstats`.

At least two clients are required; one node has nobody to disagree with.

**Caveat worth stating plainly:** only this geth fork is known to implement PBT today, so every
node is currently the same implementation. Two copies of one binary can be shown *consistent*,
never *correct* — a second independent implementation is the strongest oracle this setup does not
yet have.

## Oracles

1. **`newPayloadV5` status from every non-proposer** — the primary oracle, free, every block.
2. **`debug_getBadBlocks`** — returns the offending block's RLP, so a finding is a reproducible
   artifact. Only blocks rejected during this run count.
3. **Head agreement** — equal height with different hashes is a fork and a finding, never
   silently resolved.
4. **`eth_getProof`** — the only RPC that reads the tree per key. Targets are sampled from the
   latest block, so proofs cover state the load actually created.
5. **`debug_executionWitness`** on the disputed block — compares what state each node had to
   resolve, headers included.

Run `--self-test` first. It corrupts one byte of a real payload's state root, recomputes a
matching block hash so the node cannot dismiss it on the hash check, and requires every other
node to answer `INVALID`:

```
invalid merkle root (remote: 9903d4… local: 6603d4…)
self-test passed: divergence detected and evidence captured
```

Until that passes, "0 findings" is indistinguishable from a broken harness.

Not usable on PBT: `debug_dumpBlock`, `debug_accountRange` and `debug_storageRangeAt` are refused;
`debug_getModifiedAccountsByNumber` has no PBT guard and will mislead; `debug_stateSize` can never
initialise, since its tracker waits for a snapshot generator the tree never runs. The driver uses
the `dumpBlock` refusal as a positive "am I on the tree?" probe.

## Coverage

| workload | what it stresses | status |
|---|---|---|
| `fanout` | fresh accounts → new header stems | ✅ |
| `storage` | many spread slots → storage zone, 66-byte keys | ✅ |
| `codedup` | identical large code from several senders → **shared** code-zone leaves | ✅ |
| `destruct` | factory deploys a child with real code, then destroys it in-tx → code-zone leaves nothing reclaims | ✅ |
| 7702 delegation lifecycle | `DelegationLeafKey`, one-of-two-leaves invariant | ❌ |
| storage zeroization | a zero write must *delete* the leaf | ❌ |
| call + SLOAD, EXTCODESIZE/COPY | the tree read path | ❌ |
| legacy (type 0), access list (type 1) | envelope variety | ❌ |

**Only EIP-1559 (type 2) transactions are sent today.** Blobs (type 3) are out of scope: blob
data never enters the state tree.

A wrong *tree* representation is invisible to execution — code bytes come from the kv code table
keyed by keccak, not from the tree — so assertions must go through `eth_getProof` and root
agreement, never `eth_getCode` alone.

## Gas is two-dimensional

`core/vm/gascosts.go` splits every charge into `ExecutionGas` and `StateGas`, where state gas is
`bytes_of_new_state × CostPerStateByte`; on this branch `CostPerStateByte = 1530` and
`AccountCreationSize = 120`. Only operations that *grow* state pay it. Measured marginally:

| operation | gas |
|---|---|
| transfer to an **existing** account | 21,000 (unchanged) |
| transfer to a **fresh** account | 207,391 (+186,391; 120 × 1530 = 183,600 is state gas) |
| one fresh storage slot | 111,234 |
| one byte of deployed code | ~1,547 |

The generator estimates gas per transaction and adds only 1%; measured `gasUsed/gasLimit` is
98.21%. A hardcoded pre-Amsterdam limit sends everything out-of-gas — still included, still
burning the whole limit, still reverting every write. **Check receipt `status`, not throughput.**
Genesis uses a 200M gas limit; blocks run ~30–38 txs and up to ~198M gas.

## Genesis

Generated, not hand-written, so system-contract bytecode is exact and balances are checked against
the tree's 16-byte field. Needs `"pbt": true` **and** `"amsterdamTime": 0` (PBT errors without
Amsterdam scheduled), no `bogotaTime`, `blobSchedule` for cancun/prague/bpo1/bpo2, and the four
system contracts predeployed with `nonce: 1`.

## Known gaps

- The coverage rows marked ❌ above.
- No unit tests; verification is empirical (self-test, receipts, `evm pbt state-root`).
- Two findings for the geth branch, recorded but not fixed here: `delStem` silently no-ops on a
  partial stem while `insStem`/`getStem` return `ErrPartialStem`; and `state_sizer.go` waits on a
  snapshot generator PBT never runs, which is why `debug_stateSize` is unusable.
