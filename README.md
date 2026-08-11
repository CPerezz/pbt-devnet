# pbt-devnet

N Geth nodes on the **EIP-8297 partitioned binary tree**, post-Amsterdam, driven so every block one
node builds must be accepted by all the others — to make them disagree about a state root and catch
the block that did it.

Geth: [`CPerezz/go-ethereum@pbt`](https://github.com/CPerezz/go-ethereum/tree/pbt) (upstream PR
[#35436](https://github.com/ethereum/go-ethereum/pull/35436)). There is no consensus client: the
genesis state root is the tree root, so the genesis block hash differs from what
`eth-beacon-genesis` derives under merkle-patricia rules. `engine_newPayloadV5` makes each importing
node recompute the root and compare it to the payload's, so a `VALID` from a node that did not build
the block *is* the state-root assertion. Rationale for the rest is in
[PR #1](https://github.com/CPerezz/pbt-devnet/pull/1).

## Run

```bash
brew install kurtosis-tech/tap/kurtosis-cli          # + Docker running
scripts/build-images.sh                              # PBT_GETH_SRC=../go-ethereum by default

kurtosis run . --enclave pbt --args-file args/phase1.yaml   # load + reorgs
kurtosis run . --enclave pbt --args-file args/quiet.yaml    # baseline, no load

kurtosis service logs pbt pbtdriver -f               # the oracle
kurtosis port print  pbt geth-a rpc
kurtosis service stop pbt geth-a && kurtosis service start pbt geth-a   # chaos
kurtosis enclave dump pbt ./dump                     # post-mortem
kurtosis enclave rm -f pbt
```

Do not pass `--image-download always`; these are local tags with no registry.

Container-free loop (`PBT_NODES` defaults to 2):

```bash
scripts/local-devnet.sh start
scripts/local-devnet.sh driver --self-test           # run this first, always
scripts/local-devnet.sh driver --slot-time 3s --slots 20 --reorg-every 6
scripts/local-devnet.sh hammer --interval 400ms --batch 4 --for 120s
scripts/local-devnet.sh logs 60
scripts/local-devnet.sh stop
```

Driver: `--el name=engine,rpc` (repeatable, ≥2), `--slot-time`, `--slots`, `--reorg-every`,
`--reorg-depth`, `--probe-every`, `--self-test`, `-v`.
Hammer: `--rpc` (repeatable), `--only fanout|storage|codedup|destruct`, `--interval`, `--batch`,
`--slots-per-tx`, `--code-size`, `--for`.

Regenerate genesis and cross-check against the reference:

```bash
cd gengenesis && go build -o /tmp/gengenesis . && /tmp/gengenesis --out ../genesis/genesis.json --gaslimit 200000000
evm pbt state-root --input.alloc=<alloc.json>        # must equal the chain's genesis state root
```

## Adding an execution client

`el_nodes()` in `main.star` is the only place clients are declared; the driver's `--el` flags and the
hammer's `--rpc` flags are generated from it.

```python
def el_nodes(cfg):
    return [
        geth_node(cfg, "geth-a", ["--cache=512",  "--cache.trie=10"]),
        geth_node(cfg, "geth-b", ["--cache=3072", "--cache.trie=40", "--gcmode=archive", "--cache.preimages"]),
        geth_node(cfg, "geth-c", ["--cache=1024", "--cache.trie=25"]),        # appended
    ]
```

A non-geth client skips the helper and supplies its own entry — every client has its own CLI:

```python
{"name": "myclient", "image": "myorg/myclient:pbt", "rpc_port": 8545, "engine_port": 8551,
 "cmd": ["--genesis", "/network-configs/genesis.json", ...]}
```

Genesis is mounted at `/network-configs`, the JWT at `/jwt/jwtsecret`. For the local loop, pass one
`--el name=engineURL,rpcURL` per client. At least two are required — one node has nobody to disagree
with.

Requirements on any EL:

- engine `forkchoiceUpdatedV4`, **`getPayloadV6`**, `newPayloadV5` (`getPayloadV5` is gated to
  Osaka+BPO and refuses an Amsterdam payload)
- payload attributes with `slotNumber` and `targetGasLimit`; non-nil `blockAccessList` (EIP-7928)
- identical genesis with `"pbt": true` and `"amsterdamTime": 0` — the driver refuses to start if
  chain id, genesis hash or head differ
- geth-family: path state scheme, full sync, never `--dev` or `--vmwitnessstats`

Only this geth fork is known to implement PBT, so all nodes are the same implementation today. Two
copies of one binary can be shown consistent, never correct.

## Gotchas

- **Run `--self-test` before trusting any result.** It corrupts one byte of a real payload's state
  root and recomputes a matching block hash, forcing the node through the root comparison. Until it
  prints `invalid merkle root`, "0 findings" is indistinguishable from a broken harness.
- **Gas is two-dimensional**: `StateGas = bytes_of_new_state × 1530`. A fresh account costs 207,391
  and a fresh storage slot 111,234, while a transfer to an *existing* account is still 21,000. Gas is
  estimated per transaction, never hardcoded — a pre-Amsterdam limit puts out-of-gas transactions in
  every block, burning the full limit and reverting every write. **Check receipt `status`.**
- **Watch `txpool_status`**: a non-zero `queued` means transactions are being dropped, not executed.
- Unusable on PBT: `debug_dumpBlock`, `debug_accountRange`, `debug_storageRangeAt` (refused),
  `debug_getModifiedAccountsByNumber` (no PBT guard, misleads), `debug_stateSize` (never
  initialises). `eth_getProof` is the only per-key read of the tree.
- A wrong *tree* representation is invisible to execution, since code comes from the kv code table
  keyed by keccak. Assert via `eth_getProof` and root agreement, never `eth_getCode` alone.

## Coverage

`fanout` (fresh accounts) · `storage` (spread slots) · `codedup` (shared code-zone leaves) ·
`destruct` (factory deploys code then destroys it in-tx).

Missing: 7702 delegation lifecycle, storage zeroization, call/SLOAD, EXTCODESIZE/COPY, legacy and
access-list transactions. **Only EIP-1559 is sent today.** Blobs are out of scope — blob data never
enters the state tree. No unit tests; verification is empirical.

Two findings for the geth branch, recorded not fixed: `delStem` silently no-ops on a partial stem
while `insStem`/`getStem` return `ErrPartialStem`; `state_sizer.go` waits on a snapshot generator PBT
never runs, which is why `debug_stateSize` is unusable.
