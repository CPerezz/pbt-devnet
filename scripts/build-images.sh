#!/usr/bin/env bash
# Build the local images the Kurtosis package expects.
#
# Source checkouts are given by environment variable rather than read from the args file:
# that file is now ethpandaops/ethereum-package's own schema, and it fails on any key it
# does not recognise, so build configuration cannot live there.
#
# Besu is deliberately absent — it is a two-stage Gradle build rather than a docker build,
# so it has its own script. Run `make besu` once, then `make up`.
#
# Because these are local tags with no registry behind them, run kurtosis WITHOUT
# `--image-download always` — that would try to pull them and fail.
#
# Usage: scripts/build-images.sh [args-file]      (the args file is only echoed, for context)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARGS="${1:-$ROOT/args/devnet.yaml}"
PLATFORM="${PBT_PLATFORM:-linux/arm64}"

# Where the forks live. Defaults assume they sit beside this repo.
GETH_SRC="${PBT_GETH_SRC:-$ROOT/../go-ethereum}"
ERIGON_SRC="${PBT_ERIGON_SRC:-$ROOT/../erigon-pbt}"
EGG_SRC="${PBT_EGG_SRC:-$ROOT/../egg-pbt}"

echo "==> platform:  $PLATFORM"
echo "==> args file: $ARGS"
echo

# provenance prints what a checkout actually is, because "it built" and "it built the thing
# you meant" are different claims — especially with several PBT branches in flight.
provenance() {
  local dir=$1
  local at on
  at="$(git -C "$dir" rev-parse --short HEAD 2>/dev/null || echo 'not a git checkout')"
  on="$(git -C "$dir" branch --show-current 2>/dev/null || true)"
  [[ -n "$on" ]] && at="$at on $on"
  echo "$at"
}

# require_capability refuses a source tree that cannot express the fork. A client built
# without it does not fail -- it ignores "binaryTrieTime" and starts on the merkle-patricia
# trie with no error anywhere, which is the silent mismatch this devnet exists to catch.
#
# The needle is the MECHANISM, not the word: all three files mention binaryTrieTime in comments
# and log strings, so grepping the bare name passes on a tree where only the prose survived.
require_capability() {
  local name=$1 dir=$2 needle=$3 file=$4 fix=$5
  [[ -d "$dir" ]] || return 0          # build_from reports a missing checkout
  if [[ -f "$dir/$file" ]] && grep -q "$needle" "$dir/$file"; then
    return 0
  fi
  echo "!! $name: $dir cannot express the binary tree." >&2
  echo "   $file has no $needle, so the build would produce a client that" >&2
  echo "   ignores \"binaryTrieTime\" in genesis and runs on the merkle-patricia trie." >&2
  echo "   source is at $(provenance "$dir")" >&2
  echo "   $fix" >&2
  exit 1
}

build_from() {
  local name=$1 image=$2 dir=$3 note=$4
  if [[ ! -d "$dir" ]]; then
    echo "!! $name: no checkout at $dir" >&2
    echo "   $note" >&2
    exit 1
  fi
  echo "==> $name -> $image"
  echo "    source: $dir ($(provenance "$dir"))"
  # A .dockerignore in the checkout is what keeps build artifacts and nested worktrees out
  # of the build context.
  [[ -f "$dir/.dockerignore" ]] || echo "    WARNING: no .dockerignore in $dir — context may be huge" >&2
  docker build --platform "$PLATFORM" -t "$image" "$dir"
  echo
}

# IMAGES selects which images to build (comma list). Default = everything,
# which preserves the canonical `make build` semantics; a scoped invocation
# like IMAGES=geth,egg,tools skips the rest — the migration devnet needs no
# erigon or besu, and a broken checkout for an unrequested image must not
# block the ones that matter.
IMAGES="${IMAGES:-geth,erigon,egg,tools}"
want() {
  case ",$IMAGES," in
  *",$1,"*) return 0 ;;
  *) return 1 ;;
  esac
}

