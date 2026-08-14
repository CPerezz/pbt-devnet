"""
A multi-client PBT (EIP-8297) devnet.

There is deliberately no consensus client. On this chain the genesis state root is the
binary-tree root, so the genesis block hash differs from what the standard CL genesis
tooling computes from genesis.json under merkle-patricia rules — a real CL would embed
a hash the EL never produces. Removing it also hands the driver full control of
timestamps, competing payloads and restart timing.

What replaces it is a better oracle anyway: engine_newPayloadV5 makes each importing
node re-execute the block and compare its own computed root against the root the
payload commits to. A VALID from a node that did NOT build the block is therefore the
state-root assertion, delivered every block.

Nodes come from the args file, not from this file. To add an execution client see
"Adding a client" in the README; what lives here is only HOW to launch each client
type, because flags are logic rather than config.

Run:
  scripts/build-images.sh
  kurtosis run . --enclave pbt --args-file args/devnet.yaml
"""

GENESIS_DIR = "/network-configs"
JWT_PATH = "/jwt/jwtsecret"

# Shared port vocabulary: the IDs are the same for every client, so the driver and
# hammer command builders never branch on client type; only the numbers are per-client.
RPC_PORT_ID = "rpc"
ENGINE_PORT_ID = "engine-rpc"
P2P_PORT_ID = "p2p"

DEFAULTS = {
    "driver_image": "pbt-driver:local",
    "hammer_image": "pbt-hammer:local",
    "default_ethereum_client_images": {"geth": "pbt-geth:local"},
    "nodes": [
        # Asymmetry is the point: instances of one binary with one config are close to
        # deterministic and would agree by construction, so these two are pushed down
        # different paths through the same tree and required to produce identical roots.
        {"name": "geth-a", "client": "geth", "extra_flags": ["--cache=512", "--cache.trie=10"]},
        {"name": "geth-b", "client": "geth", "extra_flags": [
            "--cache=3072", "--cache.trie=40", "--gcmode=archive", "--cache.preimages"]},
    ],
    # Asserted against every node's genesis state root at preflight — the
    # client-agnostic proof that the chain really is on the binary tree. Printed by
    # gengenesis; empty means unchecked.
    "expected_genesis_root": "",
    "slot_time": "3s",
    "slots": 0,           # 0 = run until stopped
    "reorg_every": 12,    # competing-payload reorg every N slots; 0 disables
    "reorg_depth": 2,
    "probe_every": 8,
    "hammer_enabled": True,
    "hammer_interval": "400ms",
    "hammer_batch": 4,
    "hammer_slots_per_tx": 20,
    "hammer_code_size": 12000,
}

CLIENT_TYPE = struct(geth="geth")


def _geth_flags(ports, genesis_path):
    """Three of these are not negotiable for geth on the binary tree:
    --state.scheme=path (hashdb is refused), --syncmode=full (pathdb refuses snap
    sync), and --override.genesis (PBT comes only from genesis JSON; there is no
    --override.pbt). Never --vmwitnessstats (refused) or --dev (cannot be PBT).
    """
    return [
        "--override.genesis=" + genesis_path,
        "--state.scheme=path",
        "--syncmode=full",
        "--state.size-tracking",
        "--authrpc.jwtsecret=" + JWT_PATH,
        "--authrpc.addr=0.0.0.0",
        "--authrpc.port={0}".format(ports.engine),
        "--authrpc.vhosts=*",
        "--http",
        "--http.addr=0.0.0.0",
        "--http.port={0}".format(ports.rpc),
        "--http.vhosts=*",
        "--http.corsdomain=*",
        "--http.api=eth,net,web3,debug,txpool",
        "--rpc.allow-unprotected-txs",
        "--port={0}".format(ports.p2p),
        "--nodiscover",
        "--maxpeers=1",
        "--verbosity=3",
    ]


# How to launch each client type. Adding a client means one entry here plus an image in
# the args file; nothing else in this file changes.
#
# genesis_file is per-client on purpose. Every EL reads one shared geth-format
# genesis.json today (geth --override.genesis, besu --genesis-file, nethermind
# --Init.ChainSpecPath), but the bare-metal devnets do ship besu.json and
# chainspec.json separately, so the filename is not hard-wired.
CLIENTS = {
    CLIENT_TYPE.geth: struct(
        flags=_geth_flags,
        genesis_file="genesis.json",
        ports=struct(rpc=8545, engine=8551, p2p=30303),
    ),
}


