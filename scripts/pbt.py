#!/usr/bin/env python3
"""Operational front end for the PBT devnet: one command per question you might ask it.

  status         every execution client's head and state root, side by side
  verify         compare every client at the SAME block number
  forks          competing heads, how deep each branch is, and who is on which
  proposals      who was due to propose each slot, and who missed
  diagnose       where the chain split, and what the peers were doing then
  repeer         restart any consensus client left with no peers
  chaos-status   what pbtchaos is running, what is queued, and recent results
  scenario       ask pbtchaos to run one reorg scenario now
  split / heal   partition by hand, outside pbtchaos's queue

Every subcommand takes the enclave as its first positional argument (default "pbt").
The Makefile is the intended entry point; this is what it calls.
"""
import argparse
import json
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

SLOTS_PER_EPOCH = 32


# ---------------------------------------------------------------- kurtosis plumbing

def sh(*args):
    """Run a command and return its stdout.

    A failure is reported but not raised: callers treat an empty answer as "this service is
    not there", which is the right reading for one missing port and the wrong one for a
    kurtosis that is not running at all. Without the warning the second case is indis-
    tinguishable from an empty enclave.
    """
    p = subprocess.run(args, capture_output=True, text=True)
    if p.returncode != 0:
        print("warning: %s exited %d: %s" % (args[0], p.returncode,
                                             (p.stderr or "").strip()[:160]), file=sys.stderr)
    return p.stdout.strip()


def services(enclave, prefix):
    """Service names starting with prefix, from `kurtosis enclave inspect`.

    Service rows begin with a 12-hex-digit UUID and carry the name in column two. Matching
    that shape rather than scanning every word keeps a name out of the results when it
    appears in some other column, such as a port mapping.
    """
    out = []
    for line in sh("kurtosis", "enclave", "inspect", enclave).splitlines():
        parts = line.split()
        if len(parts) > 1 and len(parts[0]) == 12 and all(c in "0123456789abcdef" for c in parts[0]):
            if parts[1].startswith(prefix):
                out.append(parts[1])
    return sorted(set(out))


def url(enclave, service, port):
    """Base URL for a service port, or None if it is not reachable.

    `kurtosis port print` includes the scheme only when the port declares an application
    protocol, so an el rpc port comes back as "127.0.0.1:1234" while disruptoor's http port
    comes back with "http://". Normalise rather than guess. Returning None for an empty
    answer is what lets callers skip a service that is not running, instead of building the
    string "http://" and failing later with a confusing URL error.
    """
    u = sh("kurtosis", "port", "print", enclave, service, port)
    if not u:
        return None
    return u if u.startswith("http") else "http://" + u


def rpc(base, method, params, timeout=20, quiet=False):
    """JSON-RPC call. quiet=True returns None instead of raising, for callers that walk a
    list of nodes and must not stop at the first dead one."""
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(base, data=body, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r).get("result")
    except Exception:
        if quiet:
            return None
        raise


def get(base, path="", timeout=10, quiet=True):
    """HTTP GET returning parsed JSON. quiet=True returns None on any failure; diagnose
    wants the exception so it can print why a client did not answer."""
    try:
        with urllib.request.urlopen(base + path, timeout=timeout) as r:
            return json.load(r)
    except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        if quiet:
            return None
        raise


def slot_has_block(base, slot, retries=1):
    """Whether a slot has a block: True, False (genuinely absent), or None (could not tell).

    A 404 is the beacon node answering "no block at that slot", which is a real miss. A
    timeout, a 5xx or a dropped connection is the node failing to answer, which says nothing
    about the proposer. Collapsing the second into the first is how a beacon node that is
    merely slow -- which is what it is, under a chaos run -- manufactures the false miss rate
    this subcommand exists to disprove.
    """
    for _ in range(retries + 1):
        try:
            with urllib.request.urlopen(f"{base}/eth/v1/beacon/headers/{slot}", timeout=10) as r:
                json.load(r)
                return True
        except urllib.error.HTTPError as e:
            if e.code == 404:
                return False
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError):
            pass
    return None


