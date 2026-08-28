"""
A mixed-client PBT (EIP-8297) devnet, driven by real consensus clients.

This composes ethpandaops/ethereum-package rather than launching clients itself: that
package already knows how to run geth, besu and lighthouse together, wire the engine API,
generate genesis and hand out validator keys. What it does not know is the binary tree, and
that gap is closed with one fork and one load-bearing config value, with no patch to the package:

  * pbt-egg:local  — a fork of ethereum-genesis-generator that emits the tree keys AND
    bundles a fork of eth-beacon-genesis whose go.mod replaces go-ethereum with the
    EIP-8297 branch. Without it the consensus genesis embeds a merkle-patricia block hash
    the execution layer will never produce, and the chain never starts. Reached through the
    supported `ethereum_genesis_generator_params.image` hook.

On top of the network this adds three services of our own:

  pbthammer  transaction load shaped at what the tree changed, not at throughput
  pbtmonitor watches every execution client for state-root divergence, and proves its own
             oracle by feeding a corrupted payload through the engine API. It observes
             only: the consensus clients drive the chain, and a second thing driving
             forkchoice manufactures the very forks it would then report.
  pbtchaos   forces reorgs on purpose. It isolates whichever node proposes next, rotating
             so both client types get reorged, and runs scenarios that strand specific
             state on a branch that is then abandoned. It owns disruptoor exclusively, so
             two disruptions never overlap.

In migration mode (pbt_migration.enabled) the chain instead STARTS on the merkle-patricia
trie and forks to the binary tree at T = genesis + fork_offset_seconds. Two services of our
own take over there:

  migration-monitor follows every client's debug_migrationProgress and shadow roots,
             emitting JSONL findings on stdout.
  migration-chaos   drives disruptoor on an admission-gated schedule around the fork
             boundary (profiles: smoke, full). The legacy pbtchaos is skipped in migration
             mode — its reorg cadence knows nothing about the boundary.

All three are independently switchable, so `kurtosis run` with load and chaos disabled is a
quiet baseline.

Run `make up`.
"""

ethereum_package = import_module("github.com/ethpandaops/ethereum-package/main.star@5ec41d44ae23fb01b036c11b25d98547cc9c3be4")

# Our own args keys. ethereum-package sanity-checks its input and fails on anything it does
# not recognise, so these are removed before its args are handed over.
OURS = [
    "pbt_hammer",
    "pbt_monitor",
    "pbt_chaos",
    "pbt_migration",
]

# EIP-8347 migration mode: the chain starts on the merkle-patricia trie and
# schedules the binary tree fork_offset_seconds after genesis. The offset
# lives in exactly one place — this block (or its override in the args
# file) — and is injected into the genesis generator's environment below;
# T resolves at runtime to block 0's timestamp + offset, and is read BACK
# out of the generated genesis.json rather than recomputed, so the tooling
# sees exactly the value the clients see.
DEFAULT_MIGRATION = {
    "enabled": False,
    "fork_offset_seconds": 1800,
    # What drives disruption around the boundary. "none" launches no chaos at
    # all (quiet acceptance runs); "smoke" and "full" are migration-chaos's own
    # admission-gated profiles.
    "chaos_profile": "none",
    "monitor_image": "pbt-migration-monitor:local",
    "chaos_image": "pbt-migration-chaos:local",
    # Same convention as pbt_chaos.protect_nodes: participant 1 is the sole
    # bootnode and must never be disrupted. 1-based.
    "protect_nodes": [1],
}

DEFAULT_HAMMER = {
    "enabled": True,
    "image": "pbt-hammer:local",
    # Blocks must stay under half the gas limit; above it the base fee compounds with
    # nothing to stop it. See the comment on pbt_hammer in args/devnet.yaml.
    "interval": "1s",
    "batch": 2,
    "slots_per_tx": 20,
    "code_size": 6000,
    # How many of the package's prefunded accounts to send from. They come with private
    # keys, so the hammer needs no premine of its own.
    "senders": 4,
    "only": "",
}

