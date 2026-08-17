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

```
                  ┌──────────────────────────────────────┐
                  │ pbtdriver   (stands in for the CL)   │
                  │  FCUv4(attrs)  ──►  proposer         │  proposer rotates each slot
                  │  getPayloadV6  ◄──  proposer         │
                  │  newPayloadV5  ──►  ALL nodes        │  ◄── the oracle
                  │  FCUv4(head)   ──►  ALL nodes        │
                  └───┬──────────────┬──────────────┬────┘
                ┌─────▼─────┐  ┌─────▼─────┐  ┌─────▼─────┐
                │  geth-a   │  │  geth-b   │  │  besu-a   │   unpeered
                │  pruning  │  │  archive  │  │           │   asymmetric config
                └─────▲─────┘  └─────▲─────┘  └─────▲─────┘
                ┌─────┴──────────────┴──────────────┴─────┐
                │ pbthammer  →  every RPC                  │
                └──────────────────────────────────────────┘
```

Clients and nodes are declared in `args/devnet.yaml`; `main.star` only knows *how* to launch each
client type.

## Run

Needs Docker running, `kurtosis`, and `yq`. `make check` says which of those is missing.

```bash
make up      # build the images, start the devnet, follow the driver
make down    # stop and remove it
```

`make up` first proves the oracle works by corrupting a state root on purpose and requiring every
other node to reject it, then runs the slot loop with reorgs and the full transaction mix. Watch
for lines beginning `FINDING`; there should be none. Ctrl-C detaches without stopping anything.

Bare `make` lists every target.

## Workloads

The hammer sends eleven shapes, round-robin in equal measure, chosen for what EIP-8297 changed
rather than for throughput: fresh accounts, scattered storage, code shared between accounts,
self-destructing children, 7702 delegate/re-delegate/clear, storage zeroization (zero is an
*absence* on this tree, so it deletes), `SLOAD` through a `CALL`, `EXTCODESIZE`/`EXTCODECOPY` over
a large contract, legacy and access-list envelopes, and a transaction that reverts after writing.

They generate traffic; the assertion is the driver's. So they catch a *divergence* between clients,
not an implementation that is uniformly wrong — with one client, agreement is all there is to check.
The revert workload is expected to produce `status=0` receipts; everything else should be `status=1`.

## Clients

geth (`CPerezz/go-ethereum@pbt`) is the only client wired up today. Besu's EIP-8297 work lives in
[`besu-eth/besu-stateless`](https://github.com/besu-eth/besu-stateless/tree/feat/partitioned-binary-trie),
which is a trie library rather than a node — no engine API, CLI or genesis parsing — so it cannot
join yet; `besu-eth/besu` is already Amsterdam-capable but has no PBT. The `besu-a` node above and
the commented entries in `args/devnet.yaml` are the shape it will take. Until a second
implementation runs, all nodes are the same binary, which can be shown consistent but never
correct.

## Adding a client

Two edits. In `args/devnet.yaml`, give it an image, a source and a node entry:

```yaml
default_ethereum_client_images:
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
`CLIENTS` needs only the args file.

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

## Reference

Everything below is optional. Behaviour is tuned in `args/devnet.yaml`; the flags each binary
accepts are in `bin/pbtdriver --help` and `bin/pbthammer --help`.

```bash
make logs                      # re-attach to the driver
make build                     # images only
make genesis                   # regenerate genesis, print the root for the args file
make bin                       # driver + hammer as host binaries in bin/
make up ARGS=args/mine.yaml    # a different config, or ENCLAVE=... for a second devnet
```

```bash
kurtosis port print pbt geth-a rpc
kurtosis service stop pbt geth-a && kurtosis service start pbt geth-a   # chaos
kurtosis enclave dump pbt ./dump
```

Do not pass `--image-download always`; these are local tags with no registry.

Container-free loop, for iterating on the driver without images. Needs a geth binary at
`bin/pbtgeth`; `make dev` prints the command to build one.

```bash
make dev                                             # start nodes, then the driver
scripts/local-devnet.sh hammer --for 120s            # load, in another shell
scripts/local-devnet.sh logs 60                      # node logs
scripts/local-devnet.sh stop
```

Gas here is two-dimensional (`StateGas = bytes_of_new_state × 1530`), so a fresh account costs
207,391 and a fresh storage slot 111,234 while a transfer to an *existing* account is still 21,000.
Gas is estimated per transaction on every client; a disagreement between clients is reported as a
finding. Nothing is ever hardcoded — including the reverting workload, which is priced from an
identical non-reverting twin, because `eth_estimateGas` cannot price a call that always reverts.