def fork_choice_slots(base):
    """Slots holding a block in the fork-choice dump, and the range that dump covers.

    This is the only way to tell a slot whose block was reorged away from one that never had a
    block at all: both answer 404 on /eth/v1/beacon/headers/{slot}, and the ?slot= query form
    404s as well. The dump keeps non-canonical blocks, so a slot present here but absent from
    the canonical chain was proposed and then orphaned.

    It only reaches back to around the finalized checkpoint -- measured on this devnet, slots
    288..405 with finality at 320 -- so anything older cannot be classified either way.
    """
    d = get(base, "/eth/v1/debug/fork_choice")
    if not d:
        return set(), None, None
    slots = {int(n["slot"]) for n in (d.get("fork_choice_nodes") or []) if "slot" in n}
    if not slots:
        return set(), None, None
    return slots, min(slots), max(slots)


def short(h):
    return h[:12] if h else "?"


def need_els(enclave, minimum=1):
    els = services(enclave, "el-")
    if len(els) < minimum:
        sys.exit(f"need at least {minimum} execution client(s) in '{enclave}', found {len(els)}")
    return els


def api_or_die(enclave, service):
    base = url(enclave, service, "http")
    if not base:
        sys.exit(f"{service} is not running in '{enclave}'")
    return base


# ---------------------------------------------------------------- status

def cmd_status(a):
    els = need_els(a.enclave)
    print("%-26s %8s  %s" % ("CLIENT", "BLOCK", "STATE ROOT"))
    roots = []
    for el in els:
        base = url(a.enclave, el, "rpc")
        if not base:
            continue
        blk = rpc(base, "eth_getBlockByNumber", ["latest", False], quiet=True) or {}
        num = int(blk.get("number", "0x0"), 16)
        root = blk.get("stateRoot", "?")
        print("%-26s %8s  %s" % (el, num, root))
        roots.append(root)

    # Heads legitimately differ by a block or two, so this compares roots only as a hint.
    # `make verify` is the real check: it compares the SAME block number.
    if len(set(roots)) == 1:
        print("all clients on the same root")
    else:
        print("roots differ at the tip — normal if the heads differ; "
              "run 'make verify' to compare equal heights")


# ---------------------------------------------------------------- verify

def cmd_verify(a):
    names = need_els(a.enclave, 2)
    urls = {n: url(a.enclave, n, "rpc") for n in names}
    print("clients:", ", ".join(names))

    def head(n):
        try:
            return int(rpc(urls[n], "eth_blockNumber", []), 16)
        except Exception:
            return 0

    if a.wait:
        print(f"waiting for every client to reach block {a.blocks} ...")
        last = -1
        while True:
            hs = {n: head(n) for n in names}
            low = min(hs.values())
            if low != last:
                print("   " + "  ".join(
                    f"{n.split('-')[1]}{n.split('-')[2][:4]}={h}" for n, h in hs.items()))
                last = low
            if low >= a.blocks:
                break
            time.sleep(10)

    low = min(head(n) for n in names)
    if low < 1:
        sys.exit("no blocks produced yet")

    # Compare the MOST RECENT `blocks`, not blocks 1..N. A divergence is permanent once it
    # happens, so what matters is the tip: early agreeing blocks would otherwise outvote a
    # chain that has been split for an hour.
    top = low
    bottom = max(1, top - a.blocks + 1)
    print(f"comparing blocks {bottom}..{top} (the most recent {top - bottom + 1}) "
          f"across {len(names)} clients")

    agree = diverge = 0
    first_diverge = None
    for b in range(bottom, top + 1):
        roots, hashes = {}, {}
        for n in names:
            blk = rpc(urls[n], "eth_getBlockByNumber", [hex(b), False]) or {}
            roots[n], hashes[n] = blk.get("stateRoot"), blk.get("hash")
        if any(v is None for v in roots.values()):
            continue
        if len(set(roots.values())) == 1 and len(set(hashes.values())) == 1:
            agree += 1
        else:
            diverge += 1
            if first_diverge is None:
                first_diverge = b
            if diverge <= 3:
                print(f"  DIVERGE at block {b}:")
                for n in names:
                    print(f"    {n:28} hash={hashes[n]}  root={roots[n]}")
            elif diverge == 4:
                print("  ... (further divergences suppressed)")

    print(f"\n  agree: {agree}   diverge: {diverge}")
    if diverge:
        print(f"  RESULT: FAIL — clients first disagreed at block {first_diverge}")
        return 1
    print(f"  RESULT: PASS — all clients identical over {agree} blocks")
    return 0