# pbtchaos serialises its own disruptions through one queue, so two of ITS jobs never
# overlap. `make split` / `make heal` write to disruptoor directly and are deliberately
# outside that queue -- which also means pbtchaos can clear a hand-applied split when its
# next job finishes.
DEFAULT_CHAOS = {
    "enabled": True,
    "image": "pbt-chaos:local",
    # Periodic one-block reorgs, produced by isolating whichever node proposes next: it
    # builds a block nobody else receives, then has to unwind it. The doomed node rotates,
    # so reorgs land on both client types.
    "isolation": True,
    "isolate_min_blocks": 15,
    "isolate_max_blocks": 30,
    # Empty means two slots: the isolation starts once the chain reaches the slot BEFORE
    # the duty, so it has to span the rest of that slot and the whole proposal slot.
    "isolate_for": "",
    # Default depth for `make scenario` when none is given.
    "depth": 10,
    # Nodes pbtchaos must never disrupt, 1-based.
    #
    # Participant 1 is ethereum-package's sole consensus bootnode and is launched without boot
    # nodes of its own, so a partition leaves it with no way back: zero peers for the rest of
    # the run. args/devnet.yaml gives that role to a dedicated node so the four under test
    # stay eligible. Set to [] if participant 1 is a node you actually want disrupted.
    "protect_nodes": [1],
    # A pair of keys per scenario run, walked so runs do not repeat a pair: a reorged-out
    # transaction stays valid and re-enters the pool, and a reused key then reads a nonce
    # that goes stale underneath it. Three is the most that fits below the hammer without
    # reaching the accounts other services claim -- see the guard below -- so consecutive
    # runs do still share one key.
    "senders": 3,
}

DEFAULT_MONITOR = {
    "enabled": True,
    "image": "pbt-monitor:local",
    "verify_oracle": True,
    "poll": "2s",
    "probe_every": 8,
    # Optional, and the only POSITIVE proof the clients are on the binary tree: clients
    # agreeing with each other says nothing if they all built a merkle-patricia genesis.
    #
    # It has to be the root of the genesis THIS devnet runs, which pbt-egg generates. NOT
    # `make genesis`'s -- that tool allocs a different set of accounts, and a state root
    # commits to the alloc, so its value fails preflight. Read the real one from a run whose
    # clients already agree: eth_getBlockByNumber(0).stateRoot on any node.
    "expected_genesis_root": "",
}

# Prefunded accounts other services claim upstream, and will not negotiate over.
SPAMOOR_ACCOUNT = 13
ASSERTOOR_ACCOUNT = 9

# disruptoor's own listen port, from ethereum-package's launcher. pbtchaos speaks the
# native API on it rather than the friendlier start-up config.
DISRUPTOOR_SERVICE = "disruptoor"
DISRUPTOOR_PORT = 7700
CHAOS_API_PORT = 7800

# ethereum-package uploads the engine API secret under this fixed artifact name, so the
# monitor can mount the same one the clients use and speak the engine API itself.
JWT_ARTIFACT = "jwt_file"
JWT_MOUNT_DIR = "/jwt"
JWT_PATH = JWT_MOUNT_DIR + "/jwtsecret"

# ethereum-package's genesis generator stores the generated network configs —
# genesis.json included — under this fixed artifact name (StoreSpec in
# el_cl_genesis_generator.star at the pinned revision), so migration tooling can
# mount the exact genesis the clients booted from.
EL_CL_GENESIS_ARTIFACT = "el_cl_genesis_data"
EL_CL_GENESIS_MOUNT = "/network-configs"


def _merge(defaults, overrides):
    out = dict(defaults)
    for k in overrides:
        out[k] = overrides[k]
    return out


