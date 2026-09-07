#!/usr/bin/env bash
# Drive one migration run end to end: start, restart a node in the schedule's
# gap, run the post-switchover scenarios, quiesce, dump evidence, judge.
set -euo pipefail

ENCLAVE="${ENCLAVE:-pbt}"
ARGS="${ARGS:-args/migration-composite.yaml}"
OUT="${OUT:-/tmp/$ENCLAVE-lap}"
# A consensus client the gate reports starved of peers outside any scheduled
# partition is restarted, once per episode - the same remedy as `make repeer`,
# but only when the network is supposed to be whole, so a partition victim at
# zero peers is never mistaken for starvation.
watch_starved() {
  local seen=""
  while :; do
    sleep 30
    for svc in $(kurtosis service logs "$ENCLAVE" migration-gate -a 2>/dev/null \
        | grep '"finding":"cl-starved"' | grep -oE '"node":"[^"]+"' | cut -d'"' -f4 | sort -u); do
      case " $seen " in *" $svc "*) continue;; esac
      seen="$seen $svc"
      cid=$(docker ps --filter "label=kurtosis_service_name=$svc" --filter "label=kurtosis_enclave_name=$ENCLAVE" --format '{{.ID}}' | head -1)
      [ -n "$cid" ] || continue
      say "$svc has no peers outside any partition; restarting it"
      docker restart "$cid" >/dev/null 2>&1 || say "restart of $svc failed"
    done
  done
}

# Host-side node restart in the schedule's gap; 0 skips it. Never the heavy victim.
RESTART_NODE="${RESTART_NODE:-4}"
RESTART_AT="${RESTART_AT:-1000}"   # seconds after genesis
RESTART_FOR="${RESTART_FOR:-60}"
# Post-switchover scenarios: the full at-genesis suite by default.
SCENARIOS="${SCENARIOS:-code-sole code-shared delegate account storage-add storage-del}"
SCENARIO_DEPTH="${SCENARIO_DEPTH:-8}"
# The reorg service's API port inside the enclave, matching main.star.
CHAOS_PORT="${CHAOS_PORT:-7800}"

# HEAVY=<n> rewrites pbt_migration.heavy_node into a temp copy of the args file.
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

if port=$(kurtosis port print "$ENCLAVE" migration-monitor http 2>/dev/null); then
  say "live view: $port"
fi
watch_starved & WATCH=$!
trap 'kill $WATCH 2>/dev/null || true' EXIT


# The published RPC port changes across a restart; never cache URLs past this point.
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

# The gate declares no port (nothing listens until handover), so ask from inside it.
# kurtosis exec banners on stderr; the payload is one json line on stdout - keep json-shaped lines only.
gate_api() {
  kurtosis service exec "$ENCLAVE" migration-gate \
    "wget -qO- --timeout=5 $1 2>/dev/null" 2>/dev/null | grep -E '^\s*[\[{]' || true
}
gate_api_post() {
  kurtosis service exec "$ENCLAVE" migration-gate \
    "wget -qO- --timeout=10 --post-data= $1 2>/dev/null" 2>/dev/null | grep -E '^\s*[\[{]' || true
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
    for s in $SCENARIOS; do
      say "scenario $s at depth $SCENARIO_DEPTH"
      kurtosis service exec "$ENCLAVE" migration-gate \
        "wget -qO- --timeout=10 --post-data= 'http://127.0.0.1:$CHAOS_PORT/scenario/$s?depth=$SCENARIO_DEPTH'" >/dev/null 2>&1 || true
      # Wait for this scenario's own outcome; the verifier judges recorded outcomes.
      for _ in $(seq 1 40); do
        sleep 10
        seen=$(gate_api "http://127.0.0.1:$CHAOS_PORT/status" 2>/dev/null \
          | python3 -c "import json,sys;d=json.load(sys.stdin);print(sum(1 for r in d.get('history',[]) if r.get('name')=='$s'))" 2>/dev/null || echo 0)
        if [ "${seen:-0}" -ge 1 ]; then break; fi
      done
    done
    # Quiesce before judging: end state under live partitions measures the chaos driver.
    gate_api_post "http://127.0.0.1:$CHAOS_PORT/quiesce" >/dev/null 2>&1 || true
    quiesced_at=$(date +%s)
    say "reorg service quiesced; letting the network settle"
    sleep 45
    gate_api "http://127.0.0.1:$CHAOS_PORT/status" > "$OUT/chaos-status.json" 2>/dev/null || true
    scenario_results=$(python3 - "$OUT/chaos-status.json" "$SCENARIOS" <<'PYEOF'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("[]"); raise SystemExit
# history holds every job incl. cadence ops; only requested scenarios count, last run wins.
wanted = sys.argv[2].split()
last = {}
for r in d.get("history", []):
    if r.get("name") in wanted:
        last[r["name"]] = {"name": r["name"], "outcome": r.get("outcome", "")}
print(json.dumps([last[n] for n in wanted if n in last]))
PYEOF
)
  else
    say "the gate never handed over; skipping scenarios"
  fi
fi

say "dumping evidence"
for svc in $(cd scripts && python3 -c "import pbt; print(' '.join(pbt.services('$ENCLAVE','el-') + pbt.services('$ENCLAVE','cl-')))"); do
  kurtosis service logs "$ENCLAVE" "$svc" -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/$svc.log"
done
kurtosis service logs "$ENCLAVE" migration-monitor -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/migration-monitor.jsonl"
for svc in migration-chaos migration-gate; do
  kurtosis service logs "$ENCLAVE" "$svc" -a 2>/dev/null | sed 's/^\[[^]]*\] //' > "$OUT/$svc.jsonl" || true
done

# The manifest records what this driver did, for the verifier to reconcile.
python3 - "$OUT/manifest.json" "$ARGS" "${RESTART_NODE:-0}" "${restart_at_unix:-0}" "${quiesced_at}" "${scenario_results}" <<'PYEOF'
import json, re, sys
out, args_file, rnode, rat, quiesced, scenarios = sys.argv[1:7]
args = open(args_file).read()
m = re.search(r'chaos_profile:\s*"?([a-z-]+)"?', args)
manifest = {
    "profile": m.group(1) if m else "",
    "restart": {"node": int(rnode), "at": int(rat)} if int(rat) else None,
    "scenarios": json.loads(scenarios),
    # a profile that configures the gate must hand over; whether it did is the verifier's question
    "handover_expected": re.search(r'^\s*gate:\s*true', args, re.M) is not None,
    "quiesced_at": int(quiesced),
}
json.dump(manifest, open(out, "w"))
PYEOF

say "judging the run"
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
