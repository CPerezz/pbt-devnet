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
client type. Only EIP-1559 transactions are sent today.

## Run

```bash
brew install kurtosis-tech/tap/kurtosis-cli          # + Docker running, + yq
scripts/build-images.sh                              # every client in client_sources, + driver + hammer

kurtosis run . --enclave pbt --args-file args/devnet.yaml

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

Gas here is two-dimensional (`StateGas = bytes_of_new_state × 1530`), so a fresh account costs
207,391 and a fresh storage slot 111,234 while a transfer to an *existing* account is still 21,000.
Gas is estimated per transaction on every client; a disagreement between clients is reported as a
finding.
