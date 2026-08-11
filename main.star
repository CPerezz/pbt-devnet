"""
A two-node PBT (EIP-8297) devnet.

There is deliberately no consensus client here. On this chain the genesis state root
is the binary-tree root, so the genesis block hash differs from what the standard CL
genesis tooling computes from genesis.json under merkle-patricia rules — a real CL
would embed the wrong hash and never agree with the EL. Removing the CL also removes
Gloas/ePBS client-support risk, and hands the driver full control of timestamps,
competing payloads and restart timing.

What replaces it is better for this purpose anyway: engine_newPayloadV5 makes the
importing node re-execute the block and compare its own computed binary-tree root
against the root the payload commits to. A VALID from the node that did NOT build the
block is therefore the state-root agreement assertion, delivered every block.

Run:
  scripts/build-images.sh          # builds pbt-geth:local, pbt-driver:local, pbt-hammer:local
  kurtosis run . --enclave pbt --args-file args/phase1.yaml
"""

DEFAULTS = {
    "geth_image": "pbt-geth:local",
    "driver_image": "pbt-driver:local",
    "hammer_image": "pbt-hammer:local",
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

GENESIS_PATH = "/network-configs/genesis.json"
JWT_PATH = "/jwt/jwtsecret"

RPC_PORT = 8545
ENGINE_PORT = 8551
P2P_PORT = 30303

# Flags every node needs. Three are not negotiable on this branch:
#   --state.scheme=path   hashdb is refused outright for the binary tree
#   --syncmode=full       pathdb refuses snap sync; flat state cannot be rebuilt
#   --override.genesis    PBT comes only from genesis JSON (there is no
#                         --override.pbt), and this is how ethereum-package
#                         launches geth too
# Never add --vmwitnessstats (refused on the tree) or --dev (cannot be PBT).
COMMON_GETH_FLAGS = [
    "--override.genesis=" + GENESIS_PATH,
    "--state.scheme=path",
    "--syncmode=full",
    "--state.size-tracking",
    "--authrpc.jwtsecret=" + JWT_PATH,
    "--authrpc.addr=0.0.0.0",
    "--authrpc.port={0}".format(ENGINE_PORT),
    "--authrpc.vhosts=*",
    "--http",
    "--http.addr=0.0.0.0",
    "--http.port={0}".format(RPC_PORT),
    "--http.vhosts=*",
    "--http.corsdomain=*",
    "--http.api=eth,net,web3,debug,txpool",
    "--rpc.allow-unprotected-txs",
    "--port={0}".format(P2P_PORT),
    "--nodiscover",
    "--maxpeers=1",
    "--verbosity=3",
]

# geth_node builds one execution-client entry. A non-geth client would not use this
# helper: it supplies its own image, cmd and ports directly, since every client has
# its own CLI. See "Adding an execution client" in the README.
def geth_node(cfg, name, extra_flags):
    return {
        "name": name,
        "image": cfg["geth_image"],
        "cmd": COMMON_GETH_FLAGS + extra_flags,
        "rpc_port": RPC_PORT,
        "engine_port": ENGINE_PORT,
    }


# The node set. THIS is the list to append to when adding an execution client — the
# driver's --el flags and the hammer's --rpc flags are both generated from it.
#
# The asymmetry is the point: instances of one binary with one config are close to
# deterministic and would agree by construction, so these two are pushed down
# different paths through the same tree and then required to produce identical roots.
#
#   geth-a  prunes under a small cache, so it re-reads the tree from disk
#   geth-b  archive with a large cache, and keeps preimages, which changes what the
#           state reader has available
def el_nodes(cfg):
    return [
        geth_node(cfg, "geth-a", ["--cache=512", "--cache.trie=10"]),
        geth_node(cfg, "geth-b", ["--cache=3072", "--cache.trie=40", "--gcmode=archive", "--cache.preimages"]),
        # Append here to add a client. Verified with a third geth entry; see the README.
    ]


def run(plan, args={}):
    cfg = dict(DEFAULTS)
    for k in args:
        cfg[k] = args[k]

    genesis = plan.upload_files(src="./genesis/genesis.json", name="pbt-genesis")
    jwt = plan.upload_files(src="./static/jwtsecret", name="pbt-jwt")

    for node in el_nodes(cfg):
        plan.add_service(
            name=node["name"],
            config=ServiceConfig(
                image=node["image"],
                ports={
                    "rpc": PortSpec(number=node["rpc_port"], transport_protocol="TCP", application_protocol="http"),
                    "engine-rpc": PortSpec(number=node["engine_port"], transport_protocol="TCP", application_protocol="http", wait=None),
                    "p2p": PortSpec(number=P2P_PORT, transport_protocol="TCP", application_protocol="", wait=None),
                },
                files={
                    "/network-configs": genesis,
                    "/jwt": jwt,
                },
                cmd=node["cmd"],
            ),
        )
        plan.print("started {0} ({1})".format(node["name"], node["image"]))

    # The nodes are intentionally left unpeered. The hammer submits every transaction
    # to every RPC endpoint directly, so all pools see the same load without devp2p
    # gossip, and the driver stays the single source of canonical blocks. Fewer moving
    # parts, and no dependence on the admin API.

    driver_cmd = []
    for node in el_nodes(cfg):
        driver_cmd += ["--el", "{0}=http://{1}:{2},http://{1}:{3}".format(
            node["name"], node["name"], node["engine_port"], node["rpc_port"])]
    driver_cmd += [
        "--jwt", JWT_PATH,
        "--slot-time", cfg["slot_time"],
        "--slots", str(cfg["slots"]),
        "--reorg-every", str(cfg["reorg_every"]),
        "--reorg-depth", str(cfg["reorg_depth"]),
        "--probe-every", str(cfg["probe_every"]),
    ]

    plan.add_service(
        name="pbtdriver",
        config=ServiceConfig(
            image=cfg["driver_image"],
            files={"/jwt": jwt},
            cmd=driver_cmd,
        ),
    )
    plan.print("started pbtdriver: FCUv4 -> getPayloadV6 -> newPayloadV5 to every node")

    if cfg["hammer_enabled"]:
        hammer_cmd = []
        for node in el_nodes(cfg):
            hammer_cmd += ["--rpc", "http://{0}:{1}".format(node["name"], node["rpc_port"])]
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
    plan.print("  kurtosis port print <enclave> {0} rpc".format(el_nodes(cfg)[0]["name"]))
