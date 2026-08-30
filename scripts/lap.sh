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
# Scenarios to ask the reorg service for after the switchover: the full
# at-genesis suite by default, because phase 3's contract is "everything the
# at-genesis devnet tested, now on the post-fork tree".
SCENARIOS="${SCENARIOS:-code-sole code-shared delegate account storage-add storage-del}"
SCENARIO_DEPTH="${SCENARIO_DEPTH:-8}"
# The reorg service's API port inside the enclave, matching main.star.
CHAOS_PORT="${CHAOS_PORT:-7800}"

# Optional heavy-victim override: rewrites pbt_migration.heavy_node into a
# temp copy of the args file, so victim placement is a lap parameter instead
# of a hardcoded participant. Empty keeps the args file's own value.
HEAVY="${HEAVY:-}"
cd "$(dirname "${BASH_SOURCE[0]}")/.."
mkdir -p "$OUT"

say() { printf '%s  %s\n' "$(date +%H:%M:%S)" "$*"; }

say "starting $ENCLAVE from $ARGS"
if [ -n "$HEAVY" ]; then
  python3 - "$ARGS" "$OUT/args-heavy.yaml" "$HEAVY" <<'PYEOF'
import re, sys
src, dst, heavy = sys.argv[1], sys.argv[2], int(sys.argv[3])
t = open(src).read()
if re.search(r'^(\s*)heavy_node:\s*\d+', t, re.M):
    t = re.sub(r'^(\s*)heavy_node:\s*\d+', r'\g<1>heavy_node: ' + str(heavy), t, flags=re.M)
elif re.search(r'^pbt_migration:\s*$', t, re.M):
    t = re.sub(r'^pbt_migration:\s*$', 'pbt_migration:\n  heavy_node: ' + str(heavy), t, flags=re.M)
else:
    raise SystemExit(src + " has no pbt_migration block to set heavy_node in")
open(dst, 'w').write(t)
PYEOF
  ARGS="$OUT/args-heavy.yaml"
  say "heavy victim overridden to participant $HEAVY (args copy at $ARGS)"
fi
kurtosis enclave rm -f "$ENCLAVE" >/dev/null 2>&1 || true
kurtosis run . --enclave "$ENCLAVE" --args-file "$ARGS" --privileged > "$OUT/run.log" 2>&1 || {
  say "the run failed to start; tail of the log:"; tail -20 "$OUT/run.log"; exit 1; }

FORK=$( { grep -oE 'binaryTrieTime=[0-9]+' "$OUT/run.log" || true; } | head -1 | cut -d= -f2)
GENESIS=$( { grep -oE 'genesis_time=[0-9]+' "$OUT/run.log" || true; } | head -1 | cut -d= -f2)
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
restart_at_unix=""
if [ -n "$RESTART_NODE" ] && [ "$RESTART_NODE" != "0" ]; then
  target=$((GENESIS + RESTART_AT))
  now=$(date +%s)
  if [ "$target" -gt "$now" ]; then sleep $((target - now)); fi
  svc=$(cd scripts && python3 -c "import pbt; print([n for n in pbt.services('$ENCLAVE','el-') if n.startswith('el-$RESTART_NODE-')][0])")
  say "restarting $svc for ${RESTART_FOR}s (follower must recover its cursor)"
  restart_at_unix=$(date +%s)
  kurtosis service stop "$ENCLAVE" "$svc" >/dev/null
  sleep "$RESTART_FOR"
  kurtosis service start "$ENCLAVE" "$svc" >/dev/null
  say "restarted $svc"
fi

# Wait for every client to finish migrating, then for the gate's own window.
say "waiting for the migration to complete on every client"
deadline=$((FORK + 1500))
done_seen=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  done_count=$( { kurtosis service logs "$ENCLAVE" migration-monitor -a 2>/dev/null \
    | grep -c '"phase":"done"' || true; } | tr -d ' ')
  if [ "${done_count:-0}" -gt 0 ]; then done_seen=1; break; fi
  sleep 30
done
if [ "$done_seen" = 1 ]; then
  say "migration reported done"
else
  say "no client reported the migration done before the deadline; judging anyway"
fi

