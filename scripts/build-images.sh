#!/usr/bin/env bash
# Build the three images the Kurtosis package expects.
#
# Images are built here rather than by Kurtosis's ImageBuildSpec because the geth
# build context lives outside this package (in a sibling checkout), and Kurtosis
# requires a build context inside the package directory. Building on the host also
# keeps the geth image reusable across enclaves.
#
# Because these are local tags with no registry behind them, run kurtosis WITHOUT
# `--image-download always` — that flag would try to pull `pbt-geth:local` and fail.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GETH_SRC="${PBT_GETH_SRC:-$ROOT/../go-ethereum}"
PLATFORM="${PBT_PLATFORM:-linux/arm64}"

if [[ ! -d "$GETH_SRC" ]]; then
  echo "no go-ethereum checkout at $GETH_SRC (set PBT_GETH_SRC)" >&2
  exit 1
fi

pinned_sha() {
  grep -oE 'go-ethereum v0\.0\.0-[0-9]+-[0-9a-f]+' "$ROOT/driver/go.mod" | tail -1 | grep -oE '[0-9a-f]+$'
}

echo "==> geth source:   $GETH_SRC"
echo "==> branch/commit: $(git -C "$GETH_SRC" rev-parse --short HEAD) ($(git -C "$GETH_SRC" branch --show-current))"
echo "==> driver pins:   $(pinned_sha)"
echo "==> platform:      $PLATFORM"
echo

# A .dockerignore in the geth checkout is what keeps this from shipping ~84MB of
# untracked build artifacts and the nested worktrees under .claude/ into the daemon.
if [[ ! -f "$GETH_SRC/.dockerignore" ]]; then
  echo "WARNING: no .dockerignore in $GETH_SRC — the build context will be enormous" >&2
fi

echo "==> building pbt-geth:local"
docker build --platform "$PLATFORM" -t pbt-geth:local "$GETH_SRC"

echo "==> building pbt-driver:local"
docker build --platform "$PLATFORM" -t pbt-driver:local "$ROOT/driver"

echo "==> building pbt-hammer:local"
docker build --platform "$PLATFORM" -t pbt-hammer:local "$ROOT/hammer"

echo
echo "done. next:"
echo "  kurtosis run $ROOT --enclave pbt --args-file $ROOT/args/phase1.yaml"
