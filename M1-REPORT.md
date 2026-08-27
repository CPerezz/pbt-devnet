# M1 - Empty-State MPT->PBT Migration Devnet

Milestone 1 of the EIP-8347 migration devnet: a kurtosis network that STARTS
on an empty merkle-patricia state (system contracts + prefunded accounts
only), runs geth nodes with the partitioned binary tree building in the
background, survives spamoor load and forced reorgs for hundreds of blocks,
crosses `binaryTrieTime`, and finishes the migration on every node - judged
by `verify-migration` C1-C8, exit 0.

Adjudicated plan: session artifact `m1-final-plan.md` (2026-08-27, adversarial
debate, 1 proposer + 1 critic, ultrathink all). Every claim below is backed by
a command run in the session; the acceptance run's monitor/chaos JSONL and
verifier output are archived as session artifacts under `r3-acceptance/`.

## The ladder (all stages command-verified)

| stage | shape | result |
|---|---|---|
| S1 docker matrix | canonical `pbt-geth:local` shim: armed/passthrough/restart/failure | PASS; live wire fixtures captured (PascalCase, number cursor); tip REQUIRES `--cache.preimages` for convert |
| B1 egg harness | offset 1800 / unset / 0, six gates | PASS; complements cross-identical both directions; 65539 embeddings of the right hash, 0 of the wrong; unset == offset-0 byte-identical |
| T8v regression gate | 7-client PBT-at-genesis devnet, shimmed image | PASS; 50/50 blocks identical - the legacy devnet is unbroken |
| S2 (1 node, offset 600) | first live migration | bstar@99; Binary parked@98, Merkle takeover synced-at-head; done at T+11m; 0 criticals |
| S3 (4 nodes, offset 7200, one 3-min isolation) | chaos machinery | divergence visible mid-partition with RPC reachable (A13); heal reorg drop=8/add=9; converged |
| R2 (4 nodes, offset 1800, no chaos) | volume run | bstar x4 at block 298 within 11ms; done x4 at T+521s; 0 criticals; 298 canonical pre-fork blocks |
| R3 (4 nodes, offset 2400, chaos r3) | ACCEPTANCE | 3 laps (see findings); final: bstar x4 @345, done x4 at T+519s, 0 criticals, **C1-C8 all PASS, exit 0** |

Final R3 verifier output:

```
PASS C1: b*=345 (monitor bstar events agree with rpc backwalk)
PASS C2: s0=3, every 25-block bucket in [3,344] has >=1 tx block
PASS C3: 5/5 healed isolations matched, >=1 at depth >= 10, all healed before T-300
PASS C4: 43 block(s) in [313,355] agree across 4 node(s)
PASS C5: no migration-window mentions; all node timelines strictly ordered
PASS C6: 95 samples, 91 good, 56 outside chaos windows, 0 unwaived criticals
PASS C7: genesis stateRoot match pins across 4 node(s)
PASS C8: snapshot=0ea2e8... preimages=5bbc80... identical across 4 node(s)
```

## What R3 taught us (the findings that changed the design)

1. **Deep partitions do not self-heal on this stack.** Laps 1 (12-min) and
   2 (6-min windows) stranded the victim identically: once its branch
   diverges across a checkpoint the majority finalized, the MAJORITY side's
   lighthouse peer scoring bans the victim, remote bans survive a victim
   restart (`make repeer` proven insufficient live), and the node crosses
   the fork boundary alone on its own island - which the monitor's bstar
   quorum check caught as the run's only critical (F3, bstar 143 vs 263).
2. **Stake is the knob, not duration.** The shipped r3 profile gives the
   deep victim 256 of 640 validators (40%): isolating it pins the connected
   majority at 60%, BELOW the 2/3 finality threshold, so finality stalls
   for the window instead of banning anyone - and a 40% proposer share
   yields depth >= 10 inside a 190s window (~99.2% across three tries), the
   3-minute scale S3 proved heals. Lap 3: two `drop=11` reorgs on the heavy
   victim, drop=4 and drop=8 on the light shorts, all healed, all converged.
3. **Genesis hash is per-run by design** (kurtosis stamps the timestamp at
   render); the run-stable pin is the alloc's STATE ROOT, byte-identical
   across S2/R2/R3. `verify-migration` asserts `genesis_hash` only if the
   pins file carries it.
