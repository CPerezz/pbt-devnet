"""
Mixed-client PBT (EIP-8297) devnet, driven by real consensus clients, on top of
ethpandaops/ethereum-package. The binary tree is added via pbt-egg:local (a fork of
ethereum-genesis-generator + eth-beacon-genesis) through the supported
`ethereum_genesis_generator_params.image` hook -- no patch to the package.

Three services of our own:
  pbthammer  transaction load shaped at what the tree changed
  pbtmonitor watches every execution client for state-root divergence; proves its own
             oracle via a corrupted payload through the engine API (observes only)
  pbtchaos   forces reorgs by isolating the next proposer; owns disruptoor exclusively

In migration mode (pbt_migration.enabled) the chain starts on the merkle-patricia trie and
forks to the binary tree at T = genesis + fork_offset_seconds:
  migration-monitor follows debug_migrationProgress and shadow roots, emits JSONL
  migration-chaos   drives disruptoor on a gated schedule around the fork boundary;
                     pbtchaos is skipped in migration mode

All three are independently switchable. Run `make up`.
"""

ethereum_package = import_module("github.com/ethpandaops/ethereum-package/main.star@5ec41d44ae23fb01b036c11b25d98547cc9c3be4")

# Our own args keys; stripped before ethereum-package sees args (it rejects unknown keys).
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
    # "none" disables migration-chaos; other values select its schedule.
    "chaos_profile": "none",
    "monitor_image": "pbt-migration-monitor:local",
    "chaos_image": "pbt-migration-chaos:local",
    "gate_image": "pbt-migration-gate:local",
    # Participant 1 is the bootnode and is never disrupted.
    "protect_nodes": [1],
    # The anchor holds the heavy stake and is never partitioned: fork choice makes the
    # lighter side of every heal rewind, so every light is forced back onto its chain -
    # including across the fork - while it never has to rewind. It must be protected.
    "anchor_node": 1,
    "anchor_validators": 256,
    "light_validators": 128,
    # Post-switchover reorg: none, short-light, or deep-pair (two lights, 40% together).
    "post_op": "deep-pair",
    # The monitor's live view. Empty disables it.
    "monitor_http_port": 8080,
}

DEFAULT_HAMMER = {
    "enabled": True,
    "image": "pbt-hammer:local",
    # Keep blocks under half the gas limit or the base fee compounds unpayably.
    "interval": "1s",
    "batch": 2,
    "slots_per_tx": 20,
    "code_size": 6000,
    # Uses the package's own prefunded accounts; no premine of its own needed.
    "senders": 4,
    "only": "",
}

# pbtchaos serialises disruptions through one queue; `make split`/`make heal` bypass it.
DEFAULT_CHAOS = {
    "enabled": True,
    "image": "pbt-chaos:local",
    # Runs behind the gate in migration mode instead of being skipped; set gate: true.
    "gate": False,
    # Depth ceiling for the gated run: caps how deep a post-fork reorg can go.
    "gate_max_depth": 8,
    # Periodic one-block reorgs from isolating the next proposer; victim rotates.
    "isolation": True,
    "isolate_min_blocks": 15,
    "isolate_max_blocks": 30,
    # Empty means two slots: the slot before the duty plus the proposal slot.
    "isolate_for": "",
    # Default depth for `make scenario` when none is given.
    "depth": 10,
    # Participant 1 is the bootnode; a partition would strand it with no peers.
    "protect_nodes": [1],
    # Sender pair walked per scenario run so a reused key never reads a stale nonce.
    "senders": 3,
}

DEFAULT_MONITOR = {
    "enabled": True,
    "image": "pbt-monitor:local",
    "verify_oracle": True,
    "poll": "2s",
    "probe_every": 8,
    # Only positive proof the clients are on the binary tree; must be the root of
    # THIS devnet's genesis (not `make genesis`'s, which allocs different accounts).
    "expected_genesis_root": "",
}

# Prefunded accounts other services claim upstream, and will not negotiate over.
SPAMOOR_ACCOUNT = 13
ASSERTOOR_ACCOUNT = 9

    # disruptoor's listen port; pbtchaos speaks its native API rather than start-up config.
DISRUPTOOR_SERVICE = "disruptoor"
DISRUPTOOR_PORT = 7700
CHAOS_API_PORT = 7800

    # Fixed artifact name ethereum-package uploads the engine API secret under.
