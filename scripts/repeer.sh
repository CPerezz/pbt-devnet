#!/usr/bin/env bash
# Restart any consensus client that has been left with no peers.
#
# Partitions cut TCP sessions, and lighthouse does not reliably rebuild its peer set when
# they come back: it sits at the same slot as everyone else, so it never measures itself
# as behind and never range-syncs. Its discovery table is empty and nothing refills it.
#
# A container restart rebuilds the table from the bootnode and the node rejoins in a slot
# or two. This is a repair for the devnet, not something the clients should need -- run
# it after a long partition, or when `make diagnose` shows a client stuck behind.
set -euo pipefail
ENCLAVE="${1:-pbt}"
restarted=0

for svc in $(kurtosis enclave inspect "$ENCLAVE" 2>/dev/null | awk '/^[0-9a-f]{12}/ {print $2}' | grep '^cl-'); do
  url=$(kurtosis port print "$ENCLAVE" "$svc" http 2>/dev/null) || continue
  peers=$(curl -s --max-time 5 "$url/eth/v1/node/peer_count" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["connected"])' 2>/dev/null || echo "?")
  if [[ "$peers" == "0" ]]; then
    cid=$(docker ps --filter "label=kurtosis_service_name=$svc" \
                    --filter "label=kurtosis_enclave_name=$ENCLAVE" --format '{{.ID}}' | head -1)
    if [[ -n "$cid" ]]; then
      echo "==> $svc has no peers; restarting"
      docker restart "$cid" >/dev/null
      restarted=$((restarted + 1))
    fi
  else
    echo "    $svc peers=$peers"
  fi
done

if [[ "$restarted" -eq 0 ]]; then
  echo "nothing to do: every consensus client has peers"
else
  echo "restarted $restarted; give them a slot or two, then: scripts/diagnose.py $ENCLAVE"
fi
