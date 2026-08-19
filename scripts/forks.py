#!/usr/bin/env python3
"""Show the forks: competing heads, how deep each branch is, and who is on which.

This exists because forky cannot work on this chain. Under Gloas the beacon block no longer
carries an execution payload, so lighthouse records ExecutionStatus::Irrelevant and
serialises `"validity": null` for every fork-choice node. forky's parser (go-eth2-client)
accepts only valid/invalid/optimistic and discards the ENTIRE dump on the first bad field,
so it logs one error per node per slot and renders nothing. `latest` is the newest image and
it is the one that fails; upstream has no fix. Everything else in the dump is intact --
218 of 219 nodes carry `parent_root` -- so the tree is perfectly reconstructible here.

Two views, because they answer different questions:

  consensus   every head each client knows about, and the depth from a fork's tip back to
              the common ancestor. This is what "a 13-block branch was abandoned" looks like
              while it is still happening.
  execution   the same height on every execution client, compared by state root. A fork is
              invisible at the tip when heads differ by a block, so this compares equals.

Usage:
  scripts/forks.py [enclave] [--via cl-2-lighthouse-geth]
"""
import argparse
import json
import subprocess
import sys
import urllib.error
import urllib.request


def sh(*args):
    return subprocess.run(args, capture_output=True, text=True).stdout.strip()


def services(enclave, prefix):
    out = sh("kurtosis", "enclave", "inspect", enclave)
    names = []
    for line in out.splitlines():
        for word in line.split():
            if word.startswith(prefix) and word not in names:
                names.append(word)
    return sorted(names)


def url(enclave, service, port):
    u = sh("kurtosis", "port", "print", enclave, service, port)
    if not u:
        return None
    return u if u.startswith("http") else "http://" + u


def get(base, path):
    try:
        with urllib.request.urlopen(base + path, timeout=10) as r:
            return json.load(r)
    except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        return None


def rpc(base, method, params):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    req = urllib.request.Request(base, data=body, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.load(r).get("result")
    except Exception:
        return None


def short(h):
    return h[:12] if h else "?"


def consensus_forks(enclave, cls):
    """Heads each client knows, and the depth of each branch from the common ancestor."""
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

    # One head everywhere and all the same root is the quiet case.
    allroots = {r for hs in heads_by_client.values() for _, r in hs}
    for svc, hs in heads_by_client.items():
        for slot, root in sorted(hs):
            print("  %-30s head slot %-6d %s" % (svc, slot, short(root)))
    if len(allroots) == 1 and all(len(h) == 1 for h in heads_by_client.values()):
        print("  -> one head, no fork in flight")
        return
    print("  -> %d distinct heads: the chain is forked right now" % len(allroots))

    # Depth: walk the fork-choice tree back from each head to where the branches meet.
    #
    # Each head has to be resolved against a client that actually HAS it: a partitioned
    # node's branch is absent from everyone else's dump, which is the whole point of it
    # being a fork.
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
        b = rpc(base, "eth_getBlockByNumber", ["latest", False])
        if b:
            heads[svc] = int(b["number"], 16)
    if len(heads) < 2:
        print("  need at least two execution clients")
        return

    n = min(heads.values())
    roots = {}
    for svc in heads:
        base = url(enclave, svc, "rpc")
        b = rpc(base, "eth_getBlockByNumber", [hex(n), False])
        if b:
            roots[svc] = (b["hash"], b["stateRoot"])
    for svc, (h, sr) in roots.items():
        print("  %-30s block %-6d %s  root %s" % (svc, n, short(h), short(sr)))

    distinct = {h for h, _ in roots.values()}
    if len(distinct) == 1:
        print("  -> all execution clients agree at block %d" % n)
    else:
        print("  -> %d different blocks at height %d: a fork the clients have not resolved" %
              (len(distinct), n))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("enclave", nargs="?", default="pbt")
    ap.add_argument("--via", default=None)
    args = ap.parse_args()

    cls = [args.via] if args.via else services(args.enclave, "cl-")
    els = services(args.enclave, "el-")
    if not cls and not els:
        sys.exit(f"no clients found in enclave '{args.enclave}'")

    consensus_forks(args.enclave, cls)
    execution_forks(args.enclave, els)

    api = url(args.enclave, "disruptoor", "http")
    if api:
        s = get(api, "/v1/state") or {}
        p, sh_ = len(s.get("partitions") or []), len(s.get("shaping") or [])
        if p or sh_:
            print("\n%d partition(s) and %d shaping rule(s) are applied — any fork above is "
                  "deliberate." % (p, sh_))


if __name__ == "__main__":
    main()
