#!/usr/bin/env bash
# Build the local images the Kurtosis package expects. Besu is separate (two-stage
# Gradle build): run `make besu` once, then `make up`.
# Local tags only -- do not run kurtosis with --image-download always.
# Usage: scripts/build-images.sh [args-file]
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

# Prints what a checkout actually is (commit + branch), not just that it built.
provenance() {
  local dir=$1
  local at on
  at="$(git -C "$dir" rev-parse --short HEAD 2>/dev/null || echo 'not a git checkout')"
  on="$(git -C "$dir" branch --show-current 2>/dev/null || true)"
  [[ -n "$on" ]] && at="$at on $on"
  echo "$at"
}

# Refuses a source tree that cannot express the fork; a client built without it silently
# starts on the merkle-patricia trie with binaryTrieTime ignored.
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
  # .dockerignore keeps build artifacts and nested worktrees out of the build context.
  [[ -f "$dir/.dockerignore" ]] || echo "    WARNING: no .dockerignore in $dir — context may be huge" >&2
  docker build --platform "$PLATFORM" -t "$image" "$dir"
  echo
}

# IMAGES: comma list of images to build; default is everything. A scoped list like
# IMAGES=geth,egg,tools skips broken/irrelevant checkouts for the others.
IMAGES="${IMAGES:-geth,erigon,nethermind,egg,tools}"
want() {
  case ",$IMAGES," in
  *",$1,"*) return 0 ;;
  *) return 1 ;;
  esac
}

if want geth; then
require_capability "geth (EIP-8297)" "$GETH_SRC" 'json:"binaryTrieTime' "params/config.go" \
  "fix: git -C $GETH_SRC checkout pbt"
# Both needles below have failure shapes that look like success if built from the wrong branch.
require_capability "geth (EIP-8347 tooling)" "$GETH_SRC" 'bintrieImportCommand' "cmd/geth/bintrie_import.go" \
  "fix: git -C $GETH_SRC checkout pbt"
# Load-bearing symbols on the current pbt tip: the shadow-tree follower and the window knob.
require_capability "geth (EIP-8347 follower)" "$GETH_SRC" 'newBintrieFollower' "core/bintrie_follower.go" \
  "fix: git -C $GETH_SRC checkout pbt (needs the online state-migration merge)"
require_capability "geth (migration window)" "$GETH_SRC" 'MigrationWindowBlocks' "core/blockchain.go" \
  "fix: git -C $GETH_SRC checkout pbt (needs the online state-migration merge)"
build_from "geth (EIP-8297)" "pbt-geth:local" "$GETH_SRC" \
  "clone CPerezz/go-ethereum at branch pbt, or set PBT_GETH_SRC"
echo
fi

if want erigon; then
# Needle is the genesis path: parsing binaryTrieTime alone isn't enough without COMMITMENT_BIN.
require_capability "erigon (EIP-8297)" "$ERIGON_SRC" 'ResolveErigonDBSettingsForGenesis' \
  "execution/state/genesiswrite/genesis_write.go" "fix: git -C $ERIGON_SRC checkout binary-trie && git -C $ERIGON_SRC pull"
build_from "erigon (EIP-8297)" "erigon-pbt:local" "$ERIGON_SRC" \
  "clone erigontech/erigon at branch binary-trie, or set PBT_ERIGON_SRC"
fi

if want nethermind; then
echo "==> nethermind (EIP-8297) -> nethermind-pbt:local"
echo "    source: https://github.com/NethermindEth/nethermind.git#pbt-state"
docker build --platform "$PLATFORM" -t nethermind-pbt:local \
  "https://github.com/NethermindEth/nethermind.git#pbt-state"
echo
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
  build_cmd pbt-migration-monitor:local migration-monitor
  build_cmd pbt-migration-chaos:local   migration-chaos
  build_cmd pbt-migration-gate:local    migration-gate
fi

echo
if docker image inspect besu-pbt:local >/dev/null 2>&1; then
  echo "done. besu-pbt:local is present."
else
  echo "done — but besu-pbt:local is MISSING, and args/devnet.yaml expects it."
  echo "  run 'make besu' (two Gradle stages, needs JDK 25), or drop the besu participant."
fi