def run(plan, args={}):
    hammer = _merge(DEFAULT_HAMMER, args.get("pbt_hammer", {}))
    monitor = _merge(DEFAULT_MONITOR, args.get("pbt_monitor", {}))
    chaos = _merge(DEFAULT_CHAOS, args.get("pbt_chaos", {}))
    migration = _merge(DEFAULT_MIGRATION, args.get("pbt_migration", {}))

    upstream_args = {}
    for k in args:
        if k not in OURS:
            upstream_args[k] = args[k]

    # Migration mode reshapes the genesis: the egg emits binaryTrieTime =
    # amsterdam_time + fork_offset_seconds instead of scheduling the tree AT
    # genesis. Validated loudly, because both failure shapes look healthy: a
    # stock generator image emits no fork at all, and PBT unset leaves the
    # offset with nothing to schedule — either way the chain comes up
    # merkle-forever with no error anywhere.
    if migration["enabled"]:
        egg = dict(args.get("ethereum_genesis_generator_params", {}))
        if egg.get("image", "") == "":
            fail("pbt_migration needs ethereum_genesis_generator_params.image (the pbt-egg fork): " +
                 "the stock generator emits no binaryTrieTime")
        extra = dict(egg.get("extra_env", {}))
        if extra.get("PBT", "") != "true":
            fail("pbt_migration needs ethereum_genesis_generator_params.extra_env.PBT == \"true\": " +
                 "PBT selects the tree, the offset only schedules it")
        extra["PBT_OFFSET_SECONDS"] = str(migration["fork_offset_seconds"])
        egg["extra_env"] = extra
        upstream_args["ethereum_genesis_generator_params"] = egg
    net = ethereum_package.run(plan, upstream_args)

    # Execution clients only. all_participants includes the consensus side too, and a
    # participant can legitimately have no execution client.
    els = []
    for p in net.all_participants:
        if p.el_context != None:
            els.append(p.el_context)
    # Only the monitor needs a second opinion. A single-node run is legitimate for
    # debugging one client in isolation, which is exactly when you least want the package
    # refusing to start.
    if monitor["enabled"] and len(els) < 2:
        fail("pbt_monitor needs at least two execution clients: one node has nobody to " +
             "disagree with. Set pbt_monitor.enabled: false to run a single node.")

    plan.print("execution clients under test:")
    for el in els:
        plan.print("  {0} [{1}] {2}".format(el.service_name, el.client_name, el.rpc_http_url))

    if monitor["enabled"]:
        # pbtmonitor stays on in migration mode, deliberately: its only unconditional
        # genesis assertion is "not the EMPTY merkle root", which a merkle genesis with
        # an alloc passes, and its same-height cross-client comparison is
        # boundary-agnostic. The PBT-commitment assertion only arms when
        # expected_genesis_root is set, which migration args leave as the merkle root.
        _launch_monitor(plan, monitor, args, els)
    if hammer["enabled"]:
        _launch_hammer(plan, hammer, els, net.pre_funded_accounts)
    if migration["enabled"]:
        if chaos["enabled"]:
            plan.print("pbtchaos SKIPPED: its reorg cadence is not admission-gated against " +
                       "the fork boundary; migration-chaos (pbt_migration.chaos_profile) owns " +
                       "disruption in migration mode")
        _launch_migration(plan, migration, args, els)
    elif chaos["enabled"]:
        _launch_chaos(plan, chaos, args, net, els, hammer["senders"])

    return net


def _launch_monitor(plan, cfg, args, els):
    cmd = []
    for el in els:
        # name=engineURL,rpcURL — the monitor needs the engine API for its self-test, and
        # the plain RPC to follow heads.
        cmd += ["--el", "{0}=http://{1}:{2},{3}".format(
            el.service_name, el.ip_addr, el.engine_rpc_port_num, el.rpc_http_url)]
    cmd += ["--jwt", JWT_PATH, "--poll", cfg["poll"], "--probe-every", str(cfg["probe_every"])]
    # So the monitor can tell a partition we applied from a client that is actually wrong.
    if DISRUPTOOR_SERVICE in args.get("additional_services", []):
        disruptoor = plan.get_service(name=DISRUPTOOR_SERVICE)
        cmd += ["--disruptoor", "http://{0}:{1}".format(disruptoor.ip_address, DISRUPTOOR_PORT)]
    if cfg["expected_genesis_root"] != "":
        cmd += ["--expected-genesis-root", cfg["expected_genesis_root"]]
    if not cfg["verify_oracle"]:
        cmd += ["--verify-oracle=false"]

    plan.add_service(
        name="pbtmonitor",
        config=ServiceConfig(
            image=cfg["image"],
            files={JWT_MOUNT_DIR: JWT_ARTIFACT},
            cmd=cmd,
        ),
    )
    plan.print("started pbtmonitor: watching {0} execution clients for root divergence".format(len(els)))


