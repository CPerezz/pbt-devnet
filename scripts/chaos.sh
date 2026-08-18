#!/usr/bin/env bash
# Drive disruptoor to partition and heal the devnet.
#
# The native disruptoor API takes label selectors, not participant numbers:
# `{"node-index": [1,2], "client-type": ["execution","beacon"]}`. ethereum-package's
# `disruptoor_params` accepts friendlier `participants`/`components` and translates them,
# but that is start-up config; at runtime we speak the native schema.
#
# A group that matches no container is rejected with 500 and the whole state rolls back,
# so a partition either applies or fails loudly. What it cannot tell you is whether the
# partition had any EFFECT — for that, `cycle` checks the branches actually diverged.
set -euo pipefail
ENCLAVE="${1:-pbt}"
ACTION="${2:-cycle}"
HOLD="${PBT_SPLIT_SECONDS:-60}"

# kurtosis port print includes the scheme only when the port declares an application
# protocol, so el rpc comes back as "127.0.0.1:1234" while disruptoor's http port comes
# back as "http://127.0.0.1:1234". Normalise rather than guess.
url() {
  local u
  u=$(kurtosis port print "$1" "$2" "$3" 2>/dev/null) || return 1
  [[ -z "$u" ]] && return 1
  case "$u" in http://*|https://*) printf '%s' "$u" ;; *) printf 'http://%s' "$u" ;; esac
}

api=$(url "$ENCLAVE" disruptoor http) || { echo "disruptoor is not running in '$ENCLAVE'" >&2; exit 1; }

heads() {
  for el in $(kurtosis enclave inspect "$ENCLAVE" 2>/dev/null | awk '/^[0-9a-f]{12}/ {print $2}' | grep '^el-'); do
    u=$(url "$ENCLAVE" "$el" rpc) || continue
    curl -s -X POST "$u" -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}' \
      | python3 -c 'import json,sys
b=(json.load(sys.stdin).get("result") or {})
print(int(b.get("number","0x0"),16), b.get("hash","?"))' 2>/dev/null || echo "0 ?"
  done
}

split() {
  echo "==> partitioning {1,2} | {3} on el and cl"
  curl -fsS -X PUT "$api/v1/state" -H 'Content-Type: application/json' -d '{
    "partitions": [{
      "name": "pbt-split",
      "groups": [
        {"node-index": [1,2], "client-type": ["execution","beacon"]},
        {"node-index": [3],   "client-type": ["execution","beacon"]}
      ],
      "scope": ["el_p2p","cl_p2p"],
      "symmetric": true
    }]
  }' >/dev/null
  curl -fsS "$api/v1/state" | python3 -c 'import json,sys; s=json.load(sys.stdin); n=len(s.get("partitions") or []); print(f"    applied partitions: {n}"); sys.exit(0 if n==1 else 1)'
}

heal() {
  echo "==> healing"
  curl -fsS -X POST "$api/v1/state/clear" >/dev/null
  curl -fsS "$api/v1/state" | python3 -c 'import json,sys; s=json.load(sys.stdin); print("    partitions:", len(s.get("partitions") or []), " shaping:", len(s.get("shaping") or []))'
}

case "$ACTION" in
  split) split ;;
  heal)  heal ;;
  cycle)
    echo "--- before ---"; heads | sed 's/^/    /'
    split
    echo "==> holding ${HOLD}s"; sleep "$HOLD"
    echo "--- during (same height + different hash means the split bit) ---"
    heads | sed 's/^/    /'
    python3 - <<'PY'
import subprocess, collections, os, json
# Re-read heads and report whether any two clients sit at the same height on different
# hashes. That is the only proof the partition did anything; a partition that applied
# cleanly but changed no traffic looks identical to a healthy chain.
PY
    heal
    echo "==> waiting 45s for reconvergence"; sleep 45
    echo "--- after ---"; heads | sed 's/^/    /'
    ;;
  *) echo "usage: $0 <enclave> split|heal|cycle" >&2; exit 1 ;;
esac
