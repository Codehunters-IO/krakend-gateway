#!/usr/bin/env bash
# The deploy gate for the one failure mode this gateway has that fails OPEN.
#
# Measured 2026-10-09 against krakend:2.13.4: with all five plugins declared and
# the plugin folder empty, KrakenD logs
#
#     [PLUGIN: Server] No plugins registered for the module
#
# at DEBUG level, starts, and serves every request with no plugin in the chain.
# GET /api/projects reached the backend: no signature check, no issuer check, no
# session resolution, no 401. Every other failure mode here fails closed — JWKS
# not loaded is a 503, Valkey down is a 503, a plugin that refuses its own
# config leaves only itself out. This one takes out all five at once, and the
# only signal is a line nobody reads.
#
# Why this is a log gate and not a request probe. A probe cannot tell the edge
# apart from the backend: measured, a protected route answers 401 whether
# jwt-headers rejected it or the backend did. Distinguishing them means matching
# on a response detail the backend could also produce, and the gate would then
# pass while the edge was absent. The boot log is unambiguous and covers all
# five plugins rather than the one a probe can reach.
#
# Usage: check-plugins-loaded.sh [container]
#        PLUGINS="a b" check-plugins-loaded.sh      to expect a subset
#
# With no argument the container is resolved through compose, because the
# gateway service declares no container_name and the generated one carries the
# project prefix.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -n "${1:-}" ]]; then
  CONTAINER="$1"
else
  CONTAINER="$(cd "$ROOT_DIR" && docker compose ps -q krakend 2>/dev/null | head -1)"
  if [[ -z "$CONTAINER" ]]; then
    echo "check-plugins-loaded: the compose service 'krakend' is not running" >&2
    echo "  Start it with 'make up', or pass a container name as the argument." >&2
    exit 1
  fi
fi

# The expected set comes from the Makefile, so adding a plugin updates the gate.
if [[ -z "${PLUGINS:-}" ]]; then
  PLUGINS="$(sed -nE 's/^PLUGINS[[:space:]]*=[[:space:]]*(.+)$/\1/p' "$ROOT_DIR/Makefile" | head -1)"
fi

if ! docker inspect "$CONTAINER" >/dev/null 2>&1; then
  echo "check-plugins-loaded: no container named $CONTAINER" >&2
  echo "  Start it first (make up), or pass the name as the first argument." >&2
  exit 1
fi

logs="$(docker logs "$CONTAINER" 2>&1 || true)"

if grep -q "No plugins registered for the module" <<<"$logs"; then
  echo "check-plugins-loaded: FAILED" >&2
  echo "  KrakenD registered NO plugins. The edge is serving every request with" >&2
  echo "  no JWT validation, no session resolution and no trace context." >&2
  echo "  Usual cause: the .so files are missing from /opt/krakend/plugins, or" >&2
  echo "  were built with a different Go toolchain (make plugins-abi)." >&2
  echo "  Do not route traffic to this container." >&2
  exit 1
fi

missing=()
for plugin in $PLUGINS; do
  if ! grep -q "\"msg\":\"plugin loaded\",\"plugin\":\"krakend-$plugin\"" <<<"$logs"; then
    missing+=("krakend-$plugin")
  fi
done

if (( ${#missing[@]} )); then
  echo "check-plugins-loaded: FAILED" >&2
  echo "  These plugins never logged \"plugin loaded\":" >&2
  printf '    %s\n' "${missing[@]}" >&2
  echo "  A plugin that refuses its own configuration logs the reason and KrakenD" >&2
  echo "  keeps serving WITHOUT it — so this is not a crash, it is a silently" >&2
  echo "  reduced edge. Grep the container's log for the plugin's own error." >&2
  echo "  session-resolver refuses to register when INTERNAL_SHARED_SECRET is" >&2
  echo "  empty, which is the most common cause." >&2
  exit 1
fi

echo "check-plugins-loaded: OK ($(wc -w <<<"$PLUGINS" | tr -d ' ') plugins loaded in $CONTAINER)"