def _launch_hammer(plan, cfg, els, prefunded):
    cmd = []
    for el in els:
        cmd += ["--rpc", el.rpc_http_url]
    # Send from the package's own prefunded accounts, taken from the END of the list.
    # Passing keys in beats pre-funding our own addresses through the genesis generator:
    # these are funded on whatever network the package just built, whatever its chain id.
    #
    # The end, not the start: spamoor's chainload spends the low-index accounts and
    # assertoor takes another. Sharing one is a nonce collision -- see _launch_chaos.
    n = cfg["senders"]
    if n > len(prefunded):
        fail("asked for {0} senders but the network only prefunds {1} accounts".format(
            n, len(prefunded)))
    for acct in prefunded[len(prefunded) - n:]:
        cmd += ["--key", acct.private_key]
    cmd += [
        "--interval", cfg["interval"],
        "--batch", str(cfg["batch"]),
        "--slots-per-tx", str(cfg["slots_per_tx"]),
        "--code-size", str(cfg["code_size"]),
    ]
    if cfg["only"] != "":
        cmd += ["--only", cfg["only"]]

    plan.add_service(
        name="pbthammer",
        config=ServiceConfig(image=cfg["image"], cmd=cmd),
    )
    plan.print("started pbthammer: 11 workloads, round-robin, from {0} prefunded accounts".format(n))


def _launch_chaos(plan, cfg, args, net, els, hammer_senders):
    # disruptoor is what applies the partitions and the shaping. Without it pbtchaos has
    # nothing to drive, and a missing selector target is the one failure that looks like
    # success, so refuse rather than start a no-op.
    services = args.get("additional_services", [])
    if DISRUPTOOR_SERVICE not in services:
        fail("pbt_chaos needs the '" + DISRUPTOOR_SERVICE + "' additional service: " +
             "add it to additional_services, or set pbt_chaos.enabled: false")

    disruptoor = plan.get_service(name=DISRUPTOOR_SERVICE)
    cmd = ["--disruptoor", "http://{0}:{1}".format(disruptoor.ip_address, DISRUPTOOR_PORT)]
    for el in els:
        cmd += ["--el", "{0}={1}".format(el.service_name, el.rpc_http_url)]
    # Proposer duties come from the consensus layer, so isolation forks can target the node
    # that is about to build rather than a node at random.
    for p in net.all_participants:
        if p.cl_context != None:
            cmd += ["--cl", "{0}={1}".format(p.cl_context.beacon_service_name, p.cl_context.beacon_http_url)]

    # Take the accounts just below the hammer's slice.
    prefunded = net.pre_funded_accounts
    n = cfg["senders"]
    end = len(prefunded) - hammer_senders
    if end - n < 0:
        fail("not enough prefunded accounts for pbt_chaos: need {0} below the hammer's {1}".format(
            n, hammer_senders))
    # ethereum-package hands specific indices to other services: spamoor takes 13 and
    # assertoor takes 9, both hardcoded upstream. Sharing one with them means both pick the
    # same nonce and every send after the first is rejected as underpriced -- silently, and
    # only under load.
    if end - n <= SPAMOOR_ACCOUNT:
        fail(("pbt_chaos would take prefunded accounts {0}..{1}, but spamoor hardcodes {2} " +
              "and assertoor {3}, out of {4} accounts total. Lower pbt_hammer.senders " +
              "({5}) or pbt_chaos.senders ({6}).").format(
            end - n, end - 1, SPAMOOR_ACCOUNT, ASSERTOOR_ACCOUNT, len(prefunded),
            hammer_senders, n))
    for acct in prefunded[end - n:end]:
        cmd += ["--key", acct.private_key]

    cmd += [
        "--listen", ":{0}".format(CHAOS_API_PORT),
        "--depth", str(cfg["depth"]),
        "--isolate-min-blocks", str(cfg["isolate_min_blocks"]),
        "--isolate-max-blocks", str(cfg["isolate_max_blocks"]),

        # Mapping a proposer's validator index back to a participant needs the range
        # size. 128 is ethereum-package's own default, so this agrees when unset.
        "--validators-per-node", str(args.get("network_params", {}).get("num_validator_keys_per_node", 128)),
        "--slot-seconds", "{0}s".format(args.get("network_params", {}).get("seconds_per_slot", 12)),
    ]
    for n in cfg["protect_nodes"]:
        cmd += ["--protect-node", str(n)]
    if cfg["isolate_for"] != "":
        cmd += ["--isolate-for", cfg["isolate_for"]]
    if not cfg["isolation"]:
        cmd += ["--isolation=false"]

    plan.add_service(
        name="pbtchaos",
        config=ServiceConfig(
            image=cfg["image"],
            cmd=cmd,
            ports={"http": PortSpec(number=CHAOS_API_PORT, transport_protocol="TCP", application_protocol="http")},
        ),
    )
    plan.print("started pbtchaos: isolation forks every {0}-{1} blocks; scenarios on POST /scenario/<name>".format(
        cfg["isolate_min_blocks"], cfg["isolate_max_blocks"]))