# ---------------------------------------------------------------- forks

def consensus_forks(enclave, cls):
    print("CONSENSUS")
    heads_by_client = {}
    for svc in cls:
        base = url(enclave, svc, "http")
        if not base:
            continue
        h = get(base, "/eth/v2/debug/beacon/heads")
        if not h:
            continue
        heads_by_client[svc] = [(int(x["slot"]), x["root"]) for x in h["data"]]

    if not heads_by_client:
        print("  no consensus client answered")
        return

    allroots = {r for hs in heads_by_client.values() for _, r in hs}
    for svc, hs in heads_by_client.items():
        for slot, root in sorted(hs):
            print("  %-30s head slot %-6d %s" % (svc, slot, short(root)))
    if len(heads_by_client) < len(cls):
        print("  -> only %d of %d consensus clients answered; a fork held solely by a silent"
              % (len(heads_by_client), len(cls)))
        print("     client is invisible here, and a partitioned node is exactly that client")
    if len(allroots) == 1 and all(len(h) == 1 for h in heads_by_client.values()):
        print("  -> one head among the clients that answered, no fork in flight")
        return
    print("  -> %d distinct heads: the chain is forked right now" % len(allroots))

    # Each head has to be resolved against a client that actually HAS it: a partitioned
    # node's branch is absent from everyone else's dump, which is the point of it being a fork.
    dumps = {}
    for svc in heads_by_client:
        base = url(enclave, svc, "http")
        d = get(base, "/eth/v1/debug/fork_choice") if base else None
        if d:
            dumps[svc] = {n["block_root"]: n for n in d.get("fork_choice_nodes", [])}

    def ancestry(root):
        nodes = next((n for n in dumps.values() if root in n), None)
        if not nodes:
            return []
        chain, seen = [], set()
        while root in nodes and root not in seen:
            seen.add(root)
            chain.append(root)
            root = nodes[root].get("parent_root")
        return chain

    chains = {}
    for r in allroots:
        c = ancestry(r)
        if c:
            chains[r] = c
    if len(chains) < 2:
        return
    common = set.intersection(*(set(c) for c in chains.values()))
    if not common:
        print("  branches share no ancestor within the fork-choice window")
        return
    for root, chain in chains.items():
        depth = next((i for i, r in enumerate(chain) if r in common), len(chain))
        on = sorted(s for s, hs in heads_by_client.items() if any(r == root for _, r in hs))
        print("  branch %s: %d block(s) past the common ancestor, held by %s" %
              (short(root), depth, ", ".join(on) or "?"))


