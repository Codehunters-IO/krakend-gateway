#!/usr/bin/env bash
# Guards the one property that broke silently and was invisible to
# `krakend check`, to the rendered JSON read by eye, and to every plugin
# logging "plugin loaded": KrakenD's plugin/http-server executes the
# *last* entry of "name" first, so declaring this array in chain order
# instead of reversed makes jwt-headers run before session-resolver and
# reject every cookie-only request before session-resolver ever sees it.
# See the chain-order section of docs/session-flow.md for how this was
# found and confirmed.
#
# This script renders the Flexible Configuration, inspects the resulting
# plugin/http-server.name array, and fails loudly if session-resolver and
# jwt-headers are not adjacent with jwt-headers last — the one layout that
# is actually safe under KrakenD's reverse-execution rule.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SETTINGS_DIR="$ROOT_DIR/config/settings"
TEMPLATE="$ROOT_DIR/config/krakend.tmpl"
OUT_FILE="$(mktemp -t krakend-chain-order.XXXXXX.json)"
trap 'rm -f "$OUT_FILE"' EXIT

if ! command -v jq >/dev/null 2>&1; then
  echo "check-plugin-chain-order: jq is required but not found on PATH" >&2
  exit 1
fi

FC_ENABLE=1 \
FC_SETTINGS="$SETTINGS_DIR" \
FC_OUT="$OUT_FILE" \
krakend check -d -t -c "$TEMPLATE" >/dev/null

names=$(jq -c '(.extra_config."plugin/http-server".name // [])' "$OUT_FILE")
has_session=$(echo "$names" | jq 'any(. == "krakend-session-resolver")')
has_jwt=$(echo "$names" | jq 'any(. == "krakend-jwt-headers")')

if [[ "$has_session" != "true" || "$has_jwt" != "true" ]]; then
  # One or both disabled (SESSION_ENABLED=false / JWT_ENABLED=false) — the
  # ordering constraint between them is moot in that configuration.
  echo "check-plugin-chain-order: OK (session-resolver and/or jwt-headers disabled, nothing to check)"
  exit 0
fi

# KrakenD executes the DECLARED array in reverse (see the header comment).
# So for jwt-headers to execute LAST (closest to the router, after
# session-resolver has had a chance to inject the bearer) it must be
# declared FIRST — and session-resolver, which must execute immediately
# before jwt-headers, must be declared immediately AFTER it.
first=$(echo "$names" | jq -r '.[0]')
second=$(echo "$names" | jq -r '.[1] // empty')

if [[ "$first" != "krakend-jwt-headers" ]]; then
  echo "check-plugin-chain-order: FAILED" >&2
  echo "  plugin/http-server.name must start with krakend-jwt-headers — KrakenD" >&2
  echo "  executes the LAST declared entry FIRST, so declaring jwt-headers first" >&2
  echo "  is what makes it execute LAST, closest to the router." >&2
  echo "  The first entry is instead: $first" >&2
  echo "  Declared order was: $names" >&2
  echo "  See the chain-order section of docs/session-flow.md before touching this array." >&2
  exit 1
fi

if [[ "$second" != "krakend-session-resolver" ]]; then
  echo "check-plugin-chain-order: FAILED" >&2
  echo "  krakend-session-resolver must be declared immediately after" >&2
  echo "  krakend-jwt-headers, so it executes immediately BEFORE it." >&2
  echo "  The second entry is instead: $second" >&2
  echo "  Declared order was: $names" >&2
  echo "  See the chain-order section of docs/session-flow.md before touching this array." >&2
  exit 1
fi

echo "check-plugin-chain-order: OK (krakend-jwt-headers declared first / executes last, krakend-session-resolver immediately before it)"
