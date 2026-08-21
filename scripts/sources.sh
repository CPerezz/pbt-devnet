#!/usr/bin/env bash
# Make sure the checkouts this devnet builds from are present.
#
# Clones what is missing. Does NOT touch what is already there: a checkout may be sitting on
# your own branch with your own work, and switching it out from under you is not this script's
# call. A mismatch is reported with the command to fix it.
#
# Branch names are not the real gate -- build-images.sh and build-besu.sh test whether the code
# can actually express the fork, which is the question that has a wrong answer available.
#
# Usage: scripts/sources.sh          (or `make sources`; `make build` and `make besu` run it)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

ensure() {
  local name=$1 url=$2 branch=$3 dir=$4 var=$5
  # -d "$dir/.git" rejects linked worktrees, where .git is a file. Compare --show-toplevel
  # against $dir rather than testing exit status: that alone means "inside some work tree",
  # which a directory nested in an unrelated repo satisfies, and a bare repo exits 0 too.
  local top phys
  top="$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null || true)"
  [[ -n "$top" ]] && top="$(cd "$top" && pwd -P)"
  phys="$(cd "$dir" 2>/dev/null && pwd -P || true)"
  if [[ -z "$top" || "$top" != "$phys" ]]; then
    if [[ -e "$dir" ]]; then
      echo "!! $name: $dir exists but is not a git checkout" >&2
      echo "   move it aside, or point $var somewhere else" >&2
      return 1
    fi
    echo "==> $name: cloning $branch from $url"
    git clone --branch "$branch" "$url" "$dir"
    return 0
  fi

  local on at
  on="$(git -C "$dir" branch --show-current 2>/dev/null || true)"
  at="$(git -C "$dir" rev-parse --short HEAD 2>/dev/null || echo '?')"
  if [[ "$on" == "$branch" ]]; then
    echo "==> $name: $at on $branch"
  else
    echo "==> $name: $at on ${on:-(detached)} — expected $branch, left alone" >&2
    echo "    it may be your own work. To switch:  git -C $dir checkout $branch" >&2
  fi
}

ensure "geth"           https://github.com/CPerezz/go-ethereum \
       pbt                          "${PBT_GETH_SRC:-$ROOT/../go-ethereum}"       PBT_GETH_SRC
ensure "erigon"         https://github.com/erigontech/erigon \
       binary-trie                  "${PBT_ERIGON_SRC:-$ROOT/../erigon-pbt}"      PBT_ERIGON_SRC
ensure "genesis-gen"    https://github.com/CPerezz/ethereum-genesis-generator \
       pbt                          "${PBT_EGG_SRC:-$ROOT/../egg-pbt}"            PBT_EGG_SRC
ensure "besu"           https://github.com/CPerezz/besu \
       fix/pbt-fcu-null-trie-node   "${PBT_BESU_ROOT:-$ROOT/../besu-pbt}"         PBT_BESU_ROOT
ensure "besu-stateless" https://github.com/besu-eth/besu-stateless \
       feat/partitioned-binary-trie "${PBT_BESU_STATELESS:-$ROOT/../besu-stateless}" PBT_BESU_STATELESS