4. **The verifier must speak both naming conventions** - chaos says
   `node-2`, kurtosis and the monitor say `el-2-geth-lighthouse` - and must
   defer sample-mismatch judgment to the monitor's hash-grouped F1 (a
   split-shaped sample during partitions is legal, and geth's own
   `Chain reorg detected ... drop=N` line is depth evidence).
5. **1/min sampling starves C6** (~37 all-non-null samples in a 30-minute
   offset); 30s cadence delivers 91/95.

## Deviations from the adjudicated plan text

- r3 chaos windows: 12-min deeps -> 190s stake-weighted deeps + two
  sequential single-victim shorts (evidence above; the plan's own A8/H named
  deep-first + contingency, and the loop budget's one R3 relap became two
  laps + a verifier re-judge of the same lap-3 evidence).
- The "two 2.5-min" paired island was dropped: disruptoor partition state is
  global (pbtchaos serialises for the same reason) and a 2v2 island is an
  LMD tie whose heal direction is a coin flip.
- C7 pins carry only the state root (finding 3).
- ChaosDev subagent stalled twice; `cmd/migration-chaos` was written by the
  session lead to that agent's own approved plan.

## Assumption register (highest-risk resolutions)

- A8 standalone-egg alloc == kurtosis alloc: FALSE as the register predicted;
  pins captured from the first kurtosis-rendered genesis instead.
- A10 wire encoding: plain numbers + PascalCase, live-captured; FlexUint64
  keeps hexutil tolerance.
- A11 offset passthrough: TRUE (binaryTrieTime - genesis == offset exactly,
  every run).
- A13 partition-RPC reachability: TRUE (S3 mid-window reads from the victim).
- A15 ToBlock positional for both tree kinds: TRUE (B1 gates 2-4).
- A17 artifact addressability: TRUE (`el_cl_genesis_data` StoreSpec at the
  ethereum-package pin; `run_sh` + jq reads T back from the generated
  genesis, never recomputing it).

## Pins (PINS.env, frozen at T0)

- geth `pbt` tip: 7ddb0d4b027554406c6a8b85d7240719771340dd (the #31 merge)
- ethereum-package: 5ec41d44ae23fb01b036c11b25d98547cc9c3be4
- eth-beacon-genesis: 282a2b43aacb4976ec91974d49a68b4a9c8ccb5d (go.mod
  follows the merge; CL genesis embeds the MERKLE hash when the offset is
  nonzero - proven by B1's occurrence counts)

## Unsigned-commit ledger (A12: one 90s -S attempt per batch, then ledger)

egg `pbt-offset`: 3e2bc22 b56f205 - pbt-devnet `migration-m1`: ac12b23
8fce66e 4412b25 f749189 (in-branch, listed in PR #11) - pbt-devnet
`migration-m1-tooling`: c0f0172 d04d6a3 ff5007b 5298122 3879d7f 9840c8b
d8998b1 7178393 e839887 and this report's commit. Owner signs on merge.

## How to run it

```
make build                                   # images incl. the migration shim + tooling
kurtosis run . --enclave pbt --args-file args/migration.yaml --privileged
# watch: kurtosis service logs pbt migration-monitor -f
# judge: make verify-migration ENCLAVE=pbt LOGS_DIR=<dump> BINARY_TRIE_TIME=<from plan output>
```

Profiles: `migration-smoke.yaml` (1 node, offset 600, no chaos),
`migration-chaos-smoke.yaml` (4 nodes, one 3-min isolation),
`migration-r2.yaml` (4 nodes, quiet), `migration.yaml` (4 nodes, chaos r3).

## Post-M1 candidates (recorded, not started)

- besu/erigon migration participation (their images already build; the
  args files are geth-only on purpose).
- Re-anchoring at height H > 0 (`--cache.preimages` groundwork is in the
  args; the shim's conversion is genesis-anchored today).
- Boundary-straddling reorgs (post-b* chaos windows) - needs owner sign-off
  per A11.
- Fork-side anchor-on-convert + combined debug fields (A5 exclusions).
- In-enclave repeer hook if future profiles ever reintroduce
  finality-crossing partitions.
