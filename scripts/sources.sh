#!/usr/bin/env bash
# Puts each fork this devnet builds from at its pinned commit, in a checkout this script
# owns: ../pbt-devnet-src/<name>, created on first use and moved by later runs. A PBT_*_SRC
# variable builds your own checkout instead, exactly as it is, and this script leaves it alone.
# Usage: scripts/sources.sh   (or `make sources`)
#
# A pin is its branch's tip when a full lap last passed on it. To move one, set its _REF
# variable to a commit or a branch name (PBT_ERIGON_REF=binary-trie builds the moving tip).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${PBT_SRC_DIR:-$ROOT/../pbt-devnet-src}"

pin() {
  local name=$1 url=$2 ref=$3 own=$4
  local dir="$SRC/$name"
  if [[ -n "$own" ]]; then
    echo "==> $name: your checkout $own ($(git -C "$own" rev-parse --short HEAD 2>/dev/null || echo 'not a git checkout'))"
    return 0
  fi
  if [[ "$(git -C "$dir" rev-parse -q --verify HEAD 2>/dev/null || true)" == "$ref" ]]; then
    echo "==> $name: ${ref:0:10}"
    return 0
  fi
  echo "==> $name: fetching $ref from $url"
  [[ -d "$dir/.git" ]] || git init -q "$dir"
  # One commit, no history: none of these builds reads it.
  git -C "$dir" fetch -q --depth 1 "$url" "$ref"
  git -C "$dir" checkout -q --force --detach FETCH_HEAD
  echo "    at $(git -C "$dir" rev-parse --short=10 HEAD)"
}

pin geth           https://github.com/CPerezz/go-ethereum.git \
    "${PBT_GETH_REF:-fbfd486b126fcdfe131dd94ac467ace881e0b31b}"           "${PBT_GETH_SRC:-}"           # pbt
pin erigon         https://github.com/erigontech/erigon.git \
    "${PBT_ERIGON_REF:-7b675afb4bfe4cc40c1fc8128b85702106a58767}"         "${PBT_ERIGON_SRC:-}"         # binary-trie
pin egg            https://github.com/CPerezz/ethereum-genesis-generator.git \
    "${PBT_EGG_REF:-b56f205b9fef22a3d0f590be0c5fdaf4aa576cac}"            "${PBT_EGG_SRC:-}"            # pbt-offset
pin besu           https://github.com/matkt/besu.git \
    "${PBT_BESU_REF:-39377a2659f63c3bb230a5f57b4ef6846348f876}"           "${PBT_BESU_ROOT:-}"          # main-pbt
pin besu-stateless https://github.com/besu-eth/besu-stateless.git \
    "${PBT_BESU_STATELESS_REF:-62b752345cce00d9119f37c37d56eb9566269578}" "${PBT_BESU_STATELESS:-}"     # feat/partitioned-binary-trie