def _resolve(cfg, node):
    """Turn one args entry into a launchable node, or fail with a usable message."""
    client = node.get("client", CLIENT_TYPE.geth)
    if client not in CLIENTS:
        fail("unsupported client '{0}', need one of '{1}'".format(
            client, ",".join(sorted(CLIENTS.keys()))))
    spec = CLIENTS[client]

    images = cfg["default_ethereum_client_images"]
    if client not in images:
        fail("no image for client '{0}': add it to default_ethereum_client_images".format(client))

    genesis_path = GENESIS_DIR + "/" + spec.genesis_file
    return struct(
        name=node["name"],
        client=client,
        image=node.get("image", images[client]),
        ports=spec.ports,
        cmd=spec.flags(spec.ports, genesis_path) + node.get("extra_flags", []),
    )


def run(plan, args={}):
    cfg = dict(DEFAULTS)
    for k in args:
        cfg[k] = args[k]

    nodes = [_resolve(cfg, n) for n in cfg["nodes"]]
    if len(nodes) < 2:
        fail("need at least two nodes: one node has nobody to disagree with")

    genesis = plan.upload_files(src="./genesis/genesis.json", name="pbt-genesis")
    jwt = plan.upload_files(src="./static/jwtsecret", name="pbt-jwt")

    for node in nodes:
        plan.add_service(
            name=node.name,
            config=ServiceConfig(
                image=node.image,
                ports={
                    RPC_PORT_ID: PortSpec(number=node.ports.rpc, transport_protocol="TCP", application_protocol="http"),
                    ENGINE_PORT_ID: PortSpec(number=node.ports.engine, transport_protocol="TCP", application_protocol="http", wait=None),
                    P2P_PORT_ID: PortSpec(number=node.ports.p2p, transport_protocol="TCP", application_protocol="", wait=None),
                },
                files={GENESIS_DIR: genesis, "/jwt": jwt},
                cmd=node.cmd,
            ),
        )
        plan.print("started {0} [{1}] {2}".format(node.name, node.client, node.image))

    # The nodes are intentionally unpeered. The hammer submits every transaction to
    # every RPC directly, so all pools see the same load without devp2p gossip, and the
    # driver stays the single source of canonical blocks.

    driver_cmd = []
    for node in nodes:
        driver_cmd += ["--el", "{0}=http://{1}:{2},http://{1}:{3}".format(
            node.name, node.name, node.ports.engine, node.ports.rpc)]
    driver_cmd += [
        "--jwt", JWT_PATH,
        "--slot-time", cfg["slot_time"],
        "--slots", str(cfg["slots"]),
        "--reorg-every", str(cfg["reorg_every"]),
        "--reorg-depth", str(cfg["reorg_depth"]),
        "--probe-every", str(cfg["probe_every"]),
    ]
    if cfg["expected_genesis_root"] != "":
        driver_cmd += ["--expected-genesis-root", cfg["expected_genesis_root"]]

    plan.add_service(
        name="pbtdriver",
        config=ServiceConfig(image=cfg["driver_image"], files={"/jwt": jwt}, cmd=driver_cmd),
    )
    plan.print("started pbtdriver: FCUv4 -> getPayloadV6 -> newPayloadV5 to every node")

    if cfg["hammer_enabled"]:
        hammer_cmd = []
        for node in nodes:
            hammer_cmd += ["--rpc", "http://{0}:{1}".format(node.name, node.ports.rpc)]
        hammer_cmd += [
            "--interval", cfg["hammer_interval"],
            "--batch", str(cfg["hammer_batch"]),
            "--slots-per-tx", str(cfg["hammer_slots_per_tx"]),
            "--code-size", str(cfg["hammer_code_size"]),
        ]
        plan.add_service(
            name="pbthammer",
            config=ServiceConfig(image=cfg["hammer_image"], cmd=hammer_cmd),
        )
        plan.print("started pbthammer: fanout / storage / codedup / destruct")

    plan.print("")
    plan.print("  kurtosis service logs <enclave> pbtdriver -f     # the oracle")
    plan.print("  kurtosis port print <enclave> {0} rpc".format(nodes[0].name))