JWT_ARTIFACT = "jwt_file"
JWT_MOUNT_DIR = "/jwt"
JWT_PATH = JWT_MOUNT_DIR + "/jwtsecret"

    # Fixed artifact name holding the generated genesis.json (StoreSpec, pinned revision).
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

    # Migration mode rewrites the genesis generator's env so the egg emits
    # binaryTrieTime = amsterdam_time + fork_offset_seconds instead of PBT-at-genesis.
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
        # Skewed stake only needed when a profile actually partitions the network.
        if migration["chaos_profile"] != "none":
            upstream_args["participants"] = _weight_participants(
                upstream_args.get("participants", []), migration)
    net = ethereum_package.run(plan, upstream_args)

    # all_participants also includes consensus-only entries with no execution client.
    els = []
    for p in net.all_participants:
        if p.el_context != None:
            els.append(p.el_context)
    # A single node is legitimate for isolated debugging; only the monitor needs two.
    if monitor["enabled"] and len(els) < 2:
        fail("pbt_monitor needs at least two execution clients: one node has nobody to " +
             "disagree with. Set pbt_monitor.enabled: false to run a single node.")

    plan.print("execution clients under test:")
    for el in els:
        plan.print("  {0} [{1}] {2}".format(el.service_name, el.client_name, el.rpc_http_url))

    if monitor["enabled"]:
        # Stays on in migration mode: its cross-client comparison is boundary-agnostic,
        # and its genesis assertion only arms when expected_genesis_root is set.
        _launch_monitor(plan, monitor, args, els)
    if hammer["enabled"]:
        _launch_hammer(plan, hammer, els, net.pre_funded_accounts)
    if migration["enabled"]:
        # Guard: migration mode's evidence contract only exists for registered clients.
        for p in net.all_participants:
            if p.el_context == None:
                fail("pbt_migration needs every participant to run an execution client: " +
                     "victim selection and stake weighting map participant indices to ELs 1:1")
        for el in els:
            if el.client_name not in MIGRATION_READY_CLIENTS:
                fail(("pbt_migration supports {0} for now; participant runs {1}. " +
                      "Onboarding a client needs its bootstrap + a registry entry " +
                      "(see README, 'Adding a client to the migration devnet').").format(
                    MIGRATION_READY_CLIENTS, el.client_name))
        t, genesis_time = _launch_migration(plan, migration, args, els, net)
        if chaos["enabled"] and chaos["gate"]:
            _launch_gated_chaos(plan, migration, chaos, args, net, els, hammer["senders"], t, genesis_time)
        elif chaos["enabled"]:
            plan.print("pbtchaos SKIPPED: its reorg cadence knows nothing about the fork " +
                       "boundary. Set pbt_chaos.gate: true to run it after the switchover, " +
                       "or leave it off; migration-chaos owns disruption until then")
    elif chaos["enabled"]:
        _launch_chaos(plan, chaos, args, net, els, hammer["senders"])

    return net


def _launch_monitor(plan, cfg, args, els):
    cmd = []
    for el in els:
        # engineURL,rpcURL: the monitor needs both the engine API and plain RPC.
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
    # Uses the package's own prefunded accounts rather than a separate premine.
    #
    # Takes from the end of the list: spamoor/assertoor spend the low-index accounts.
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


def _chaos_cmd(plan, cfg, args, net, els, hammer_senders, extra_protect, validator_counts):
    """The reorg service's argv, shared by the plain and gated launches: keeps both
    on identical endpoints, accounts, and cadence."""
    # Refuse rather than start a no-op if disruptoor is not wired up.
    services = args.get("additional_services", [])
    if DISRUPTOOR_SERVICE not in services:
        fail("pbt_chaos needs the '" + DISRUPTOOR_SERVICE + "' additional service: " +
             "add it to additional_services, or set pbt_chaos.enabled: false")

    disruptoor = plan.get_service(name=DISRUPTOOR_SERVICE)
    cmd = ["--disruptoor", "http://{0}:{1}".format(disruptoor.ip_address, DISRUPTOOR_PORT)]
    for el in els:
        cmd += ["--el", "{0}={1}".format(el.service_name, el.rpc_http_url)]
    # Proposer duties from the consensus layer let isolation target the next proposer.
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
    # spamoor hardcodes account 13, assertoor 9; sharing one causes a silent nonce collision.
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

        # 128 matches ethereum-package's own default validator range size.
        "--validators-per-node", str(args.get("network_params", {}).get("num_validator_keys_per_node", 128)),
        "--slot-seconds", "{0}s".format(args.get("network_params", {}).get("seconds_per_slot", 12)),
    ]
    for n in cfg["protect_nodes"] + extra_protect:
        cmd += ["--protect-node", str(n)]
    if cfg["isolate_for"] != "":
        cmd += ["--isolate-for", cfg["isolate_for"]]
    if not cfg["isolation"]:
        cmd += ["--isolation=false"]
    if validator_counts != "":
        cmd += ["--validator-counts", validator_counts]
    if cfg["gate_max_depth"] > 0 and validator_counts != "":
        cmd += ["--max-depth", str(cfg["gate_max_depth"])]
    return cmd