# Scenarios, if the reorg service is running behind the gate. Its API is
# reachable only inside the enclave: the gate declares no ports, because
# kurtosis would wait for one to open and nothing listens there until the
# handover. So ask from inside the container.
gate_api() {
  kurtosis service exec "$ENCLAVE" migration-gate \
    "wget -qO- --timeout=5 $1 2>/dev/null" 2>/dev/null | tail -n +2
}
gate_api_post() {
  kurtosis service exec "$ENCLAVE" migration-gate \
    "wget -qO- --timeout=10 --post-data= $1 2>/dev/null" 2>/dev/null | tail -n +2
}
handed_over=0
scenario_results="[]"
quiesced_at=0
if kurtosis service inspect "$ENCLAVE" migration-gate >/dev/null 2>&1; then
  say "waiting for the gate to hand over to the reorg service"
  for _ in $(seq 1 60); do
    if kurtosis service logs "$ENCLAVE" migration-gate -a 2>/dev/null \
      | grep -q 'control API listening'; then handed_over=1; break; fi
    sleep 15
  done
  if [ "$handed_over" = 1 ]; then
    done_scenarios=""
    for s in $SCENARIOS; do
      say "scenario $s at depth $SCENARIO_DEPTH"
      kurtosis service exec "$ENCLAVE" migration-gate \
        "wget -qO- --timeout=10 --post-data= 'http://127.0.0.1:$CHAOS_PORT/scenario/$s?depth=$SCENARIO_DEPTH'" >/dev/null 2>&1 || true
      # Wait for THIS scenario to finish rather than sleeping a guess:
      # pbtchaos serialises its jobs, so the next POST would queue anyway,
      # and the verifier judges recorded outcomes, not fire-and-forget.
      for _ in $(seq 1 40); do
        sleep 10
        n=$(gate_api "http://127.0.0.1:$CHAOS_PORT/status" 2>/dev/null \
          | python3 -c "import json,sys;d=json.load(sys.stdin);print(len(d.get('results',[])))" 2>/dev/null || echo 0)
        if [ "${n:-0}" -ge "$(( $(echo "$done_scenarios" | wc -w) + 1 ))" ]; then break; fi
      done
      done_scenarios="${done_scenarios:-} $s"
    done
    # Quiesce the cadence before judging: an end state sampled under live
    # partitions measures the chaos driver, not the clients.
    gate_api_post "http://127.0.0.1:$CHAOS_PORT/quiesce" >/dev/null 2>&1 || true
    quiesced_at=$(date +%s)
    say "reorg service quiesced; letting the network settle"
    sleep 45
    gate_api "http://127.0.0.1:$CHAOS_PORT/status" > "$OUT/chaos-status.json" 2>/dev/null || true
    scenario_results=$(python3 - "$OUT/chaos-status.json" <<'PYEOF'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("[]"); raise SystemExit
out = [{"name": r.get("scenario") or r.get("name") or "", "outcome": r.get("outcome", "")}
       for r in d.get("results", [])]
print(json.dumps(out))
PYEOF
)
  else
    say "the gate never handed over; skipping scenarios"
  fi
fi

say "dumping evidence"
for svc in $(cd scripts && python3 -c "import pbt; print(' '.join(pbt.services('$ENCLAVE','el-')))"); do
  kurtosis service logs "$ENCLAVE" "$svc" -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/$svc.log"
done
kurtosis service logs "$ENCLAVE" migration-monitor -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/migration-monitor.jsonl"
for svc in migration-chaos migration-gate; do
  kurtosis service logs "$ENCLAVE" "$svc" -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/$svc.jsonl" || true
done

# The manifest records what this driver DID - restart, scenarios and their
# recorded outcomes, quiesce - so the verifier can fail a lap whose steps
# silently never ran instead of judging a thinner run green.
python3 - "$OUT/manifest.json" "$ARGS" "${RESTART_NODE:-0}" "${restart_at_unix:-0}" "${handed_over}" "${quiesced_at}" "${scenario_results}" <<'PYEOF'
import json, re, sys
out, args_file, rnode, rat, handed, quiesced, scenarios = sys.argv[1:8]
profile = ""
m = re.search(r'chaos_profile:\s*"?([a-z-]+)"?', open(args_file).read())
if m:
    profile = m.group(1)
manifest = {
    "profile": profile,
    "restart": {"node": int(rnode), "at": int(rat)} if int(rat) else None,
    "scenarios": json.loads(scenarios),
    "handover_expected": handed == "1",
    "quiesced_at": int(quiesced),
}
json.dump(manifest, open(out, "w"))
PYEOF

say "judging the run"
kill $FOLLOW 2>/dev/null || true
els=$(cd scripts && python3 -c "import pbt; print(' '.join('--el ' + n + '=' + pbt.url('$ENCLAVE', n, 'rpc') for n in pbt.services('$ENCLAVE','el-')))")
chaos_args=""
[ -s "$OUT/migration-chaos.jsonl" ] && chaos_args="--chaos-jsonl $OUT/migration-chaos.jsonl"
if [ -s "$OUT/migration-gate.jsonl" ]; then
  grep '^{' "$OUT/migration-gate.jsonl" >> "$OUT/migration-chaos.jsonl" || true
fi
go build -o bin/verify-migration ./cmd/verify-migration
set +e
bin/verify-migration $els \
  --monitor-jsonl "$OUT/migration-monitor.jsonl" \
  $chaos_args \
  --logs-dir "$OUT" \
  --pins verify/pins.yaml \
  --binary-trie-time "$FORK" \
  --manifest "$OUT/manifest.json" \
  --summary "$OUT/summary.md" | tee "$OUT/verdict.txt"
code=$?
set -e
say "verifier exit=$code; evidence in $OUT"
exit $code
