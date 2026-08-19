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

# require_capability refuses to build a source tree that cannot express the fork.
#
# A checkout can be perfectly valid git and still be the wrong one: several PBT branches are
# in flight and only some carry the timestamp-fork plumbing. A geth built without
# BinaryTrieTime does not fail -- it ignores "binaryTrieTime" in genesis and starts on the
# merkle-patricia trie, with the wrong state root and no error anywhere. That is the exact
# silent mismatch this devnet exists to catch, so it must not be possible to build it.
#
# The test is whether the source can parse the key we ship, not whether its commit matches a
# recorded one: a hash comparison only tells you the checkout moved, which is not the same
# question and has a wrong answer available.
require_capability() {
  local name=$1 dir=$2 needle=$3 file=$4 fix=$5
  [[ -d "$dir" ]] || return 0          # build_from reports a missing checkout
  if [[ -f "$dir/$file" ]] && grep -q "$needle" "$dir/$file"; then
    return 0
  fi
  echo "!! $name: $dir cannot express the binary tree." >&2
  echo "   $file does not mention $needle, so the build would produce a client that" >&2
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

require_capability "geth (EIP-8297)" "$GETH_SRC" "BinaryTrieTime" "params/config.go" \
  "fix: git -C $GETH_SRC checkout pbt"
build_from "geth (EIP-8297)" "pbt-geth:local" "$GETH_SRC" \
  "clone CPerezz/go-ethereum at branch pbt, or set PBT_GETH_SRC"

require_capability "genesis generator" "$EGG_SRC" "binaryTrieTime" "apps/el-gen/generate_genesis.sh" \
  "fix: git -C $EGG_SRC checkout pbt"
build_from "genesis generator" "pbt-egg:local" "$EGG_SRC" \
  "clone CPerezz/ethereum-genesis-generator at branch pbt, or set PBT_EGG_SRC"

# Our three services come out of one Dockerfile and one module; only the command differs.
build_cmd() {
  echo "==> $1"
  docker build --platform "$PLATFORM" --build-arg "CMD=$2" -t "$1" "$ROOT"
}
build_cmd pbt-monitor:local pbtmonitor
build_cmd pbt-hammer:local  pbthammer
build_cmd pbt-chaos:local   pbtchaos

echo
if docker image inspect besu-pbt:local >/dev/null 2>&1; then
  echo "done. besu-pbt:local is present."
else
  echo "done — but besu-pbt:local is MISSING, and args/devnet.yaml expects it."
  echo "  run 'make besu' (two Gradle stages, needs JDK 25), or drop the besu participant."
fi
