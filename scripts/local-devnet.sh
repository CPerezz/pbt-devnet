#!/usr/bin/env bash
# Run the devnet directly on the host, no Docker and no Kurtosis.
#
# This is the fast iteration loop. Kurtosis adds container and image plumbing that is
# noise while you are changing the driver; main.star runs the same thing in an enclave.
#
# Geth-only by design: it launches $PBT_GETH binaries directly. Multi-client runs go
# through Kurtosis, where each client is an image rather than a local binary.
#
# Usage:
#   scripts/local-devnet.sh start            # PBT_NODES=2 by default
#   PBT_NODES=3 scripts/local-devnet.sh start
#   scripts/local-devnet.sh driver --self-test
#   scripts/local-devnet.sh driver --slot-time 3s --slots 20 --reorg-every 6
#   scripts/local-devnet.sh hammer --for 120s
#   scripts/local-devnet.sh logs 60
#   scripts/local-devnet.sh stop
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN="${PBT_RUN_DIR:-/tmp/pbt-local}"
GETH="${PBT_GETH:-/tmp/pbtgeth}"
DRIVER="${PBT_DRIVER:-/tmp/pbtdriver}"
HAMMER="${PBT_HAMMER:-/tmp/pbthammer}"
NODES="${PBT_NODES:-2}"

# Node i gets its own port block so any number of clients can coexist.
http_port() { echo $((8545 + $1 * 100)); }
auth_port() { echo $((8551 + $1 * 100)); }
p2p_port()  { echo $((30411 + $1)); }

common_flags() {
  # Three of these are not negotiable on this branch:
  #   --state.scheme=path  hashdb is refused outright for the binary tree
  #   --syncmode=full      pathdb refuses snap sync (flat state can't be rebuilt)
  #   --override.genesis   PBT comes only from genesis JSON; there is no --override.pbt,
  #                        and this is how ethereum-package launches geth too
  # Never --vmwitnessstats (refused) or --dev (cannot be PBT).
  echo "--state.scheme=path"
  echo "--syncmode=full"
  echo "--override.genesis=$RUN/genesis.json"
  echo "--authrpc.jwtsecret=$RUN/jwtsecret"
  echo "--authrpc.addr=127.0.0.1"
  echo "--authrpc.vhosts=*"
  echo "--http"
  echo "--http.addr=127.0.0.1"
  echo "--http.api=eth,net,web3,debug,txpool"
  echo "--nodiscover"
  echo "--maxpeers=0"
  echo "--verbosity=3"
  echo "--rpc.allow-unprotected-txs"
}

# Deliberately different profiles: identical binaries with identical config agree by
# construction and prove nothing.
profile_flags() {
  case "$1" in
    0) echo "--cache=512";  echo "--cache.trie=10" ;;
    1) echo "--cache=3072"; echo "--cache.trie=40"; echo "--gcmode=archive"; echo "--cache.preimages" ;;
    *) echo "--cache=1024"; echo "--cache.trie=25" ;;
  esac
}

start() {
  [[ -x "$GETH"   ]] || { echo "no geth at $GETH"     >&2; exit 1; }
  [[ -x "$DRIVER" ]] || { echo "no driver at $DRIVER" >&2; exit 1; }
  (( NODES >= 2 )) || { echo "PBT_NODES must be >= 2 (need someone to disagree)" >&2; exit 1; }

  rm -rf "$RUN"
  mkdir -p "$RUN/artifacts"
  openssl rand -hex 32 > "$RUN/jwtsecret"
  cp "$ROOT/genesis/genesis.json" "$RUN/genesis.json"

  mapfile -t COMMON < <(common_flags)
  for i in $(seq 0 $((NODES - 1))); do
    mkdir -p "$RUN/n$i"
    mapfile -t PROFILE < <(profile_flags "$i")
    "$GETH" --datadir "$RUN/n$i" "${COMMON[@]}" "${PROFILE[@]}" \
      --http.port="$(http_port "$i")" --authrpc.port="$(auth_port "$i")" --port="$(p2p_port "$i")" \
      > "$RUN/n$i.log" 2>&1 &
    echo "geth-$i pid $! (http $(http_port "$i"), engine $(auth_port "$i"))"
  done
}

el_args() {
  for i in $(seq 0 $((NODES - 1))); do
    printf -- "--el\ngeth-%s=http://127.0.0.1:%s,http://127.0.0.1:%s\n" \
      "$i" "$(auth_port "$i")" "$(http_port "$i")"
  done
}

rpc_args() {
  for i in $(seq 0 $((NODES - 1))); do
    printf -- "--rpc\nhttp://127.0.0.1:%s\n" "$(http_port "$i")"
  done
}

driver() {
  shift || true
  mapfile -t ELS < <(el_args)
  # Same expected root the Kurtosis args files use, so both paths make the same
  # binary-tree assertion from one source of truth.
  local root=""
  if command -v yq >/dev/null 2>&1; then
    root="$(yq -r '.expected_genesis_root // ""' "$ROOT/args/phase1.yaml" 2>/dev/null || true)"
  fi
  local extra=()
  [[ -n "$root" && "$root" != "null" ]] && extra=(--expected-genesis-root "$root")
  PBT_ARTIFACT_DIR="${PBT_ARTIFACT_DIR:-$RUN/artifacts}" \
    "$DRIVER" "${ELS[@]}" --jwt "$RUN/jwtsecret" "${extra[@]}" "$@"
}

hammer() {
  shift || true
  [[ -x "$HAMMER" ]] || { echo "no hammer at $HAMMER" >&2; exit 1; }
  mapfile -t RPCS < <(rpc_args)
  "$HAMMER" "${RPCS[@]}" "$@"
}

stop() {
  pkill -f "$GETH --datadir $RUN" 2>/dev/null || true
  pkill -f "$HAMMER" 2>/dev/null || true
  echo stopped
}

logs() { tail -n "${2:-40}" "$RUN"/n*.log; }

case "${1:-start}" in
  start)  start ;;
  stop)   stop ;;
  logs)   logs "$@" ;;
  driver) driver "$@" ;;
  hammer) hammer "$@" ;;
  *) echo "usage: $0 [start|stop|logs|driver|hammer]" >&2; exit 1 ;;
esac