def _genesis_field(plan, name, jq_filter, fmt):
    """One value out of the GENERATED genesis.json, or a loud plan failure.

    run_sh's default image ships jq (upstream's own read-osaka-time step relies on
    exactly that), and a nonzero exit aborts the plan — which is the point: a missing
    binaryTrieTime means the wrong egg image, and T must never be guessed. fmt "%d"
    converts genesis.json's hex timestamp to decimal, the same printf trick the pinned
    ethereum-package uses for the shadowfork block height.
    """
    result = plan.run_sh(
        name=name,
        description="Reading {0} from the generated genesis".format(jq_filter),
        run=("v=$(jq -r '{0}' {1}/genesis.json); ".format(jq_filter, EL_CL_GENESIS_MOUNT) +
             "if [ -z \"$v\" ] || [ \"$v\" = null ]; then " +
             "echo \"genesis.json has no {0} — wrong genesis generator image?\" >&2; exit 1; fi; ".format(jq_filter) +
             "printf '{0}' \"$v\"".format(fmt)),
        files={EL_CL_GENESIS_MOUNT: EL_CL_GENESIS_ARTIFACT},
    )
    return result.output


def _launch_migration(plan, cfg, args, els):
    profile = cfg["chaos_profile"]
    if profile not in ["none", "smoke", "full"]:
        fail("pbt_migration.chaos_profile must be none, smoke or full, got {0}".format(profile))

    # T and the genesis time come from the generated genesis, never recomputed: the
    # egg wrote binaryTrieTime there and every client reads that file, so the tooling
    # must see the identical values. b* and every acceptance window derive from these.
    t = _genesis_field(plan, "read-binary-trie-time", ".config.binaryTrieTime", "%s")
    genesis_time = _genesis_field(plan, "read-genesis-time", ".timestamp", "%d")
    plan.print("migration fork: binaryTrieTime={0} genesis_time={1}".format(t, genesis_time))

    cmd = []
    for el in els:
        cmd += ["--el", "{0}={1}".format(el.service_name, el.rpc_http_url)]
    # Poll cadence stays on the binary's own 2s default. JSONL on
    # stdout so `kurtosis service logs` is the log, with nothing to mount or lose.
    # Samples every 30s, not the binary's 60s default: C6 counts all-non-null
    # samples, and a measured 30-minute-offset run accrued only ~37 at 1/min -
    # usable window, under the >=50 bar. Polling at 30s clears it with margin
    # (A3's ws fallback stays unneeded).
    cmd += ["--binary-trie-time", t, "--sample-interval", "30s", "--jsonl", "/dev/stdout"]
    plan.add_service(
        name="migration-monitor",
        config=ServiceConfig(image=cfg["monitor_image"], cmd=cmd),
    )
    plan.print("started migration-monitor: {0} execution clients, JSONL on stdout".format(len(els)))

    if profile == "none":
        plan.print("migration-chaos not launched: pbt_migration.chaos_profile is none")
        return
    _launch_migration_chaos(plan, cfg, args, els, t, genesis_time)


def _launch_migration_chaos(plan, cfg, args, els, t, genesis_time):
    # Same refusal as _launch_chaos: without disruptoor a chaos driver that starts
    # cleanly and disrupts nothing is the failure that looks like success.
    if DISRUPTOOR_SERVICE not in args.get("additional_services", []):
        fail("pbt_migration.chaos_profile needs the '" + DISRUPTOOR_SERVICE + "' additional " +
             "service: add it to additional_services, or set chaos_profile: none")

    disruptoor = plan.get_service(name=DISRUPTOOR_SERVICE)
    cmd = ["--disruptoor", "http://{0}:{1}".format(disruptoor.ip_address, DISRUPTOOR_PORT)]
    for el in els:
        cmd += ["--el", "{0}={1}".format(el.service_name, el.rpc_http_url)]
    for n in cfg["protect_nodes"]:
        cmd += ["--protect-node", str(n)]
    cmd += [
        "--genesis-time", genesis_time,
        "--binary-trie-time", t,
        "--profile", cfg["chaos_profile"],
        "--jsonl", "/dev/stdout",
    ]
    plan.add_service(
        name="migration-chaos",
        config=ServiceConfig(image=cfg["chaos_image"], cmd=cmd),
    )
    plan.print("started migration-chaos: profile {0}, hard stop at T-300s".format(cfg["chaos_profile"]))
