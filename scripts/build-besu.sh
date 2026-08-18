#!/usr/bin/env bash
# Build a PBT-capable besu image (besu-pbt:local).
#
# Besu is the one client that cannot go through scripts/build-images.sh, because it is a
# two-stage Gradle build rather than a plain `docker build`:
#
#   1. besu-eth/besu-stateless@feat/partitioned-binary-trie is the partitioned-binary-trie
#      library. It has to be published to the local Maven repository first — besu declares
#      `implementation('org.hyperledger.besu:besu-stateless')` and resolves it from
#      mavenLocal, so skipping this step yields a besu built against nothing or against a
#      stale copy.
#   2. matkt/besu@glamsterdam-devnet-8-pbt then builds with `installDist`, producing a
#      distribution in build/install/besu.
#
# Only then can the image be built: besu's docker/Dockerfile does
# `COPY --chown=besu:besu besu /opt/besu/`, i.e. it wants a directory literally named
# `besu` in the build context, so the distribution is staged under that name.
#
# Both repos require JDK 25 (gradle/gradle-daemon-jvm.properties: toolchainVersion=25).
# The JDK is passed to Gradle explicitly rather than via JAVA_HOME on PATH, so a keg-only
# install can serve this build without becoming the machine's default java.
#
# Usage:
#   scripts/build-besu.sh                  # both Gradle stages, then the image
#   scripts/build-besu.sh --skip-gradle    # image only, reusing build/install/besu
#   PBT_BESU_ROOT=... PBT_BESU_STATELESS=... scripts/build-besu.sh
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

DIST="$BESU/build/install/besu"
[[ -x "$DIST/bin/besu" ]] || {
  echo "no besu distribution at $DIST (run without --skip-gradle)" >&2; exit 1; }

# The Dockerfile's COPY expects the distribution to be named `besu` in the context, and
# pyroscope.properties alongside it.
CTX="$BESU/build/docker-ctx"
rm -rf "$CTX" && mkdir -p "$CTX"
cp -R "$DIST" "$CTX/besu"
cp "$BESU/docker/Dockerfile" "$BESU/docker/pyroscope.properties" "$CTX/"

echo "==> $IMAGE ($PLATFORM)"
docker build --platform "$PLATFORM" -t "$IMAGE" "$CTX"

echo
echo "done. sanity check:"
echo "  docker run --rm --platform $PLATFORM --entrypoint besu $IMAGE --version"
