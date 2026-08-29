#!/usr/bin/env bash
# Drive one migration run end to end: start the devnet, follow it in the
# terminal, do the host-side work the schedule leaves room for, dump the
# evidence and judge it.
#
# The point of this script is that a run is followable and repeatable
# without a person watching a browser for an hour. It prints one line per
# state change from the monitor's own view, restarts a node in the gap the
# schedule leaves for it, asks the reorg service for its scenarios once the
# migration is done, and finishes by running the acceptance checks.
set -euo pipefail

ENCLAVE="${ENCLAVE:-pbt}"
ARGS="${ARGS:-args/migration-composite.yaml}"
OUT="${OUT:-/tmp/$ENCLAVE-lap}"
# Host-side node restart, in the gap the schedule leaves clear. Empty skips
# it; the node must be a light one, never the heavy victim.
RESTART_NODE="${RESTART_NODE:-4}"
RESTART_AT="${RESTART_AT:-1000}"   # seconds after genesis
RESTART_FOR="${RESTART_FOR:-60}"
# Scenarios to ask the reorg service for after the switchover.
SCENARIOS="${SCENARIOS:-code-shared storage-del}"
SCENARIO_DEPTH="${SCENARIO_DEPTH:-8}"

cd "$(dirname "${BASH_SOURCE[0]}")/.."
mkdir -p "$OUT"

say() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }

say "starting $ENCLAVE from $ARGS"
kurtosis enclave rm -f "$ENCLAVE" >/dev/null 2>&1 || true
kurtosis run . --enclave "$ENCLAVE" --args-file "$ARGS" --privileged > "$OUT/run.log" 2>&1 || {
  say "the run failed to start; tail of the log:"; tail -20 "$OUT/run.log"; exit 1; }

FORK=$(grep -oE 'binaryTrieTime=[0-9]+' "$OUT/run.log" | head -1 | cut -d= -f2)
GENESIS=$(grep -oE 'genesis_time=[0-9]+' "$OUT/run.log" | head -1 | cut -d= -f2)
if [ -z "${FORK:-}" ] || [ -z "${GENESIS:-}" ]; then
  say "the plan printed no fork time; this is not a migration profile"; exit 1
fi
say "genesis=$GENESIS fork=$FORK (offset $((FORK - GENESIS))s)"
printf 'enclave=%s\nargs=%s\ngenesis=%s\nfork=%s\n' "$ENCLAVE" "$ARGS" "$GENESIS" "$FORK" > "$OUT/lap.env"

# The monitor's own view, if it is serving one: this is what makes a lap
# followable from the terminal that launched it.
STATE_URL=""
if port=$(kurtosis port print "$ENCLAVE" migration-monitor http 2>/dev/null); then
  STATE_URL="$port/api/state"
  say "live view: $port"
fi