def execution_forks(enclave, els):
    """Compare every execution client at the SAME height, which is where a fork shows."""
    print("\nEXECUTION")
    heads = {}
    for svc in els:
        base = url(enclave, svc, "rpc")
        if not base:
            continue
        b = rpc(base, "eth_getBlockByNumber", ["latest", False], timeout=10, quiet=True)
        if b:
            heads[svc] = int(b["number"], 16)
    if len(heads) < 2:
        print("  need at least two execution clients")
        return

    n = min(heads.values())
    roots = {}
    for svc in heads:
        base = url(enclave, svc, "rpc")
        b = rpc(base, "eth_getBlockByNumber", [hex(n), False], timeout=10, quiet=True)
        if b:
            roots[svc] = (b["hash"], b["stateRoot"])
    for svc, (h, sr) in roots.items():
        print("  %-30s block %-6d %s  root %s" % (svc, n, short(h), short(sr)))

    if len(roots) < len(els):
        print("  -> %d of %d execution clients answered; the rest are not compared below"
              % (len(roots), len(els)))
    distinct = {h for h, _ in roots.values()}
    if len(distinct) == 1:
        print("  -> the %d client(s) that answered agree at block %d" % (len(roots), n))
    else:
        print("  -> %d different blocks at height %d: a fork the clients have not resolved" %
              (len(distinct), n))


def cmd_forks(a):
    cls = [a.via] if a.via else services(a.enclave, "cl-")
    els = services(a.enclave, "el-")
    if not cls and not els:
        sys.exit(f"no clients found in enclave '{a.enclave}'")

    consensus_forks(a.enclave, cls)
    execution_forks(a.enclave, els)

    api = url(a.enclave, "disruptoor", "http")
    if api:
        s = get(api, "/v1/state") or {}
        p, sh_ = len(s.get("partitions") or []), len(s.get("shaping") or [])
        if p or sh_:
            print("\n%d partition(s) and %d shaping rule(s) are applied — any fork above is "
                  "deliberate." % (p, sh_))


# ---------------------------------------------------------------- proposals

