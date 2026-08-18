#!/usr/bin/env python3
"""Say what the devnet is doing and, if it has split, when and alongside what.

verify.py answers "do the tips agree". That is the wrong question once they do not: what
you need is the last block everyone agreed on, the first one they did not, and what the
consensus layer was doing at that moment. Reading those from three places by hand is how
a peer drop and a chain split sat next to each other for an afternoon without being
connected.

Usage: diagnose.py [enclave]
"""
import json, re, subprocess, sys, urllib.request

def sh(*a):
    return subprocess.run(a, capture_output=True, text=True).stdout.strip()

def rpc(url, method, params):
    req = urllib.request.Request(url, method="POST",
        headers={"Content-Type": "application/json"},
        data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode())
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.load(r).get("result")

def get(url):
    with urllib.request.urlopen(url, timeout=20) as r:
        return json.load(r)

def services(enclave, prefix):
    out = []
    for line in sh("kurtosis", "enclave", "inspect", enclave).splitlines():
        parts = line.split()
        if len(parts) > 1 and parts[1].startswith(prefix):
            out.append(parts[1])
    return sorted(set(out))

def endpoint(enclave, svc, port):
    u = sh("kurtosis", "port", "print", enclave, svc, port)
    return u if u.startswith("http") else "http://" + u

def main():
    enclave = sys.argv[1] if len(sys.argv) > 1 else "pbt"
    els = services(enclave, "el-")
    cls = services(enclave, "cl-")
    if not els:
        print(f"no execution clients in '{enclave}'", file=sys.stderr)
        return 2

    print("=== execution layer ===")
    urls, heads = {}, {}
    for n in els:
        urls[n] = endpoint(enclave, n, "rpc")
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
            hs = {n: (rpc(urls[n], "eth_getBlockByNumber", [hex(b), False]) or {}).get("hash") for n in live}
            if len(set(hs.values())) == 1:
                if first_bad is None:
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
            u = endpoint(enclave, c, "http")
            peers = get(f"{u}/eth/v1/node/peer_count")["data"]["connected"]
            head = get(f"{u}/eth/v1/beacon/headers/head")["data"]
            slot = head["header"]["message"]["slot"]
            print(f"  {c:30} peers={peers:<4} head_slot={slot} root={head['root'][:18]}")
        except Exception as e:
            print(f"  {c:30} unreachable: {e}")

    # The correlation this whole script exists for: peers going to zero and the chain
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

    return 0

sys.exit(main())
