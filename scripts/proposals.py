#!/usr/bin/env python3
"""Attribute every slot to the node that was due to propose it, and report who missed.

This exists because "besu orphans a lot of blocks" turned out to be false, and nothing in
the repo could show that. Reading Dora, besu appeared to miss most of its slots; measured
here over a window with no disruption, it misses about as often as geth. The difference
was that every reorg scenario used to strand besu, so its blocks never reached the node
being asked.

Two things make the number trustworthy:

  * Duties are read per epoch and mapped to a participant by validator index, since
    ethereum-package hands out sequential ranges of `num_validator_keys_per_node`.
  * A slot is only counted as missed if the block is absent from a node that was NOT
    partitioned. Asking a majority node about a minority node's slots during a partition
    measures the partition, not the proposer.

Usage:
  scripts/proposals.py [enclave] [--epochs N] [--from-epoch N] [--via cl-2-lighthouse-geth]
"""
import argparse
import json
import subprocess
import sys
import urllib.error
import urllib.request

SLOTS_PER_EPOCH = 32


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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("enclave", nargs="?", default="pbt")
    ap.add_argument("--epochs", type=int, default=4, help="how many epochs back to measure")
    ap.add_argument("--from-epoch", type=int, default=None, help="start here instead")
    ap.add_argument("--via", default=None, help="which consensus client to ask")
    ap.add_argument("--validators-per-node", type=int, default=128)
    args = ap.parse_args()

    cls = services(args.enclave, "cl-")
    if not cls:
        sys.exit(f"no consensus clients found in enclave '{args.enclave}'")
    via = args.via or cls[1] if len(cls) > 1 else cls[0]
    base = url(args.enclave, via, "http")
    if not base:
        sys.exit(f"could not reach {via}")

    head_hdr = get(base, "/eth/v1/beacon/headers/head")
    if not head_hdr:
        sys.exit(f"{via} did not answer")
    head = int(head_hdr["data"]["header"]["message"]["slot"])
    head_epoch = head // SLOTS_PER_EPOCH

    first = args.from_epoch if args.from_epoch is not None else max(0, head_epoch - args.epochs)
    names = {i + 1: n for i, n in enumerate(services(args.enclave, "el-"))}

    stats = {n: {"due": 0, "missed": 0, "slots": []} for n in names}
    unmapped = 0
    got, asked = [], list(range(first, head_epoch + 1))
    for epoch in asked:
        duties = get(base, f"/eth/v1/validator/duties/proposer/{epoch}")
        if not duties:
            # The beacon API only serves duties for a narrow recent window; older epochs
            # 404. Skipping them silently once made 2 epochs of data print as 151.
            continue
        got.append(epoch)
        for duty in duties["data"]:
            slot = int(duty["slot"])
            if slot == 0 or slot > head:
                continue
            node = int(duty["validator_index"]) // args.validators_per_node + 1
            if node not in stats:
                unmapped += 1
                continue
            stats[node]["due"] += 1
            if get(base, f"/eth/v1/beacon/headers/{slot}") is None:
                stats[node]["missed"] += 1
                stats[node]["slots"].append(slot)

    if not got:
        sys.exit(f"{via} served no proposer duties for epochs {first}..{head_epoch}")
    print(f"epochs {got[0]}..{got[-1]} ({len(got)} of the {len(asked)} asked for; the beacon API "
          f"only keeps recent duties), slots up to {head}, as seen by {via}\n")
    print(f"{'proposer':30} {'due':>5} {'missed':>7} {'miss %':>8}   missed slots")
    worst = 0.0
    for node, name in names.items():
        s = stats[node]
        pct = 100 * s["missed"] / s["due"] if s["due"] else 0.0
        worst = max(worst, pct)
        shown = ", ".join(str(x) for x in s["slots"][:8]) + ("…" if len(s["slots"]) > 8 else "")
        print(f"{name:30} {s['due']:5} {s['missed']:7} {pct:7.1f}%   {shown}")

    if unmapped:
        print(f"\n{unmapped} duties fell outside the known nodes — is --validators-per-node right?")

    # A partition in flight makes this meaningless, so say so rather than report a number
    # that will be read as a client defect.
    api = url(args.enclave, "disruptoor", "http")
    if api:
        state = get(api, "/v1/state") or {}
        if state.get("partitions") or state.get("shaping"):
            print("\nWARNING: disruptoor has state applied. A partitioned node's blocks do not")
            print("reach the node being asked, so these misses measure the partition, not the")
            print("proposer. Re-run with chaos disabled for a baseline.")
            return
    if worst > 25:
        print(f"\nWorst miss rate is {worst:.1f}%. With no disruption applied that is worth")
        print("investigating: check the proposer's CL for block production errors.")


if __name__ == "__main__":
    main()
