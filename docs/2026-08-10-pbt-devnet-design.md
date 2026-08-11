# pbt-devnet — design decisions

A record of the decisions and the corrections, so neither has to be rediscovered. Operating
detail lives in `README.md`.

## Goal

Two Geth nodes on the EIP-8297 partitioned binary tree, post-Amsterdam, arranged so a
state-root disagreement is caught and the block that caused it is captured. The implementation
had never run as a live network before this.

## Decisions

**No consensus client.** `pbt: true` changes the genesis state root, hence the genesis block
hash, but `eth-beacon-genesis` derives that hash from `genesis.json` under merkle-patricia
rules — so a real CL embeds a hash the EL will never produce. The verified escape hatch is
`--shadow-fork-rpc` against a throwaway PBT geth, which needs a patched two-pass generator;
deferred to Phase 2. Dropping the CL also drops Gloas/ePBS client-support risk and hands the
driver control of timestamps, competing payloads and restart timing.

**The oracle is `newPayloadV5`.** The importing node re-executes and compares its own computed
root against the payload's, so a `VALID` from the node that did not build the block *is* the
assertion — every block, no polling.

**Asymmetric nodes.** One pruning with small caches, one archive with large caches and
preimages, alternating proposer. Two instances of one binary with one config agree by
construction; alternating forces each node's builder output through the other's validator.

**Unpeered, with the generator broadcasting to both.** Keeps the driver the only source of
canonical blocks. Post-merge blocks do not travel over devp2p anyway.

**Generated genesis.** `gengenesis/` builds it against the geth checkout so system-contract
bytecode is exact and balances are checked against the tree's 16-byte field.

**Engine triple is FCUv4 → getPayloadV6 → newPayloadV5.** `getPayloadV5` is gated to Osaka+BPO
and refuses an Amsterdam payload. The branch's own test calls the internal helper and so
bypasses the version gate — V6 over the wire was unproven until this devnet ran it.

**Blobs out of scope.** Blob data never enters the state tree; only the sender's nonce and
balance change. Highest plumbing cost, no tree coverage.

**spamoor and tx-fuzz rejected.** Neither estimates gas (spamoor's `eoatx` hardcodes
`GasLimit: 21000`) and neither reads `receipt.Status`, so on this chain both would report full
throughput while every transaction ran out of gas.

## Corrections made along the way

Each of these was believed, then measured and found wrong.

**"Amsterdam repriced transfers."** It did not. A transfer to an *existing* account still costs
exactly 21,000. Gas here is two-dimensional (`core/vm/gascosts.go`): state gas is
`bytes_of_new_state × CostPerStateByte`, with `CostPerStateByte = 1530` and
`AccountCreationSize = 120`. Only state *growth* is repriced. An earlier note also gave ~121k
per storage slot by dividing a total by a count; the marginal figure is 111,234.

**Hardcoded gas limits looked like load.** With pre-Amsterdam limits every transaction ran out
of gas — still included, still burning its whole limit, still reverting every state write. Fixed
by estimating per transaction with a 1% margin; measured `gasUsed/gasLimit` is 98.21%, so the
estimate is accurate and no slack is needed.

**"The reorg gap causes silent divergence."** The guard landed: `Recoverable()` returns false
for PBT, so rollback is refused and reorgs re-execute forward. The target is an under-tested
fallback path, not silent corruption.

**Two thirds of the load never executed.** The generator alternated each transaction between two
unpeered nodes, so neither held a gapless nonce sequence; gapped transactions sat in the queued
subpool, capped at 64 per account, and the surplus was dropped with no error returned. 384
queued per node = 6 senders × `AccountQueue`. Fixed by broadcasting to both nodes, plus a
nonce-drift check. Blocks went from 12 txs / 63M gas to 35–38 txs / ~190M gas.

**The harness could swallow a divergence.** Transport errors were classified by matching
substrings against a message that embeds geth's own `validationError`, so a genuine `INVALID`
mentioning "EOF" — the shape of an RLP or block-access-list decode divergence — was dismissed as
an outage. Now classified structurally, by error type at the point of failure. A node that hangs
is deliberately a finding, not an outage.

**An equal-height fork was not a finding.** `resync` adopted node A whenever the heights were
not strictly ordered, so the textbook divergence was logged as a warning.

**`destruct` deployed no code.** `PUSH20; SELFDESTRUCT` halts before any `RETURN`, so the
workload documented as producing orphaned code-zone leaves produced none: 215,814 gas against
210,588 for a bare empty CREATE. Rebuilt as a factory that deploys a child *with* code and then
destroys it in the same transaction — 3,576,558 gas, confirming ~2048 bytes were written.

**The deployed contracts were not callable.** `patternCode` filled the blob with `PUSH0`, which
overflows the 1024-slot stack after ~1024 bytes; `eth_call` returned `stack limit reached 1024`.
The terminator has to come first. Fixed, which unblocks every read-path workload.

**`debug_stateSize` can never work here.** Its tracker waits for snapshot generation; the binary
tree has no generator and cannot have one. The oracle was deleted rather than left to skip
itself silently.

**`debug_executionWitness` decoding was wrong all along** — headers arrive as objects, not hex
strings. It went unnoticed because the oracle only ran on a finding, and there had never been
one. Found by adding `--self-test`, which is the point of having one.

## For the geth branch

- `delStem` silently no-ops on a partial stem while `insStem`/`getStem` return `ErrPartialStem`.
  Harmless while partial stems only come from multiproofs; a wrong-root-reported-as-success once
  the multiproof witness lands.
- `state_sizer.go` waits on a snapshot generator PBT never runs. Treating a nil generator as
  already complete would restore the only cross-node comparison of state content the tree
  supports.
- Observation, not a bug: a second deployment of identical code writes no new code-zone leaves
  but still pays the full 1530/byte state charge. Intentional, or a mismatch worth a spec note?

## Open

7702 delegation, storage zeroization, call/SLOAD, EXTCODESIZE, legacy and access-list
transactions are all still uncovered — see the coverage table in `README.md`. No unit tests;
verification is empirical.
