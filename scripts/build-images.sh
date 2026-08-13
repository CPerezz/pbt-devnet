#!/usr/bin/env bash
# Build the images the Kurtosis package expects: one per execution client listed in an
# args file's client_sources, plus the driver and the hammer.
#
# Client images are built here rather than by Kurtosis's ImageBuildSpec because each
# client's source tree lives outside this package, and Kurtosis requires a build context
# inside the package directory.
#
# Because these are local tags with no registry behind them, run kurtosis WITHOUT
# `--image-download always` — that would try to pull them and fail.
#
# Usage: scripts/build-images.sh [args-file]        (default: args/phase1.yaml)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARGS="${1:-$ROOT/args/phase1.yaml}"
PLATFORM="${PBT_PLATFORM:-linux/arm64}"

[[ -f "$ARGS" ]] || { echo "no args file at $ARGS" >&2; exit 1; }

# Read the YAML with whatever is available. yq is nicest; PyYAML is the common fallback.
# Emits one "client<TAB>image<TAB>path<TAB>repository<TAB>ref" line per client.
read_clients() {
  if command -v yq >/dev/null 2>&1; then
    yq -r '
      .client_sources as $s | .default_ethereum_client_images as $i |
      ($s | keys[]) as $c |
      [$c, ($i[$c] // ""), ($s[$c].path // ""), ($s[$c].repository // ""), ($s[$c].ref // "")]
      | @tsv' "$ARGS"
  elif python3 -c 'import yaml' >/dev/null 2>&1; then
    python3 - "$ARGS" <<'PY'
import sys, yaml
d = yaml.safe_load(open(sys.argv[1])) or {}
srcs = d.get("client_sources") or {}
imgs = d.get("default_ethereum_client_images") or {}
for c, s in srcs.items():
    s = s or {}
    print("\t".join([c, imgs.get(c, ""), s.get("path", ""), s.get("repository", ""), s.get("ref", "")]))
PY
  else
    echo "need either 'yq' or python3 with PyYAML to read $ARGS" >&2
    echo "  brew install yq     # or: pip3 install pyyaml" >&2
    exit 1
  fi
}

echo "==> args file: $ARGS"
echo "==> platform:  $PLATFORM"
echo

built=0
while IFS=$'\t' read -r client image path repository ref; do
  [[ -n "$client" ]] || continue
  if [[ -z "$image" ]]; then
    echo "!! $client has no entry in default_ethereum_client_images; skipping" >&2
    continue
  fi
  src="$path"
  [[ "$src" = /* ]] || src="$ROOT/$src"
  if [[ ! -d "$src" ]]; then
    echo "!! $client: no checkout at $src" >&2
    echo "   clone ${repository:-<repository>} at ref ${ref:-<ref>} and point client_sources[$client].path at it" >&2
    exit 1
  fi

  echo "==> $client -> $image"
  local at="$(git -C "$src" rev-parse --short HEAD 2>/dev/null || echo 'not a git checkout')"
  local on="$(git -C "$src" branch --show-current 2>/dev/null || true)"
  [[ -n "$on" ]] && at="$at on $on"
  echo "    source:  $src ($at)"
  echo "    upstream: ${repository:-?} @ ${ref:-?}"
  # A .dockerignore in the client checkout is what keeps the build context from including
  # untracked build artifacts and nested worktrees.
  [[ -f "$src/.dockerignore" ]] || echo "    WARNING: no .dockerignore in $src — build context may be huge" >&2
  docker build --platform "$PLATFORM" -t "$image" "$src"
  built=$((built + 1))
  echo
done < <(read_clients)

[[ "$built" -gt 0 ]] || { echo "no client images built — is client_sources empty?" >&2; exit 1; }

echo "==> pbt-driver:local"
docker build --platform "$PLATFORM" -t pbt-driver:local "$ROOT/driver"
echo "==> pbt-hammer:local"
docker build --platform "$PLATFORM" -t pbt-hammer:local "$ROOT/hammer"

echo
echo "done ($built client image(s)). next:"
echo "  kurtosis run $ROOT --enclave pbt --args-file $ARGS"