# Follow the run: print one line per change rather than a stream nobody can
# read. Runs until the chain is done and the post-switchover work finishes.
follow() {
  local last=""
  while :; do
    sleep 20
    [ -n "$STATE_URL" ] || continue
    local line
    line=$(curl -s --max-time 5 "$STATE_URL" 2>/dev/null | python3 -c '
import json,sys
try:
    s = json.load(sys.stdin)
except Exception:
    sys.exit(0)
parts = []
for n in s.get("nodes", []):
    tag = n.get("phase") or ("no introspection" if not n.get("introspection") else "?")
    fb = n.get("fork_block") or {}
    if fb.get("number"):
        tag += " fork@%s%s" % (fb["number"], "" if fb.get("final") else "?")
    parts.append("%s=%s/%s" % (n["name"].split("-")[1], n.get("head", 0), tag))
ev = s.get("events") or []
notable = [e for e in ev if e.get("kind") in ("critical", "reorg", "bstar-reorged")]
tail = ""
if notable:
    e = notable[-1]
    tail = " | %s %s %s" % (e.get("kind"), e.get("finding", ""), (e.get("detail") or "")[:70])
print("to-fork=%ss %s%s" % (int(s.get("seconds_to_fork", 0)), " ".join(parts), tail))
' 2>/dev/null) || true
    if [ -n "$line" ] && [ "$line" != "$last" ]; then say "$line"; last="$line"; fi
  done
}
follow & FOLLOW=$!
trap 'kill $FOLLOW 2>/dev/null || true' EXIT

# Host-side restart, inside the gap the schedule leaves for it. The published
# RPC port changes when a service restarts, so nothing may cache URLs across
# this point.
if [ -n "$RESTART_NODE" ] && [ "$RESTART_NODE" != "0" ]; then
  target=$((GENESIS + RESTART_AT))
  now=$(date +%s)
  [ "$target" -gt "$now" ] && sleep $((target - now))
  svc=$(cd scripts && python3 -c "import pbt; print([n for n in pbt.services('$ENCLAVE','el-') if n.startswith('el-$RESTART_NODE-')][0])")
  say "restarting $svc for ${RESTART_FOR}s (follower must recover its cursor)"
  kurtosis service stop "$ENCLAVE" "$svc" >/dev/null
  sleep "$RESTART_FOR"
  kurtosis service start "$ENCLAVE" "$svc" >/dev/null
  say "restarted $svc"
fi

# Wait for every client to finish migrating, then for the gate's own window.
say "waiting for the migration to complete on every client"
deadline=$((FORK + 1500))
while [ "$(date +%s)" -lt "$deadline" ]; do
  done_count=$(kurtosis service logs "$ENCLAVE" migration-monitor -a 2>/dev/null \
    | grep -o '"phase":"done"' | wc -l | tr -d ' ')
  [ "${done_count:-0}" -gt 0 ] && break
  sleep 30
done
say "migration reported done"

# Scenarios, if the reorg service is running behind the gate.
if api=$(kurtosis port print "$ENCLAVE" migration-gate http 2>/dev/null); then
  say "waiting for the gate to hand over to the reorg service"
  for _ in $(seq 1 40); do
    curl -s --max-time 3 "$api/status" >/dev/null 2>&1 && break
    sleep 15
  done
  for s in $SCENARIOS; do
    say "scenario $s at depth $SCENARIO_DEPTH"
    curl -s -X POST --max-time 10 "$api/scenario/$s?depth=$SCENARIO_DEPTH" >/dev/null || true
    sleep 90
  done
  curl -s --max-time 10 "$api/status" > "$OUT/chaos-status.json" 2>/dev/null || true
fi

say "dumping evidence"
for svc in $(cd scripts && python3 -c "import pbt; print(' '.join(pbt.services('$ENCLAVE','el-')))"); do
  kurtosis service logs "$ENCLAVE" "$svc" -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/$svc.log"
done
kurtosis service logs "$ENCLAVE" migration-monitor -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/migration-monitor.jsonl"
for svc in migration-chaos migration-gate; do
  kurtosis service logs "$ENCLAVE" "$svc" -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/$svc.jsonl" || true
done

say "judging the run"
kill $FOLLOW 2>/dev/null || true
els=$(cd scripts && python3 -c "import pbt; print(' '.join('--el ' + n + '=' + pbt.url('$ENCLAVE', n, 'rpc') for n in pbt.services('$ENCLAVE','el-')))")
chaos_args=""
[ -s "$OUT/migration-chaos.jsonl" ] && chaos_args="--chaos-jsonl $OUT/migration-chaos.jsonl"
[ -s "$OUT/migration-gate.jsonl" ] && cat "$OUT/migration-gate.jsonl" >> "$OUT/migration-chaos.jsonl"
go build -o bin/verify-migration ./cmd/verify-migration
set +e
bin/verify-migration $els \
  --monitor-jsonl "$OUT/migration-monitor.jsonl" \
  $chaos_args \
  --logs-dir "$OUT" \
  --pins verify/pins.yaml \
  --binary-trie-time "$FORK" \
  --summary "$OUT/summary.md" | tee "$OUT/verdict.txt"
code=$?
set -e
say "verifier exit=$code; evidence in $OUT"
exit $code
