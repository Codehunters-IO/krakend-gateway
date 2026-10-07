#!/usr/bin/env bash
# Guards the invariant that makes Go plugins loadable at all: a .so must be
# built with the same Go toolchain as the host binary that dlopen()s it. A
# mismatch fails at runtime with a cryptic "plugin was built with a different
# version of package runtime" and no amount of `krakend check` sees it.
#
# The repository pins that toolchain in two independent places that nothing
# compares:
#
#   - plugins/Dockerfile.builder — `FROM golang:X-alpine`, used by
#     `make plugin-build` for local development.
#   - the KrakenD image itself (KRAKEND_IMAGE) — whose binary was built with
#     whatever Go version its maintainers used for that release.
#
# The production Dockerfile sidesteps the problem by building with the official
# `krakend/builder:<tag>`, which matches by construction. The local builder does
# not, so a Go patch bump in Dockerfile.builder or a KrakenD upgrade can silently
# diverge and only break on a developer's machine.
#
# Usage: check-plugin-abi.sh <krakend-image>
set -euo pipefail

IMAGE="${1:?usage: check-plugin-abi.sh <krakend-image>}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILDER_DOCKERFILE="$ROOT_DIR/plugins/Dockerfile.builder"
PLUGIN_BUILD_DIR="$ROOT_DIR/plugins/build"

if [[ ! -f "$BUILDER_DOCKERFILE" ]]; then
  echo "check-plugin-abi: $BUILDER_DOCKERFILE not found" >&2
  exit 1
fi

# `FROM golang:1.25.9-alpine` -> 1.25.9
builder_go="$(sed -nE 's/^FROM[[:space:]]+golang:([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/p' \
  "$BUILDER_DOCKERFILE" | head -1)"

if [[ -z "$builder_go" ]]; then
  echo "check-plugin-abi: could not read a golang version from $BUILDER_DOCKERFILE" >&2
  echo "  expected a line like: FROM golang:1.25.9-alpine" >&2
  exit 1
fi

# The KrakenD binary records its own toolchain. `strings` is enough and avoids
# needing a Go toolchain inside the image, which the runtime image does not ship.
runtime_go="$(docker run --rm --entrypoint sh "$IMAGE" -c \
  'strings /usr/bin/krakend 2>/dev/null | grep -m1 "^go1\."' | tr -d '[:space:]')"

if [[ -z "$runtime_go" ]]; then
  echo "check-plugin-abi: could not read the Go version of /usr/bin/krakend in $IMAGE" >&2
  exit 1
fi

runtime_go="${runtime_go#go}"

if [[ "$builder_go" != "$runtime_go" ]]; then
  echo "check-plugin-abi: FAILED" >&2
  echo "  plugins/Dockerfile.builder pins Go $builder_go" >&2
  echo "  $IMAGE was built with Go $runtime_go" >&2
  echo "  Plugins built locally will fail to load with \"plugin was built with a" >&2
  echo "  different version of package runtime\". Align Dockerfile.builder with the" >&2
  echo "  image, or pin the image to a release built with Go $builder_go." >&2
  exit 1
fi

echo "check-plugin-abi: OK (builder and $IMAGE both on Go $runtime_go)"

# If plugins have been built locally, verify the artefacts themselves rather
# than trusting the Dockerfile. Skipped when absent (CI builds them through the
# production Dockerfile instead) or when no Go toolchain is available.
if ! compgen -G "$PLUGIN_BUILD_DIR/*.so" >/dev/null; then
  echo "check-plugin-abi: no built plugins in $PLUGIN_BUILD_DIR, skipping artefact check"
  exit 0
fi

if ! command -v go >/dev/null 2>&1; then
  echo "check-plugin-abi: go not on PATH, skipping artefact check"
  exit 0
fi

failed=0
for so in "$PLUGIN_BUILD_DIR"/*.so; do
  # `go version -m <file>` prints "<path>: go1.25.9". The path may contain
  # spaces (this repository lives under an iCloud path that does), so take the
  # last field, never the second.
  so_go="$(go version -m "$so" 2>/dev/null | head -1 | awk '{print $NF}')"
  so_go="${so_go#go}"
  if [[ "$so_go" != "$runtime_go" ]]; then
    echo "check-plugin-abi: FAILED" >&2
    echo "  $(basename "$so") was built with Go ${so_go:-unknown}, image is on Go $runtime_go" >&2
    echo "  Rebuild with: make plugin-build" >&2
    failed=1
  fi
done

if (( failed )); then
  exit 1
fi

echo "check-plugin-abi: OK ($(compgen -G "$PLUGIN_BUILD_DIR/*.so" | wc -l | tr -d ' ') built plugins on Go $runtime_go)"
