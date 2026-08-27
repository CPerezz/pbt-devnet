#!/bin/sh
# geth-shim: EIP-8347 migration bootstrap wrapped around the real geth binary.
#
# ethereum-package's launcher runs `geth init ... && geth <server flags>`
# inside one `sh -c` string, so this shim sits at the exact name `geth` and
# intercepts BOTH invocations; the real binary lives at geth.real. With
# PBT_MIGRATION unset the shim is a byte-faithful exec and the image behaves
# exactly like the unshimmed one — the PBT-at-genesis devnet must not be able
# to tell the difference.
#
# With PBT_MIGRATION=1 the server invocation is preceded, once per datadir, by
# the empty-state EIP-8347 flow the migration devnet exists to exercise:
# convert the freshly-inited merkle genesis on a scratch COPY (convert mutates
# its target datadir in place), emit the byte-canonical distribution
# artifacts, and import them back through the dual-check at anchor 0. The
# round-trip is strictly file-mediated: the node's shadow tree comes only from
# the emitted files. Any failure exits non-zero and never falls through to
# the node. Since the #31 online-migration merge the import is OPTIONAL to
# the node itself — genesis-seeded catch-up works — but exercising the
# artifact flow is this devnet's point, so the shim keeps running it.
#
# Known ceiling: a flags-only invocation such as `geth --help` is
# indistinguishable from a server run and takes the migration path under
# PBT_MIGRATION=1; it fails loudly on the missing --datadir rather than
# printing help. Nothing in the launcher does that.
set -eu

REAL=/usr/local/bin/geth.real

# No arguments: nothing to inspect, hand over.
[ "$#" -gt 0 ] || exec "$REAL"

case "$1" in
init)
    if [ "${PBT_MIGRATION:-}" = "1" ]; then
        # The offline conversion re-derives every merkle path from its
        # preimage, so the genesis commit must retain them.
        shift
        exec "$REAL" init --cache.preimages "$@"
    fi
    exec "$REAL" "$@"
    ;;
-*)
    # Flags first: the server run. Falls through to the migration prep below.
    ;;
*)
    # Any other subcommand (bintrie, db, version, ...) passes through
    # untouched, so the recovery commands the node's own errors prescribe
    # stay runnable through the shim.
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
    # Restart of an already-prepared datadir: prep is once per datadir, the
    # node recovers its own state from here.
    exec "$REAL" "$@"
fi

if [ ! -d "$DATADIR/geth/chaindata" ]; then
    # The kurtosis launcher always inits before the server run, so this is a
    # defensive fallback for anything else driving the image — using the
    # mount path the launcher uses.
    GENESIS=/network-configs/genesis.json
    if [ ! -f "$GENESIS" ]; then
        echo "geth-shim: datadir $DATADIR holds no chain and $GENESIS is absent; init first" >&2
        exit 1
    fi
    "$REAL" init --state.scheme=path --cache.preimages --datadir "$DATADIR" "$GENESIS"
fi

SCRATCH="$(mktemp -d /tmp/pbt-convert.XXXXXX)"
trap 'rm -rf "$SCRATCH"' EXIT

# convert mutates its target datadir in place — binary-tree nodes land right
# next to the merkle data — so it runs against a copy. The node's own datadir
# stays pure merkle until the verified import below writes the shadow.
cp -R "$DATADIR/." "$SCRATCH/data"

ART="$DATADIR/pbt-artifacts"
mkdir -p "$ART"
"$REAL" bintrie convert --datadir "$SCRATCH/data" \
    --snapshot-out "$ART/snapshot" --preimages-out "$ART/preimages"

# Full-strength digests for the cross-node determinism check: every node
# converts the same genesis with the same binary, so these must be identical
# on every participant (a determinism check, not migration-correctness
# evidence).
echo "PBT_ARTIFACT_DIGESTS $(sha256sum "$ART/snapshot" "$ART/preimages" | awk '{print $2 "=" $1}' | tr '\n' ' ')"

# Anchor 0: the artifacts cover exactly the genesis state, and init has made
# block zero canonical. Flags before positionals — urfave/cli silently prints
# usage and exits 0 the other way around.
"$REAL" bintrie import --datadir "$DATADIR" "$ART/snapshot" "$ART/preimages" 0

touch "$MARKER"
exec "$REAL" "$@"
