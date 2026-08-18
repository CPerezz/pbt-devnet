#!/usr/bin/env bash
# Talk to pbtchaos, which owns every disruption on this devnet.
#
# pbtchaos runs continuously and produces a one-block reorg every 15-30 blocks on its
# own; this script is for asking it to run a named scenario now, and for the manual
# split/heal used when poking at the network by hand.
#
# split/heal go straight to disruptoor rather than through pbtchaos, so they are
# deliberately outside its queue: use them for exploration, not alongside a scenario.
set -euo pipefail
ENCLAVE="${1:-pbt}"
ACTION="${2:-status}"

# kurtosis port print includes the scheme only when the port declares an application
# protocol, so normalise rather than guess.
url() {
  local u
  u=$(kurtosis port print "$1" "$2" "$3" 2>/dev/null) || return 1
  [[ -z "$u" ]] && return 1
  case "$u" in http://*|https://*) printf '%s' "$u" ;; *) printf 'http://%s' "$u" ;; esac
}

pretty() { python3 -m json.tool 2>/dev/null || cat; }

case "$ACTION" in
  status)
    api=$(url "$ENCLAVE" pbtchaos http) || { echo "pbtchaos is not running in '$ENCLAVE'" >&2; exit 1; }
    curl -fsS "$api/status" | pretty
    ;;
  scenario)
    name="${3:-}"
    depth="${4:-}"
    [[ -z "$name" ]] && { echo "usage: $0 <enclave> scenario <name> [depth]" >&2; exit 1; }
    api=$(url "$ENCLAVE" pbtchaos http) || { echo "pbtchaos is not running in '$ENCLAVE'" >&2; exit 1; }
    q=""; [[ -n "$depth" ]] && q="?depth=$depth"
    curl -fsS -X POST "$api/scenario/$name$q" | pretty
    echo "queued. watch it with: make chaos-status, or kurtosis service logs $ENCLAVE pbtchaos -f"
    ;;
  split)
    api=$(url "$ENCLAVE" disruptoor http) || { echo "disruptoor is not running in '$ENCLAVE'" >&2; exit 1; }
    # The native API takes label selectors, and a group matching no container is
    # rejected with 500 rather than silently applying nothing.
    n=$(kurtosis enclave inspect "$ENCLAVE" 2>/dev/null | grep -cE '^[0-9a-f]{12}.*[[:space:]]el-[0-9]+-' || true)
    [[ "$n" -lt 2 ]] && { echo "need at least two execution clients, found $n" >&2; exit 1; }
    majority=$(seq 1 $((n - 1)) | paste -sd, -)
    echo "==> partitioning {$majority} | {$n} on el and cl"
    curl -fsS -X PUT "$api/v1/state" -H 'Content-Type: application/json' -d "{
      \"partitions\": [{
        \"name\": \"pbt-split\",
        \"groups\": [
          {\"node-index\": [$majority], \"client-type\": [\"execution\",\"beacon\"]},
          {\"node-index\": [$n],        \"client-type\": [\"execution\",\"beacon\"]}
        ],
        \"scope\": [\"el_p2p\",\"cl_p2p\"],
        \"symmetric\": true
      }]
    }" >/dev/null
    echo "    applied. heal it with: $0 $ENCLAVE heal"
    ;;
  heal)
    api=$(url "$ENCLAVE" disruptoor http) || { echo "disruptoor is not running in '$ENCLAVE'" >&2; exit 1; }
    curl -fsS -X POST "$api/v1/state/clear" >/dev/null
    echo "==> healed. Check reconvergence with scripts/diagnose.py $ENCLAVE"
    ;;
  *)
    echo "usage: $0 <enclave> status|scenario <name> [depth]|split|heal" >&2
    exit 1
    ;;
esac
