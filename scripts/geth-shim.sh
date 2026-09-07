#!/bin/sh
# geth-shim: EIP-8347 migration bootstrap around the real geth binary (geth.real).
# Intercepts both `geth init` and the server invocation from ethereum-package's launcher.
# PBT_MIGRATION unset: byte-faithful exec, behaves exactly like unshimmed geth.
# PBT_MIGRATION=1: once per datadir, converts the merkle genesis, emits EIP-8347
# artifacts, and imports them back before the real server run; any failure aborts
# rather than falling through to the node.
# Known ceiling: a flags-only run like `geth --help` is misread as a server run
# under PBT_MIGRATION=1 and fails loudly on the missing --datadir.
set -eu

REAL=/usr/local/bin/geth.real

# No arguments: nothing to inspect, hand over.
[ "$#" -gt 0 ] || exec "$REAL"

case "$1" in
init)
    if [ "${PBT_MIGRATION:-}" = "1" ]; then
        # Preimages are needed later: the offline conversion re-derives merkle paths from them.
        shift
        exec "$REAL" init --cache.preimages "$@"
    fi
    exec "$REAL" "$@"
    ;;
-*)
    # Flags first: the server run. Falls through to the migration prep below.
    ;;
*)
    # Other subcommands pass through untouched so the node's own recovery commands work.
    exec "$REAL" "$@"
    ;;
esac

[ "${PBT_MIGRATION:-}" = "1" ] || exec "$REAL" "$@"

DATADIR=
prev=
for arg in "$@"; do
    case "$arg" in
    --datadir=*) DATADIR="${arg#--datadir=}" ;;
    esac
    [ "$prev" = "--datadir" ] && DATADIR="$arg"
    prev="$arg"
done
if [ -z "$DATADIR" ]; then
    echo "geth-shim: PBT_MIGRATION=1 but the server invocation carries no --datadir" >&2
    exit 1
fi

MARKER="$DATADIR/.pbt_migration_done"
if [ -e "$MARKER" ]; then
    # Already prepared: prep is once per datadir, the node recovers its own state.
    exec "$REAL" "$@"
fi

if [ ! -d "$DATADIR/geth/chaindata" ]; then
    # Defensive fallback: kurtosis always inits first, but other drivers might not.
    GENESIS=/network-configs/genesis.json
    if [ ! -f "$GENESIS" ]; then
        echo "geth-shim: datadir $DATADIR holds no chain and $GENESIS is absent; init first" >&2
        exit 1
    fi
    "$REAL" init --state.scheme=path --cache.preimages --datadir "$DATADIR" "$GENESIS"
fi

SCRATCH="$(mktemp -d /tmp/pbt-convert.XXXXXX)"
trap 'rm -rf "$SCRATCH"' EXIT

# convert mutates its target datadir in place, so it runs against a scratch copy; the
# node's own datadir stays pure merkle until the verified import below writes the shadow.
cp -R "$DATADIR/." "$SCRATCH/data"

ART="$DATADIR/pbt-artifacts"
mkdir -p "$ART"
"$REAL" bintrie convert --datadir "$SCRATCH/data" \
    --snapshot-out "$ART/snapshot" --preimages-out "$ART/preimages"

# Digests must match across every node (same genesis, same binary) -- a determinism
# check, not proof of correctness.
echo "PBT_ARTIFACT_DIGESTS $(sha256sum "$ART/snapshot" "$ART/preimages" | awk '{print $2 "=" $1}' | tr '\n' ' ')"

# Anchor 0 = genesis state; flags must precede positionals or urfave/cli prints usage and exits 0.
"$REAL" bintrie import --datadir "$DATADIR" "$ART/snapshot" "$ART/preimages" 0

touch "$MARKER"
exec "$REAL" "$@"