def _launch_chaos(plan, cfg, args, net, els, hammer_senders):
    cmd = _chaos_cmd(plan, cfg, args, net, els, hammer_senders, [], "")
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
    """One value out of the GENERATED genesis.json, or a loud plan failure: a missing
    binaryTrieTime means the wrong egg image, and T must never be guessed."""
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


MIGRATION_PROFILES = ["none", "composite", "composite-smoke"]

# Clients with a migration evidence contract (bootstrap shim, introspection RPCs,
# registry entry). Erigon rejects any binaryTrieTime later than genesis; not ready yet.
MIGRATION_READY_CLIENTS = ["geth"]


def _weight_participants(participants, cfg):
    """Give the anchor its validator share, everyone else the rest: a pair of lights
    (>1/3) stalls finality without winning, and no single light can outweigh the anchor."""
    anchor = cfg["anchor_node"]
    out = []
    for i, p in enumerate(participants):
        weighted = dict(p)
        if i + 1 == anchor:
            weighted["validator_count"] = cfg["anchor_validators"]
        else:
            weighted["validator_count"] = cfg["light_validators"]
        out.append(weighted)
    return out


def _anchor_share(cfg, els):
    """The anchor's share of the validator set, derived from the same numbers that render
    validator_count so the chaos driver's admission math cannot drift."""
    anchor = cfg["anchor_validators"]
    total = anchor + (len(els) - 1) * cfg["light_validators"]
    return float(anchor) / float(total)


def _launch_migration(plan, cfg, args, els, net):
    profile = cfg["chaos_profile"]
    if profile not in MIGRATION_PROFILES:
        fail("pbt_migration.chaos_profile must be one of {0}, got {1}".format(
            MIGRATION_PROFILES, profile))

    # Read back from the GENERATED genesis rather than recomputed, so tooling matches clients.
    t = _genesis_field(plan, "read-binary-trie-time", ".config.binaryTrieTime", "%s")
    genesis_time = _genesis_field(plan, "read-genesis-time", ".timestamp", "%d")
    plan.print("migration fork: binaryTrieTime={0} genesis_time={1}".format(t, genesis_time))

    _launch_migration_monitor(plan, cfg, args, els, net, t, genesis_time)

    if profile == "none":
        plan.print("migration-chaos not launched: pbt_migration.chaos_profile is none")
        return t, genesis_time

    anchor = cfg["anchor_node"]
    if anchor < 1 or anchor > len(els):
        fail("pbt_migration.anchor_node is {0}, outside the {1} execution clients".format(
            anchor, len(els)))
    if anchor not in cfg["protect_nodes"]:
        fail(("pbt_migration.anchor_node is {0} but protect_nodes is {1}: the anchor holds the " +
              "heavy stake and must never be partitioned, or an island could win a heal").format(
            anchor, cfg["protect_nodes"]))
    if len(els) - len(cfg["protect_nodes"]) < 2:
        fail("the migration profiles need at least two disruptable lights; {0} clients, {1} protected".format(
            len(els), cfg["protect_nodes"]))
    _launch_migration_chaos(plan, cfg, args, els, t, genesis_time, net)
    return t, genesis_time


def _launch_migration_monitor(plan, cfg, args, els, net, t, genesis_time):
    cmd = []
    for el in els:
        cmd += ["--el", "{0}={1}".format(el.service_name, el.rpc_http_url)]
    # 30s sampling (not the default minute): the default gave too few cross-node samples.
    cmd += ["--binary-trie-time", t, "--sample-interval", "30s", "--jsonl", "/dev/stdout"]
    ports = {}
    if cfg["monitor_http_port"] > 0:
        # The page draws on a slot axis and overlays the reorg service's schedule and
        # disruptoor's applied partitions; consensus peers come from the beacon APIs.
        cmd += [
            "--http", ":{0}".format(cfg["monitor_http_port"]),
            "--genesis-time", genesis_time,
            "--seconds-per-slot", str(_slot_seconds(args)),
        ]
        for p in net.all_participants:
            if p.cl_context != None:
                cmd += ["--cl", "{0}={1}".format(p.cl_context.beacon_service_name, p.cl_context.beacon_http_url)]
        # The chaos launch refuses a profile without disruptoor with its own message; skip here.
        if cfg["chaos_profile"] != "none" and DISRUPTOOR_SERVICE in args.get("additional_services", []):
            disruptoor = plan.get_service(name=DISRUPTOOR_SERVICE)
            cmd += [
                "--disruptoor", "http://{0}:{1}".format(disruptoor.ip_address, DISRUPTOOR_PORT),
                "--profile", cfg["chaos_profile"],
                "--anchor-node", str(cfg["anchor_node"]),
                "--anchor-share", str(_anchor_share(cfg, els)),
            ]
            for n in cfg["protect_nodes"]:
                cmd += ["--protect-node", str(n)]
        ports["http"] = PortSpec(
            number=cfg["monitor_http_port"], transport_protocol="TCP", application_protocol="http")
    plan.add_service(
        name="migration-monitor",
        config=ServiceConfig(image=cfg["monitor_image"], cmd=cmd, ports=ports),
    )
    plan.print("started migration-monitor: {0} execution clients, JSONL on stdout".format(len(els)))


