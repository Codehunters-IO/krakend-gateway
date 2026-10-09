#!/usr/bin/env bash
# Runs `krakend check -d -t -c <template>` against the KrakenD version this
# repository actually targets, whatever is on PATH.
#
# Why this exists. The config declares `"version": 3`, which KrakenD 2.x reads
# and KrakenD 3.x refuses outright:
#
#   ERROR parsing the configuration file: unsupported version: 3 (want: 4)
#
# A `brew upgrade` is enough to bring 3.x in, and then every config target in
# the Makefile fails on a repository that is perfectly fine — a failure that
# reads like broken configuration and is not. CI never saw it, because CI
# shims `krakend` to the pinned image.
#
# So: use the local binary when its version matches the pin, otherwise run the
# pinned image, mounting the workspace at an IDENTICAL path so every absolute
# path in FC_SETTINGS and FC_OUT resolves the same inside and out. Fail with
# an explicit message only when neither is available.
#
# Usage: krakend-check.sh <template> [extra krakend args...]
#        KRAKEND_IMAGE=krakend:2.13.4 overrides the pin read from the Makefile.
#
# FC_ENABLE, FC_SETTINGS and FC_OUT are forwarded. Under the container path,
# FC_OUT must live inside the repository: the host temp dir is not mounted.
set -euo pipefail

TEMPLATE="${1:?usage: krakend-check.sh <template> [args...]}"
shift || true

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The pin lives in the Makefile and is read from there, so there is exactly one
# place to bump it.
if [[ -z "${KRAKEND_IMAGE:-}" ]]; then
  KRAKEND_IMAGE="$(sed -nE 's/^KRAKEND_IMAGE[[:space:]]*=[[:space:]]*(.+)$/\1/p' \
    "$ROOT_DIR/Makefile" | head -1 | tr -d '[:space:]')"
fi

if [[ -z "$KRAKEND_IMAGE" ]]; then
  echo "krakend-check: could not read KRAKEND_IMAGE from the Makefile" >&2
  exit 1
fi

want="${KRAKEND_IMAGE##*:}"
have=""
if command -v krakend >/dev/null 2>&1; then
  # `krakend version` prints to stderr, not stdout. Capturing only stdout
  # leaves $have empty, which silently skips the version comparison and the
  # "running the pinned image instead" notice with it.
  have="$(krakend version 2>&1 | sed -nE 's/^KrakenD Version:[[:space:]]*([0-9.]+).*/\1/p' | head -1)"
fi

if [[ -n "$have" && "$have" == "$want" ]]; then
  exec krakend check -d -t -c "$TEMPLATE" "$@"
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "krakend-check: FAILED" >&2
  if [[ -z "$have" ]]; then
    echo "  No krakend on PATH and no docker to fall back to." >&2
  else
    echo "  The krakend on PATH is $have, this repository targets $want, and there" >&2
    echo "  is no docker to fall back to. KrakenD 3.x rejects this config outright:" >&2
    echo "  it wants \"version\": 4 and the repository declares 3." >&2
  fi
  echo "  Install $KRAKEND_IMAGE, or docker, and retry." >&2
  exit 1
fi

if [[ -n "$have" ]]; then
  echo "krakend-check: local krakend is $have, this repository targets $want — running $KRAKEND_IMAGE instead." >&2
fi

exec docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e FC_ENABLE -e FC_SETTINGS -e FC_OUT \
  -v "$ROOT_DIR:$ROOT_DIR" \
  -w "$PWD" \
  "$KRAKEND_IMAGE" check -d -t -c "$TEMPLATE" "$@"