def cmd_proposals(a):
    cls = services(a.enclave, "cl-")
    if not cls:
        sys.exit(f"no consensus clients found in enclave '{a.enclave}'")
    # Ask the bootnode by default. It is participant 1, which pbtchaos is configured never to
    # disrupt, so it is the one vantage point that sees every proposer's blocks for the whole
    # run. Asking a node that gets partitioned makes the majority's slots look missing, which
    # is the false-miss artefact this subcommand exists to avoid.
    via = a.via or cls[0]
    base = url(a.enclave, via, "http")
    if not base:
        sys.exit(f"could not reach {via}")

    head_hdr = get(base, "/eth/v1/beacon/headers/head")
    if not head_hdr:
        sys.exit(f"{via} did not answer")
    head = int(head_hdr["data"]["header"]["message"]["slot"])
    head_epoch = head // SLOTS_PER_EPOCH

    first = a.from_epoch if a.from_epoch is not None else max(0, head_epoch - a.epochs)
    names = {i + 1: n for i, n in enumerate(services(a.enclave, "el-"))}

    fc_slots, fc_lo, fc_hi = fork_choice_slots(base)
    stats = {n: {"due": 0, "missed": 0, "orphaned": 0, "unknown": 0, "unverifiable": 0,
                 "slots": []} for n in names}
    unmapped = 0
    got, asked = [], list(range(first, head_epoch + 1))
    for epoch in asked:
        duties = get(base, f"/eth/v1/validator/duties/proposer/{epoch}")
        if not duties:
            # The beacon API only serves duties for a narrow recent window; older epochs 404.
            continue
        got.append(epoch)
        for duty in duties["data"]:
            slot = int(duty["slot"])
            if slot == 0 or slot > head:
                continue
            node = int(duty["validator_index"]) // a.validators_per_node + 1
            if node not in stats:
                unmapped += 1
                continue
            present = slot_has_block(base, slot)
            if present is None:
                # Neither due nor missed: a slot we could not check tells us nothing about
                # the proposer, and counting it either way would be a made-up number.
                stats[node]["unverifiable"] += 1
                continue
            stats[node]["due"] += 1
            if present:
                continue
            # No canonical block. Either it was never proposed, or it was proposed and chaos
            # reorged it away -- and a proposer whose block was orphaned did its job.
            if fc_lo is not None and fc_lo <= slot <= fc_hi:
                if slot in fc_slots:
                    stats[node]["orphaned"] += 1
                else:
                    stats[node]["missed"] += 1
                    stats[node]["slots"].append(slot)
            else:
                stats[node]["unknown"] += 1

    if not got:
        sys.exit(f"{via} served no proposer duties for epochs {first}..{head_epoch}")
    print(f"epochs {got[0]}..{got[-1]} ({len(got)} of the {len(asked)} asked for; the beacon API "
          f"only keeps recent duties), slots up to {head}, as seen by {via}\n")
    print(f"{'proposer':30} {'due':>5} {'missed':>7} {'orphan':>7} {'miss %':>8} "
          f"{'unknown':>8} {'unchecked':>10}   missed slots")
    worst = 0.0
    for node, name in names.items():
        st = stats[node]
        # Rate over the slots we could actually classify: an unknown outcome is not evidence
        # either way, and folding it into the denominator would quietly flatter the proposer.
        classified = st["due"] - st["unknown"]
        pct = 100 * st["missed"] / classified if classified else 0.0
        worst = max(worst, pct)
        rate = f"{pct:7.1f}%" if classified else "      -"
        shown = ", ".join(str(x) for x in st["slots"][:8]) + ("…" if len(st["slots"]) > 8 else "")
        print(f"{name:30} {st['due']:5} {st['missed']:7} {st['orphaned']:7} {rate} "
              f"{st['unknown']:8} {st['unverifiable']:10}   {shown}")

    orphaned = sum(st["orphaned"] for st in stats.values())
    if orphaned:
        print(f"\n{orphaned} slot(s) held a block that was later reorged out. Those proposers "
              f"built; chaos removed it, so they are not misses.")

    unknown = sum(st["unknown"] for st in stats.values())
    if unknown:
        lo = fc_lo if fc_lo is not None else "?"
        print(f"\n{unknown} slot(s) are older than the fork-choice window (which starts at slot "
              f"{lo}), so whether their blocks were orphaned or never built cannot be told from "
              f"the beacon API. Ask for fewer --epochs to stay inside the window.")

    unchecked = sum(st["unverifiable"] for st in stats.values())
    if unchecked:
        print(f"\n{unchecked} slot(s) could not be checked — the beacon node did not answer for "
              f"them. They count as neither due nor missed.")

    if unmapped:
        print(f"\n{unmapped} duties fell outside the known nodes — is --validators-per-node right?")

    # A partition in flight makes this meaningless, so say so rather than report a number
    # that will be read as a client defect.
    api = url(a.enclave, "disruptoor", "http")
    if api:
        state = get(api, "/v1/state") or {}
        if state.get("partitions") or state.get("shaping"):
            print("\nWARNING: disruptoor has state applied. A partitioned node's blocks do not")
            print("reach the node being asked, so these misses measure the partition, not the")
            print("proposer. Re-run with chaos disabled for a baseline.")
            return
    if worst > 25:
        # Disruptoor holding nothing right now does not mean the measured window was quiet:
        # a partition that ended a minute ago still cost the isolated node its slots. Only a
        # run with chaos disabled throughout measures a proposer rather than the harness.
        print(f"\nWorst miss rate is {worst:.1f}%. Check `make chaos-status` first — a partition")
        print("anywhere in these epochs costs the isolated node its slots even though none is")
        print("applied now. If the window really was quiet, check the proposer's CL for block")
        print("production errors.")


# ---------------------------------------------------------------- diagnose

