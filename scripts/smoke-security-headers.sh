#!/usr/bin/env bash
# The confirmation ADR-0001 asked for and never got: the security headers on a
# real response off the socket, not in the rendered template.
#
# `make check` proves the template renders a security/http block. It cannot
# prove KrakenD applies it, that the values survive the module's own mapping
# (frame_deny -> X-Frame-Options: DENY), or that HSTS stays off while
# sts_seconds is 0. Three different claims; only the socket answers them.
#
# Two things measured on 2026-10-09 while writing this, both asserted below
# rather than worked around:
#
#   * /__health, the endpoint ADR-0001 named as the target, carries NONE of the
#     security headers. security/http does not wrap KrakenD's own built-in
#     endpoint, so the test the ADR prescribed was unsatisfiable as written and
#     would have failed for a reason unrelated to the decision.
#   * Until the same day, the edge answered 401 to /__health at all, because it
#     was missing from jwt-headers' skip_paths.
#
# What this does NOT cover, stated so nobody mistakes it for coverage: the
# gateway runs here with every plugin flag off, so these responses never pass
# through the plugin chain. Plugins wrap the router, so a denial written by
# jwt-headers or session-resolver answers BEFORE security/http and carries its
# own headers instead (see the deny helper in plugins/jwt-headers). A smoke test
# for that path needs the built .so files and belongs with them.
#
# Usage: smoke-security-headers.sh [port]
set -euo pipefail

PORT="${1:-18443}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTAINER="krakend-smoke-$$"
RENDER="$ROOT_DIR/.smoke-$$.json"

KRAKEND_IMAGE="$(sed -nE 's/^KRAKEND_IMAGE[[:space:]]*=[[:space:]]*(.+)$/\1/p' \
  "$ROOT_DIR/Makefile" | head -1 | tr -d '[:space:]')"

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -f "$RENDER"
}
trap cleanup EXIT

if [[ ! -s "$ROOT_DIR/certs/server.crt" || ! -s "$ROOT_DIR/certs/server.key" ]]; then
  echo "smoke-security-headers: no certificate at certs/server.{crt,key}" >&2
  echo "  TLS is enabled (config/settings/tls.json), and HSTS only renders over" >&2
  echo "  TLS. Run: make tls-dev-cert" >&2
  exit 1
fi

# A plugin-free render, so no .so files are needed and the job does not depend
# on krakend/builder. The argument is HSTS_SECONDS: that assertion runs in both
# directions and the value is fixed at render time.
# render_with_plugins renders the real configuration -- every flag at its
# settings default -- and is never run. It exists for one assertion the socket
# test cannot make: this gateway runs with every plugin disabled, so /__health
# answers 200 here whether or not it is in jwt-headers' skip_paths. Mutation
# testing caught that: removing /__health from skip_paths changed nothing. The
# fact is checkable in the render, so it is checked there.
render_with_plugins() {
  FC_ENABLE=1 \
  FC_SETTINGS="$ROOT_DIR/config/settings" \
  FC_OUT="$RENDER" \
  "$ROOT_DIR/scripts/krakend-check.sh" "$ROOT_DIR/config/krakend.tmpl" >/dev/null 2>&1
}

render() {
  JWT_ENABLED=false \
  SESSION_ENABLED=false \
  GATEWAY_TIMEOUT_ENABLED=false \
  ACCEPT_LANGUAGE_ENABLED=false \
  TRACE_CONTEXT_ENABLED=false \
  HSTS_SECONDS="$1" \
  FC_ENABLE=1 \
  FC_SETTINGS="$ROOT_DIR/config/settings" \
  FC_OUT="$RENDER" \
  "$ROOT_DIR/scripts/krakend-check.sh" "$ROOT_DIR/config/krakend.tmpl" >/dev/null 2>&1
}

start() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker run -d --name "$CONTAINER" -p "$PORT:8090" \
    --user "$(id -u):$(id -g)" \
    -v "$RENDER:/etc/krakend/krakend.json:ro" \
    -v "$ROOT_DIR/certs:/etc/krakend/certs:ro" \
    "$KRAKEND_IMAGE" run -c /etc/krakend/krakend.json >/dev/null

  for _ in $(seq 1 40); do
    if [[ "$(curl -ks -o /dev/null -w '%{http_code}' "https://localhost:$PORT/__health" || true)" == "200" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "smoke-security-headers: the gateway never answered /__health with 200" >&2
  docker logs "$CONTAINER" 2>&1 | tail -20 >&2
  exit 1
}

# A run killed between start and trap leaves a container holding the port, and
# the next run then dies with a docker name conflict that looks nothing like the
# real cause. Clear any of ours before starting.
docker ps -aq --filter "name=krakend-smoke-" | xargs -r docker rm -f >/dev/null 2>&1 || true

fail=0

want_header() {
  local name="$1" value="$2" headers="$3" got
  got="$(printf '%s' "$headers" | tr -d '\r' | sed -nE "s/^$name:[[:space:]]*(.*)$/\1/Ip" | head -1)"
  if [[ "$got" != "$value" ]]; then
    echo "  FAILED $name = ${got:-<absent>}, want $value" >&2
    fail=1
  else
    echo "  ok     $name: $got"
  fi
}

# A GET, not the HEAD that `curl -I` sends: KrakenD answers HEAD on a GET-only
# route with 405, and asserting headers on a 405 is asserting on the wrong
# response. /api/ping's backend is not running, so this is an upstream error —
# which is the case worth covering, because an error response that loses its
# security headers is the one nobody looks at.
probe() { curl -ks -o /dev/null -D - "https://localhost:$PORT$1"; }

echo "smoke-security-headers: $KRAKEND_IMAGE on :$PORT"

echo "  --- /__health in the real render, with the plugins enabled ---"
render_with_plugins
for plugin in krakend-jwt-headers krakend-session-resolver; do
  if jq -e --arg p "$plugin" \
      '(.extra_config["plugin/http-server"][$p].skip_paths // []) | index("/__health")' \
      "$RENDER" >/dev/null 2>&1; then
    echo "  ok     $plugin skips /__health"
  else
    echo "  FAILED $plugin does not skip /__health, so it answers 401 to every" >&2
    echo "         liveness probe. The plugins wrap the whole server, not the router." >&2
    fail=1
  fi
done

render 0
start

echo "  --- /__health off the socket, with the plugins disabled ---"
if [[ "$(curl -ks -o /dev/null -w '%{http_code}' "https://localhost:$PORT/__health")" == "200" ]]; then
  echo "  ok     answers 200"
else
  echo "  FAILED /__health does not answer 200" >&2
  fail=1
fi
if probe /__health | grep -qiE '^(x-frame-options|content-security-policy|x-content-type-options):'; then
  echo "  note   /__health now carries security headers: security/http has started" >&2
  echo "         wrapping KrakenD's own endpoint. An improvement — update this test" >&2
  echo "         and ADR-0001's confirmation note." >&2
  fail=1
else
  echo "  ok     carries no security headers, as measured — this is why the"
  echo "         assertions below run against a routed endpoint instead"
fi

headers="$(probe /api/ping)"

echo "  --- ADR-0001's three, on a routed endpoint ---"
# Measured: frame_deny: false does NOT remove this header in 2.13.4 — the module
# answers DENY either way, so this assertion cannot fail by flipping the flag,
# only by the block being dropped. Recorded in security_headers.json.
want_header "X-Frame-Options" "DENY" "$headers"
want_header "X-Content-Type-Options" "nosniff" "$headers"
want_header "Content-Security-Policy" "frame-ancestors 'none'" "$headers"
echo "  --- and the rest of the block ---"
want_header "Referrer-Policy" "strict-origin-when-cross-origin" "$headers"
want_header "X-XSS-Protection" "1; mode=block" "$headers"

# HSTS has to be asserted in both directions: sts_seconds is 0 while the edge
# runs behind a terminator over plain HTTP, and a header promising a year of
# HTTPS from a deployment that does not have it is worse than no header.
if printf '%s' "$headers" | grep -qiE '^strict-transport-security:'; then
  echo "  FAILED Strict-Transport-Security present with HSTS_SECONDS=0" >&2
  fail=1
else
  echo "  ok     Strict-Transport-Security absent while HSTS_SECONDS=0"
fi

render 31536000
start
want_header "Strict-Transport-Security" "max-age=31536000; includeSubDomains" "$(probe /api/ping)"

if (( fail )); then
  echo "smoke-security-headers: FAILED" >&2
  exit 1
fi

echo "smoke-security-headers: OK"
