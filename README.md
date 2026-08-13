# pbt-devnet

N execution clients on the **EIP-8297 partitioned binary tree**, post-Amsterdam, driven so every
block one node builds must be accepted by all the others — to make them disagree about a state
root and catch the block that did it.

There is no consensus client. The genesis state root is the tree root, so the genesis block hash
differs from what `eth-beacon-genesis` derives under merkle-patricia rules. What replaces it is a
better oracle: `engine_newPayloadV5` makes each importing node recompute the root and compare it
to the payload's, so a `VALID` from a node that did not build the block *is* the state-root
assertion. Rationale for the rest is in
[PR #1](https://github.com/CPerezz/pbt-devnet/pull/1).

Clients are declared in the args file; `main.star` only knows *how* to launch each client type.

## Run

```bash
brew install kurtosis-tech/tap/kurtosis-cli          # + Docker running, + yq
scripts/build-images.sh                              # builds every client in client_sources, + driver + hammer

kurtosis run . --enclave pbt --args-file args/phase1.yaml       # load + reorgs
kurtosis run . --enclave pbt --args-file args/quiet.yaml        # baseline, no load
kurtosis run . --enclave pbt --args-file args/three-nodes.yaml  # 3 nodes, config-only

kurtosis service logs pbt pbtdriver -f               # the oracle
kurtosis port print  pbt geth-a rpc
kurtosis service stop pbt geth-a && kurtosis service start pbt geth-a   # chaos
kurtosis enclave dump pbt ./dump
kurtosis enclave rm -f pbt
```

Do not pass `--image-download always`; these are local tags with no registry.

Container-free loop (geth-only, `PBT_NODES` defaults to 2):

```bash
scripts/local-devnet.sh start
scripts/local-devnet.sh driver --self-test           # run this first, always
scripts/local-devnet.sh driver --slot-time 3s --slots 20 --reorg-every 6
scripts/local-devnet.sh hammer --interval 400ms --batch 4 --for 120s
scripts/local-devnet.sh logs 60
scripts/local-devnet.sh stop
```

Driver: `--el name=engine,rpc` (repeatable, ≥2), `--expected-genesis-root`, `--slot-time`,
`--slots`, `--reorg-every`, `--reorg-depth`, `--probe-every`, `--self-test`, `-v`.
Hammer: `--rpc` (repeatable), `--only fanout|storage|codedup|destruct`, `--interval`, `--batch`,
`--slots-per-tx`, `--code-size`, `--for`.

Regenerate genesis. It also prints the root to paste into `expected_genesis_root`:

```bash
cd gengenesis && go build -o /tmp/gengenesis . && /tmp/gengenesis --out ../genesis/genesis.json --gaslimit 200000000
```

## Adding a client

Two edits. In the args file, give it an image, a source and a node entry:

```yaml
default_ethereum_client_images:
  geth: pbt-geth:local
  myclient: pbt-myclient:local

client_sources:
  myclient: {repository: myorg/myclient, ref: pbt, path: ../myclient}

nodes:
  - {name: myclient-a, client: myclient}
```

In `main.star`, add one `CLIENTS` entry with a flag builder, ports and genesis filename — flags
are logic, not config, so they live in code:

```python
CLIENTS = {
    CLIENT_TYPE.myclient: struct(
        flags=_myclient_flags,          # (ports, genesis_path) -> [str]
        genesis_file="genesis.json",
        ports=struct(rpc=8545, engine=8551, p2p=30303),
    ),
}
```

Genesis is mounted at `/network-configs`, the JWT at `/jwt/jwtsecret`. An unknown client fails with
the supported list, and at least two nodes are required. Another instance of a client already in
`CLIENTS` needs only the args file — see `args/three-nodes.yaml`.

**Required of any client:**

- engine `forkchoiceUpdatedV4`, **`getPayloadV6`**, `newPayloadV5`, JWT-authed. V6 matters:
  `getPayloadV5` is gated to Osaka+BPO and refuses an Amsterdam payload.
- payload attributes carrying `slotNumber` and `targetGasLimit`; non-nil `blockAccessList`
  (EIP-7928).
- loads the shared genesis with `"pbt": true` / `"amsterdamTime": 0` and **computes the same
  genesis state root as every other node**. The driver refuses to start otherwise.
- `eth_chainId`, `eth_getBlockByNumber`, `eth_getBlockByHash`.

**Optional** — absent gives one `ORACLE DEGRADED` warning and the run continues: `eth_getProof`,
`eth_getBlockReceipts`, `debug_getBadBlocks`, `debug_executionWitness`.

## Gotchas

- **Run `--self-test` before trusting any result.** It corrupts one byte of a real payload's state
  root and recomputes a matching block hash, forcing the node through the root comparison. Until
  it prints `invalid merkle root`, "0 findings" is indistinguishable from a broken harness.
- **Set `expected_genesis_root`.** Without it the nodes are only checked against each other, not
  against the binary tree — they could all be on the merkle-patricia trie and agree.
- **Gas is two-dimensional**: `StateGas = bytes_of_new_state × 1530`, so a fresh account costs
  207,391 and a fresh storage slot 111,234 while a transfer to an *existing* account is still
  21,000. Gas is estimated per transaction on every client; a hardcoded pre-Amsterdam limit fills
  blocks with out-of-gas transactions that burn the full limit and revert every write.
  **Check receipt `status`, not throughput.** A gas disagreement between clients is a finding.
- **Watch `txpool_status`**: non-zero `queued` means transactions are being dropped, not executed.
- Only `eth_getProof` reads the tree per key. `debug_dumpBlock`, `debug_accountRange` and
  `debug_storageRangeAt` are refused on PBT, `debug_getModifiedAccountsByNumber` has no PBT guard
  and misleads, `debug_stateSize` never initialises. And a wrong *tree* representation is invisible
  to execution — code comes from the kv code table keyed by keccak — so assert via `eth_getProof`
  and root agreement, never `eth_getCode` alone.
- geth: `--state.scheme=path` (hashdb refused), `--syncmode=full` (snap sync refused), never
  `--dev` or `--vmwitnessstats`.

## Coverage

`fanout` (fresh accounts) · `storage` (spread slots) · `codedup` (shared code-zone leaves) ·
`destruct` (factory deploys code then destroys it in-tx).

Missing: 7702 delegation lifecycle, storage zeroization, call/SLOAD, EXTCODESIZE/COPY, legacy and
access-list transactions. **Only EIP-1559 is sent today.** Blobs are out of scope — blob data never
enters the state tree. No unit tests; verification is empirical.