def cmd_diagnose(a):
    els = services(a.enclave, "el-")
    cls = services(a.enclave, "cl-")
    if not els:
        sys.exit(f"no execution clients in '{a.enclave}'")

    print("=== execution layer ===")
    urls, heads = {}, {}
    for n in els:
        urls[n] = url(a.enclave, n, "rpc")
        try:
            heads[n] = int(rpc(urls[n], "eth_blockNumber", []), 16)
        except Exception as e:
            heads[n] = -1
            print(f"  {n:30} unreachable: {e}")
    for n in els:
        if heads[n] >= 0:
            blk = rpc(urls[n], "eth_getBlockByNumber", [hex(heads[n]), False]) or {}
            print(f"  {n:30} block {heads[n]:<6} root {blk.get('stateRoot','?')}")

    live = [n for n in els if heads[n] > 0]
    if len(live) >= 2:
        top = min(heads[n] for n in live)
        # Walk forward for the boundary. Linear rather than a bisect because a chain can
        # agree, split, and coincidentally re-agree at a height, and the FIRST break is
        # the one that matters.
        last_ok, first_bad = None, None
        for b in range(1, top + 1):
            hs = {n: (rpc(urls[n], "eth_getBlockByNumber", [hex(b), False]) or {}).get("hash")
                  for n in live}
            if len(set(hs.values())) == 1:
                last_ok = b
            else:
                first_bad = b
                break
        print()
        if first_bad is None:
            print(f"  clients agree on every block up to {top}")
        else:
            print(f"  last block all clients agreed on : {last_ok}")
            print(f"  FIRST DIVERGENCE                 : block {first_bad}")
            for n in live:
                blk = rpc(urls[n], "eth_getBlockByNumber", [hex(first_bad), False]) or {}
                print(f"    {n:30} {blk.get('hash')}")

    print("\n=== consensus layer ===")
    for c in cls:
        try:
            u = url(a.enclave, c, "http")
            peers = get(u, "/eth/v1/node/peer_count", timeout=20, quiet=False)["data"]["connected"]
            head = get(u, "/eth/v1/beacon/headers/head", timeout=20, quiet=False)["data"]
            slot = head["header"]["message"]["slot"]
            print(f"  {c:30} peers={peers:<4} head_slot={slot} root={head['root'][:18]}")
        except Exception as e:
            print(f"  {c:30} unreachable: {e}")

    # The correlation this whole subcommand exists for: peers going to zero and the chain
    # splitting are the same event seen from two sides.
    print("\n=== peer transitions (from CL logs) ===")
    for c in cls:
        cid = sh("docker", "ps", "--filter", f"name={c}", "--format", "{{.ID}}").split("\n")[0]
        if not cid:
            continue
        logs = subprocess.run(["docker", "logs", cid], capture_output=True, text=True)
        text = (logs.stdout or "") + (logs.stderr or "")
        transitions, prev = [], None
        for m in re.finditer(r"(\d{2}:\d{2}:\d{2})\.\d+\s+INFO\s+Synced.*?peers: \"(\d+)\"", text):
            t, p = m.group(1), int(m.group(2))
            if prev is None or (p == 0) != (prev == 0):
                transitions.append((t, p))
            prev = p
        summary = "  ".join(f"{t}->{p}" for t, p in transitions[:6]) or "(no Synced lines)"
        print(f"  {c:30} {summary}")
        for pat in ("Goodbye", "Disconnect", "banned", "Failed to connect", "peer_score"):
            hits = [l for l in text.splitlines() if pat.lower() in l.lower()]
            if hits:
                print(f"      {pat}: {len(hits)}x  e.g. {hits[0][:120]}")


# ---------------------------------------------------------------- repeer

def cmd_repeer(a):
    restarted = 0
    for svc in services(a.enclave, "cl-"):
        base = url(a.enclave, svc, "http")
        if not base:
            continue
        data = get(base, "/eth/v1/node/peer_count", timeout=5)
        peers = data["data"]["connected"] if data else "?"
        if str(peers) == "0":
            cid = sh("docker", "ps", "--filter", f"label=kurtosis_service_name={svc}",
                     "--filter", f"label=kurtosis_enclave_name={a.enclave}",
                     "--format", "{{.ID}}").split("\n")[0]
            if cid:
                print(f"==> {svc} has no peers; restarting")
                sh("docker", "restart", cid)
                restarted += 1
        else:
            print(f"    {svc} peers={peers}")

    if restarted == 0:
        print("nothing to do: every consensus client has peers")
    else:
        print(f"restarted {restarted}; give them a slot or two, then: make diagnose")