if want geth; then
require_capability "geth (EIP-8297)" "$GETH_SRC" 'json:"binaryTrieTime' "params/config.go" \
  "fix: git -C $GETH_SRC checkout pbt"
# The migration needs more than the fork key. Both of these have failure
# shapes that look like success if built from the wrong branch: without the
# bintrie tooling the shim's prep would fail at first boot, and without the
# open-mode work a future binaryTrieTime silently runs the tree from genesis.
require_capability "geth (EIP-8347 tooling)" "$GETH_SRC" 'bintrieImportCommand' "cmd/geth/bintrie_import.go" \
  "fix: git -C $GETH_SRC checkout pbt"
# The needles are load-bearing symbols on the current pbt tip (the
# online state-migration merge): the follower is what maintains the shadow tree,
# and the window knob is the switchover's closing condition. A tree carrying
# only the tooling would convert and import but never follow.
require_capability "geth (EIP-8347 follower)" "$GETH_SRC" 'newBintrieFollower' "core/bintrie_follower.go" \
  "fix: git -C $GETH_SRC checkout pbt (needs the online state-migration merge)"
require_capability "geth (migration window)" "$GETH_SRC" 'MigrationWindowBlocks' "core/blockchain.go" \
  "fix: git -C $GETH_SRC checkout pbt (needs the online state-migration merge)"
# Two-stage: the fork's own Dockerfile builds the binary under a staging tag,
# then a thin overlay installs the migration shim at the exact name the
# launcher invokes (see scripts/geth-shim.sh). The overlay must never build
# FROM pbt-geth:local itself — a self-referencing base would shim the shim on
# the next rebuild.
build_from "geth (EIP-8297)" "pbt-geth-binary:local" "$GETH_SRC" \
  "clone CPerezz/go-ethereum at branch pbt, or set PBT_GETH_SRC"
echo "==> geth migration shim -> pbt-geth:local"
docker build --platform "$PLATFORM" -t pbt-geth:local -f "$ROOT/scripts/geth-shim.Dockerfile" "$ROOT/scripts"
echo
fi

if want erigon; then
# The needle is the genesis path, not the config key: a tree that only PARSES binaryTrieTime
# still needs COMMITMENT_BIN to turn the tree on, and this package no longer passes it, so
# such a checkout would refuse to start rather than build a wrong chain. Failing here says
# why.
require_capability "erigon (EIP-8297)" "$ERIGON_SRC" 'ResolveErigonDBSettingsForGenesis' \
  "execution/state/genesiswrite/genesis_write.go" "fix: git -C $ERIGON_SRC checkout binary-trie && git -C $ERIGON_SRC pull"
build_from "erigon (EIP-8297)" "erigon-pbt:local" "$ERIGON_SRC" \
  "clone erigontech/erigon at branch binary-trie, or set PBT_ERIGON_SRC"
fi

if want egg; then
require_capability "genesis generator" "$EGG_SRC" '"binaryTrieTime":' "apps/el-gen/generate_genesis.sh" \
  "fix: git -C $EGG_SRC checkout pbt"
build_from "genesis generator" "pbt-egg:local" "$EGG_SRC" \
  "clone CPerezz/ethereum-genesis-generator at branch pbt, or set PBT_EGG_SRC"
fi

# Our three services come out of one Dockerfile and one module; only the command differs.
build_cmd() {
  echo "==> $1"
  docker build --platform "$PLATFORM" --build-arg "CMD=$2" -t "$1" "$ROOT"
}
if want tools; then
  build_cmd pbt-monitor:local pbtmonitor
  build_cmd pbt-hammer:local  pbthammer
  build_cmd pbt-chaos:local   pbtchaos
fi

echo
if docker image inspect besu-pbt:local >/dev/null 2>&1; then
  echo "done. besu-pbt:local is present."
else
  echo "done — but besu-pbt:local is MISSING, and args/devnet.yaml expects it."
  echo "  run 'make besu' (two Gradle stages, needs JDK 25), or drop the besu participant."
fi
