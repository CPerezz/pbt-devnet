#!/usr/bin/env python3
"""Compare every execution client at the SAME block number, not at their own heads.

Heads legitimately differ by a block or two, so comparing tips reports divergences that
are not there. This walks a fixed range and asks every client for the same number.

Usage: verify.py <enclave> [--blocks N] [--wait]
"""
import json, subprocess, sys, time, urllib.request

def sh(*a):
    return subprocess.run(a, capture_output=True, text=True).stdout.strip()

def rpc(url, method, params):
    req = urllib.request.Request(url, method="POST",
        headers={"Content-Type": "application/json"},
        data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode())
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.load(r).get("result")

def main():
    enclave = sys.argv[1] if len(sys.argv) > 1 else "pbt"
    target = 100
    wait = False
    for i, a in enumerate(sys.argv):
        if a == "--blocks":
            target = int(sys.argv[i + 1])
        if a == "--wait":
            wait = True

    names = [l.split()[1] for l in sh("kurtosis", "enclave", "inspect", enclave).splitlines()
             if l[:12].isalnum() and len(l.split()) > 1 and l.split()[1].startswith("el-")]
    if len(names) < 2:
        print(f"need at least two execution clients, found {names}", file=sys.stderr)
        return 2
    def endpoint(n):
        # port print includes the scheme only for ports that declare an application
        # protocol, so add it only when it is missing.
        u = sh("kurtosis", "port", "print", enclave, n, "rpc")
        return u if u.startswith("http") else "http://" + u

    urls = {n: endpoint(n) for n in names}
    print("clients:", ", ".join(names))

    def head(n):
        try:
            return int(rpc(urls[n], "eth_blockNumber", []), 16)
        except Exception:
            return 0

    if wait:
        print(f"waiting for every client to reach block {target} ...")
        last = -1
        while True:
            hs = {n: head(n) for n in names}
            low = min(hs.values())
            if low != last:
                print("   " + "  ".join(f"{n.split('-')[1]}{n.split('-')[2][:4]}={h}" for n, h in hs.items()))
                last = low
            if low >= target:
                break
            time.sleep(10)

    low = min(head(n) for n in names)
    if low < 1:
        print("no blocks produced yet", file=sys.stderr)
        return 2

    # Compare the MOST RECENT `target` blocks, not blocks 1..target.
    #
    # A divergence is permanent once it happens, so what matters is the tip: early
    # agreeing blocks would otherwise outvote a chain that has been split for an hour.
    top = low
    bottom = max(1, top - target + 1)
    print(f"comparing blocks {bottom}..{top} (the most recent {top - bottom + 1}) across {len(names)} clients")

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

sys.exit(main())
