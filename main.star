"""
A mixed-client PBT (EIP-8297) devnet, driven by real consensus clients.

This composes ethpandaops/ethereum-package rather than launching clients itself: that
package already knows how to run geth, besu and lighthouse together, wire the engine API,
generate genesis and hand out validator keys. What it does not know is the binary tree, and
that gap is closed with two forks and one config value, with no patch to the package:

  * pbt-egg:local  — a fork of ethereum-genesis-generator that emits the tree keys AND
    bundles a fork of eth-beacon-genesis whose go.mod replaces go-ethereum with the
    EIP-8297 branch. Without it the consensus genesis embeds a merkle-patricia block hash
    the execution layer will never produce, and the chain never starts. Reached through the
    supported `ethereum_genesis_generator_params.image` hook.
  * network_params.network stays "kurtosis". This is load-bearing: both el launchers pick
    full sync only for that network name, and the binary tree refuses snap sync outright.
    A custom network name silently gets --syncmode=snap and the engine API dies.

On top of the network this adds two services of our own:

  pbthammer  transaction load shaped at what the tree changed, not at throughput
  pbtmonitor watches every execution client for state-root divergence, and proves its own
             oracle by feeding a corrupted payload through the engine API

Both are optional and independently switchable, so `kurtosis run` with load disabled is a
quiet baseline.

Run `make up`.
"""

ethereum_package = import_module("github.com/ethpandaops/ethereum-package/main.star")

# Our own args keys. ethereum-package sanity-checks its input and fails on anything it does
# not recognise, so these are removed before its args are handed over.
OURS = [
    "pbt_hammer",
    "pbt_monitor",
]

DEFAULT_HAMMER = {
    "enabled": True,
    "image": "pbt-hammer:local",
    "interval": "400ms",
    "batch": 4,
    "slots_per_tx": 20,
    "code_size": 12000,
    # How many of the package's prefunded accounts to send from. They come with private
    # keys, so the hammer needs no premine of its own.
    "senders": 6,
    "only": "",
}

DEFAULT_MONITOR = {
    "enabled": True,
    "image": "pbt-driver:local",
    "verify_oracle": True,
    "probe_every": 8,
}

# ethereum-package uploads the engine API secret under this fixed artifact name, so the
# monitor can mount the same one the clients use and speak the engine API itself.
JWT_ARTIFACT = "jwt_file"
JWT_MOUNT_DIR = "/jwt"
JWT_PATH = JWT_MOUNT_DIR + "/jwtsecret"


def _merge(defaults, overrides):
    out = dict(defaults)
    for k in overrides:
        out[k] = overrides[k]
    return out


def run(plan, args={}):
    hammer = _merge(DEFAULT_HAMMER, args.get("pbt_hammer", {}))
    monitor = _merge(DEFAULT_MONITOR, args.get("pbt_monitor", {}))

    upstream_args = {}
    for k in args:
        if k not in OURS:
            upstream_args[k] = args[k]

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
        _launch_monitor(plan, monitor, els)
    if hammer["enabled"]:
        _launch_hammer(plan, hammer, els, net.pre_funded_accounts)

    return net


def _launch_monitor(plan, cfg, els):
    cmd = []
    for el in els:
        # name=engineURL,rpcURL — the monitor needs the engine API for its self-test, and
        # the plain RPC to follow heads.
        cmd += ["--el", "{0}=http://{1}:{2},{3}".format(
            el.service_name, el.ip_addr, el.engine_rpc_port_num, el.rpc_http_url)]
    cmd += ["--jwt", JWT_PATH, "--probe-every", str(cfg["probe_every"])]
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
    # Send from the package's own prefunded accounts. Passing keys in beats pre-funding our
    # own addresses through the genesis generator: these are guaranteed funded on whatever
    # network the package just built, whatever its chain id or alloc.
    n = cfg["senders"]
    if n > len(prefunded):
        fail("asked for {0} senders but the network only prefunds {1} accounts".format(
            n, len(prefunded)))
    for acct in prefunded[:n]:
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
