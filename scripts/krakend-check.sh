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
# Read below for why this is needed; it is also a reminder that `set -u` does
# not save you inside a process substitution. An undefined variable there kills
# only the subshell, so the loop reading from it simply sees no input and the
# script carries on as if the list were empty.
SETTINGS_DIR="${FC_SETTINGS:-$ROOT_DIR/config/settings}"

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

# Forwarding FC_* alone renders every `env "..."` lookup in the template as the
# empty string, because the container does not inherit the shell's environment.
# `make check` would still pass — empty values are valid config — while
# `make generate` would quietly write a krakend.json with no secrets, no hosts
# and no issuer in it. So forward exactly the variables the template reads,
# parsed out of the template itself rather than kept in a list here that would
# drift. `-e NAME` with no value passes the host's value when set and is
# ignored when not, which is the behaviour the `| default` pipelines expect.
#
# Two sources, because the template looks variables up in two ways. Literal
# `env "NAME"` is grep-able from the template. Backend hosts are not: the
# template does `env $e.host_env`, so the NAMES live in the generated
# endpoints.json (AUTH_BFF_HOST, FORGEOS_HOST, ...) and a grep over the
# template finds none of them. Missing those renders every backend host as its
# compiled-in default, which is the one failure that would look like working
# config pointed at the wrong services.
#
# bash 3.2 on macOS has no mapfile, hence the read loop.
env_args=(-e FC_ENABLE -e FC_SETTINGS -e FC_OUT)
while IFS= read -r var; do
  [[ -n "$var" ]] && env_args+=(-e "$var")
done < <(
  {
    grep -oE 'env "[A-Za-z_][A-Za-z0-9_]*"' "$TEMPLATE" |
      sed -E 's/env "([A-Za-z_][A-Za-z0-9_]*)"/\1/'
    grep -ohE '"host_env"[[:space:]]*:[[:space:]]*"[A-Za-z_][A-Za-z0-9_]*"' \
      "$SETTINGS_DIR"/*.json 2>/dev/null |
      sed -E 's/.*"([A-Za-z_][A-Za-z0-9_]*)"$/\1/'
  } | sort -u
)

exec docker run --rm \
  --user "$(id -u):$(id -g)" \
  "${env_args[@]}" \
  -v "$ROOT_DIR:$ROOT_DIR" \
  -w "$PWD" \
  "$KRAKEND_IMAGE" check -d -t -c "$TEMPLATE" "$@"
