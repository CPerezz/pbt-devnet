#!/usr/bin/env bash
# Build a PBT-capable besu image (besu-pbt:local): two Gradle stages (besu-stateless ->
# mavenLocal, then besu installDist), then a docker build. Needs JDK 25 (keg-only ok).
# Usage:
#   scripts/build-besu.sh                  # both Gradle stages, then the image
#   scripts/build-besu.sh --skip-gradle    # image only, reusing build/install/besu
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BESU="${PBT_BESU_ROOT:-$ROOT/../besu-pbt}"
STATELESS="${PBT_BESU_STATELESS:-$ROOT/../besu-stateless}"
IMAGE="${PBT_BESU_IMAGE:-besu-pbt:local}"
PLATFORM="${PBT_PLATFORM:-linux/arm64}"
SKIP_GRADLE=0

for arg in "$@"; do
  case "$arg" in
    --skip-gradle) SKIP_GRADLE=1 ;;
    *) echo "unknown argument: $arg" >&2; exit 1 ;;
  esac
done

# Find a JDK 25 without disturbing whatever `java` currently points at.
find_jdk25() {
  local candidates=(
    "${PBT_JAVA_HOME:-}"
    "/opt/homebrew/opt/openjdk@25/libexec/openjdk.jdk/Contents/Home"
    "/usr/lib/jvm/java-25-openjdk-amd64"
    "/usr/lib/jvm/java-25-openjdk-arm64"
  )
  for c in "${candidates[@]}"; do
    [[ -n "$c" && -x "$c/bin/javac" ]] && { echo "$c"; return 0; }
  done
  if command -v /usr/libexec/java_home >/dev/null 2>&1; then
    /usr/libexec/java_home -v 25 2>/dev/null && return 0
  fi
  return 1
}

if [[ "$SKIP_GRADLE" -eq 0 ]]; then
  JDK25="$(find_jdk25 || true)"
  if [[ -z "$JDK25" ]]; then
    echo "no JDK 25 found, and besu requires it (gradle-daemon-jvm.properties: toolchainVersion=25)" >&2
    echo "  macOS:  brew install openjdk@25      # keg-only, will not become your default java" >&2
    echo "  or set PBT_JAVA_HOME to a JDK 25 home" >&2
    exit 1
  fi
  echo "==> JDK 25: $JDK25"

  [[ -d "$STATELESS" ]] || { echo "no besu-stateless checkout at $STATELESS" >&2; exit 1; }
  echo "==> besu-stateless -> mavenLocal ($(git -C "$STATELESS" rev-parse --short HEAD 2>/dev/null || echo '?'))"
  ( cd "$STATELESS" && JAVA_HOME="$JDK25" ./gradlew build publishToMavenLocal -x test \
      --console=plain -Dorg.gradle.java.installations.paths="$JDK25" )

  [[ -d "$BESU" ]] || { echo "no besu checkout at $BESU" >&2; exit 1; }
  echo "==> besu installDist ($(git -C "$BESU" rev-parse --short HEAD 2>/dev/null || echo '?'))"
  ( cd "$BESU" && JAVA_HOME="$JDK25" ./gradlew installDist -x test \
      --console=plain -Dorg.gradle.java.installations.paths="$JDK25" )
fi

# Both of these build, and a besu missing either one fails in a way that looks
# like the devnet's fault rather than the checkout's, so refuse instead.
#
# The trie has to be chosen per header: on the older branches it came from the
# datadir's storage format, fixed at startup, and such a node keeps committing
# merkle roots straight past binaryTrieTime.
if ! grep -rq 'resolveTrieBranchType' "$BESU/ethereum/core/src/main/java" 2>/dev/null; then
  echo "!! $BESU cannot migrate: no per-header trie switch (resolveTrieBranchType)." >&2
  echo "   fix: git -C $BESU fetch && git -C $BESU checkout glamsterdam-devnet-8-pbt && git -C $BESU pull" >&2
  echo "   then: git -C $STATELESS pull   # that branch needs a besu-stateless gradle is told to expect" >&2
  exit 1
fi
# Without matkt/besu#31 (NPE fix), besu imports blocks but refuses every forkchoiceUpdated.
# The call moved into the binary writer when the per-header switch landed, so match
# the deletion itself rather than one file's signature.
if ! grep -rq 'removeTrieNode(' \
     "$BESU/ethereum/core/src/main/java/org/hyperledger/besu/ethereum/mainnet/staterootcommitter" 2>/dev/null; then
  echo "!! $BESU does not contain the binary-trie deletion fix (matkt/besu#31)." >&2
  echo "   Without it besu will import blocks and then refuse every forkchoiceUpdated." >&2
  exit 1
fi

DIST="$BESU/build/install/besu"
[[ -x "$DIST/bin/besu" ]] || {
  echo "no besu distribution at $DIST (run without --skip-gradle)" >&2; exit 1; }

# Dockerfile's COPY expects the distribution staged as `besu` alongside pyroscope.properties.
CTX="$BESU/build/docker-ctx"
rm -rf "$CTX" && mkdir -p "$CTX"
cp -R "$DIST" "$CTX/besu"
cp "$BESU/docker/Dockerfile" "$BESU/docker/pyroscope.properties" "$CTX/"
# The Dockerfile COPYs the agent from the build context rather than ADDing it from
# GitHub, and only besu's own distDocker task stages it; this builds the image by
# hand, so fetch it here. Same release build.gradle pins.
if [[ ! -f "$CTX/pyroscope.jar" ]]; then
  curl -fsSL -o "$CTX/pyroscope.jar" \
    "https://github.com/grafana/pyroscope-java/releases/download/v2.6.0/pyroscope.jar"
fi

echo "==> $IMAGE ($PLATFORM)"
docker build --platform "$PLATFORM" -t "$IMAGE" "$CTX"

echo
echo "done. sanity check:"
echo "  docker run --rm --platform $PLATFORM --entrypoint besu $IMAGE --version"
