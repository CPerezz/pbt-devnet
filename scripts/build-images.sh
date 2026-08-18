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

build_from "geth (EIP-8297)" "pbt-geth:local" "$GETH_SRC" \
  "clone CPerezz/go-ethereum at branch pbt, or set PBT_GETH_SRC"

build_from "genesis generator" "pbt-egg:local" "$EGG_SRC" \
  "clone CPerezz/ethereum-genesis-generator at branch pbt, or set PBT_EGG_SRC"

echo "==> pbt-driver:local"
docker build --platform "$PLATFORM" -t pbt-driver:local "$ROOT/driver"
echo "==> pbt-hammer:local"
docker build --platform "$PLATFORM" -t pbt-hammer:local "$ROOT/hammer"

echo
if docker image inspect besu-pbt:local >/dev/null 2>&1; then
  echo "done. besu-pbt:local is present."
else
  echo "done — but besu-pbt:local is MISSING, and args/devnet.yaml expects it."
  echo "  run 'make besu' (two Gradle stages, needs JDK 25), or drop the besu participant."
fi