# ---------------------------------------------------------------- chaos

def cmd_chaos_status(a):
    api = api_or_die(a.enclave, "pbtchaos")
    try:
        body = get(api, "/status", quiet=False)
    except Exception as e:
        sys.exit(f"pbtchaos did not answer /status: {e}")
    print(json.dumps(body, indent=4))


def cmd_scenario(a):
    api = api_or_die(a.enclave, "pbtchaos")
    # Without a minority the daemon takes the next node in its rotation, which is what
    # spreads reorgs across both client types.
    q = []
    if a.depth:
        q.append(f"depth={a.depth}")
    if a.minority:
        q.append(f"minority={a.minority}")
    path = f"/scenario/{a.name}" + ("?" + "&".join(q) if q else "")
    req = urllib.request.Request(api + path, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            print(json.dumps(json.load(r), indent=4))
    except urllib.error.HTTPError as e:
        sys.exit(f"pbtchaos refused: {e.code} {e.read().decode()[:300]}")
    print(f"queued. watch it with: make chaos-status, "
          f"or kurtosis service logs {a.enclave} pbtchaos -f")


def cmd_split(a):
    api = api_or_die(a.enclave, "disruptoor")
    n = len(services(a.enclave, "el-"))
    if n < 2:
        sys.exit(f"need at least two execution clients, found {n}")
    majority = list(range(1, n))
    print(f"==> partitioning {{{','.join(map(str, majority))}}} | {{{n}}} on el and cl")
    payload = {"partitions": [{
        "name": "pbt-split",
        "groups": [
            {"node-index": majority, "client-type": ["execution", "beacon"]},
            {"node-index": [n], "client-type": ["execution", "beacon"]},
        ],
        "scope": ["el_p2p", "cl_p2p"],
        "symmetric": True,
    }]}
    req = urllib.request.Request(api + "/v1/state", method="PUT",
                                 data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=20):
        pass
    print("    applied. heal it with: make heal")


def cmd_heal(a):
    api = api_or_die(a.enclave, "disruptoor")
    req = urllib.request.Request(api + "/v1/state/clear", method="POST")
    with urllib.request.urlopen(req, timeout=20):
        pass
    print("==> healed. Check reconvergence with: make diagnose")


# ---------------------------------------------------------------- entry point

def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    def add(name, fn, **kw):
        p = sub.add_parser(name, **kw)
        p.add_argument("enclave", nargs="?", default="pbt")
        p.set_defaults(fn=fn)
        return p

    add("status", cmd_status)
    p = add("verify", cmd_verify)
    p.add_argument("--blocks", type=int, default=100)
    p.add_argument("--wait", action="store_true")
    p = add("forks", cmd_forks)
    p.add_argument("--via", default=None)
    p = add("proposals", cmd_proposals)
    p.add_argument("--epochs", type=int, default=4, help="how many epochs back to measure")
    p.add_argument("--from-epoch", type=int, default=None, help="start here instead")
    p.add_argument("--via", default=None, help="which consensus client to ask")
    p.add_argument("--validators-per-node", type=int, default=128)
    add("diagnose", cmd_diagnose)
    add("repeer", cmd_repeer)
    add("chaos-status", cmd_chaos_status)
    p = add("scenario", cmd_scenario)
    p.add_argument("name")
    p.add_argument("depth", nargs="?", default=None)
    p.add_argument("minority", nargs="?", default=None)
    add("split", cmd_split)
    add("heal", cmd_heal)

    a = ap.parse_args()
    return a.fn(a) or 0


if __name__ == "__main__":
    sys.exit(main())