def _launch_migration_chaos(plan, cfg, args, els, t, genesis_time, net):
    # Same refusal as _launch_chaos: no-op without disruptoor looks like success.
    if DISRUPTOOR_SERVICE not in args.get("additional_services", []):
        fail("pbt_migration.chaos_profile needs the '" + DISRUPTOOR_SERVICE + "' additional " +
             "service: add it to additional_services, or set chaos_profile: none")

    disruptoor = plan.get_service(name=DISRUPTOOR_SERVICE)
    cmd = ["--disruptoor", "http://{0}:{1}".format(disruptoor.ip_address, DISRUPTOOR_PORT)]
    for el in els:
        cmd += ["--el", "{0}={1}".format(el.service_name, el.rpc_http_url)]
    for n in cfg["protect_nodes"]:
        cmd += ["--protect-node", str(n)]
    # Two senders for the straddle injector, from accounts 10-11 (between assertoor's
    # hardcoded 9 and spamoor's hardcoded 13, so neither collides).
    prefunded = net.pre_funded_accounts
    if len(prefunded) > 11:
        cmd += ["--key", prefunded[10].private_key, "--key", prefunded[11].private_key]
    else:
        plan.print("straddle injector disabled: fewer than 12 prefunded accounts")
    cmd += [
        "--genesis-time", genesis_time,
        "--binary-trie-time", t,
        "--profile", cfg["chaos_profile"],
        "--anchor-node", str(cfg["anchor_node"]),
        "--anchor-share", str(_anchor_share(cfg, els)),
        "--seconds-per-slot", str(_slot_seconds(args)),
        "--jsonl", "/dev/stdout",
    ]
    plan.add_service(
        name="migration-chaos",
        config=ServiceConfig(image=cfg["chaos_image"], cmd=cmd),
    )
    plan.print("started migration-chaos: profile {0}, anchor is participant {1}".format(
        cfg["chaos_profile"], cfg["anchor_node"]))


def _slot_seconds(args):
    return int(args.get("network_params", {}).get("seconds_per_slot", 12))


def _launch_gated_chaos(plan, migration, chaos, args, net, els, hammer_senders, t, genesis_time):
    """Runs the tree-at-genesis reorg service behind the migration gate: waits for every
    client to finish, runs one partition, then execs the service with no overlap."""
    disruptoor = plan.get_service(name=DISRUPTOOR_SERVICE)
    api = "http://{0}:{1}".format(disruptoor.ip_address, DISRUPTOOR_PORT)

    cmd = ["--disruptoor", api]
    for el in els:
        cmd += ["--el", "{0}={1}".format(el.service_name, el.rpc_http_url)]
    for p in net.all_participants:
        if p.cl_context != None:
            cmd += ["--cl", "{0}={1}".format(p.cl_context.beacon_service_name, p.cl_context.beacon_http_url)]
    for n in migration["protect_nodes"]:
        cmd += ["--protect-node", str(n)]
    cmd += [
        "--genesis-time", genesis_time,
        "--binary-trie-time", t,
        "--profile", migration["chaos_profile"],
        "--anchor-node", str(migration["anchor_node"]),
        "--anchor-share", str(_anchor_share(migration, els)),
        "--seconds-per-slot", str(_slot_seconds(args)),
        "--post-op", migration["post_op"],
        "--jsonl", "/dev/stdout",
    ]

    # From here on: the command the gate execs once it hands over. Every test node is a
    # light, so its scenarios need no extra protection beyond the anchor.
    counts = []
    for i in range(len(els)):
        if i + 1 == migration["anchor_node"]:
            counts.append(str(migration["anchor_validators"]))
        else:
            counts.append(str(migration["light_validators"]))
    cmd += ["pbtchaos"] + _chaos_cmd(
        plan, chaos, args, net, els, hammer_senders,
        [], ",".join(counts))

    # No ports declared: kurtosis would wait for one to accept connections before
    # calling the service started, and this one binds only once it hands over.
    plan.add_service(
        name="migration-gate",
        config=ServiceConfig(image=migration["gate_image"], cmd=cmd),
    )
    plan.print(("started migration-gate: waits for every client to finish, runs a {0} " +
                "partition, then hands the reorg service its API on port {1} inside " +
                "the enclave").format(migration["post_op"], CHAOS_API_PORT))
