#!/usr/bin/env bash
# Fails when any pinned KrakenD image could put this gateway on a release that
# does not load Go plugins.
#
# KrakenD CE 3.0 removed Go plugin support. Measured 2026-10-18 with the same
# configuration against both binaries:
#
#   krakend:2.13.4  DEBUG: [SERVICE: Plugin Loader] Starting loading process
#                   DEBUG: [PLUGIN: Server] No plugins registered for the module
#   krakend:3.0.1   (no plugin loader output at all)
#
# 3.0.1 accepts the `plugin` and `plugin/http-server` blocks, `krakend check`
# answers "Syntax OK!" and counts the component, the server boots clean -- and
# loads nothing. There is no `auth/validator` in the template, so every route
# would be served with no signature check, no introspection, no session
# resolution and no x-user-* injection. Dead configuration that reads as
# protection, which is the failure mode this repository refuses.
#
# `scripts/check-plugins-loaded.sh` catches it against a running container,
# which is late: this one catches it in `make check`, before anything is built.
#
# Two ways a pin can land on 3.x, and both fail here:
#
#   - an explicit bump to 3.y.z
#   - a floating tag. `krakend:latest` resolves to 3.0.1 as of 2026-10-18, so
#     a tag that is not an exact x.y.z is a 3.x waiting for the next pull.
#
# If plugins are ever removed from the template, this guard stops applying and
# a new ADR supersedes ADR-0009 -- it reads the template rather than assuming.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$ROOT_DIR/config/krakend.tmpl"
MAKEFILE="$ROOT_DIR/Makefile"
DOCKERFILE="$ROOT_DIR/Dockerfile"
COMPOSE="$ROOT_DIR/docker-compose.yml"

# Oldest major that cannot load plugins. ADR-0009 is the record.
FIRST_PLUGINLESS_MAJOR=3

for f in "$TEMPLATE" "$MAKEFILE" "$DOCKERFILE" "$COMPOSE"; do
  if [[ ! -f "$f" ]]; then
    echo "check-krakend-major: $f not found" >&2
    exit 1
  fi
done

# The guard is about plugins, so it asks the template whether there are any.
if ! grep -q 'plugin/http-server' "$TEMPLATE"; then
  echo "check-krakend-major: OK (the template declares no plugins, the pin is unconstrained)"
  exit 0
fi

# Every place a KrakenD image is pinned, as "<file>:<line> <tag>". Collected by
# matching the image names rather than the version, so a bump cannot hide from
# this list by changing the number.
pins="$(grep -nE '(^|[^a-zA-Z0-9_/-])krakend(/builder)?:[^[:space:]"]+' \
  "$MAKEFILE" "$DOCKERFILE" "$COMPOSE" |
  sed -E 's#.*(krakend(/builder)?:[^[:space:]"]+).*#\1#' |
  sort -u || true)"

if [[ -z "$pins" ]]; then
  echo "check-krakend-major: FAILED" >&2
  echo "  found no krakend image pin in the Makefile, Dockerfile or compose file." >&2
  echo "  Either the image names changed or this guard stopped seeing them; a" >&2
  echo "  guard that silently matches nothing is worse than no guard." >&2
  exit 1
fi

problems=()
for pin in $pins; do
  tag="${pin##*:}"
  # krakend/builder publishes 3.0.1-linux-generic alongside 3.0.1.
  version="${tag%-linux-generic}"

  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    problems+=("$pin pins the floating tag '$tag', which resolves to whatever is current")
    continue
  fi

  major="${version%%.*}"
  if (( major >= FIRST_PLUGINLESS_MAJOR )); then
    problems+=("$pin is KrakenD $major.x, which does not load Go plugins")
  fi
done

if (( ${#problems[@]} > 0 )); then
  echo "check-krakend-major: FAILED" >&2
  for p in "${problems[@]}"; do
    echo "    $p" >&2
  done
  echo "  This gateway's whole identity layer is Go plugins: jwt-headers," >&2
  echo "  session-resolver, trace-context, accept-language, gateway-timeout." >&2
  echo "  KrakenD CE ${FIRST_PLUGINLESS_MAJOR}.0 dropped plugin loading, and it fails silently --" >&2
  echo "  the config still validates and the server still boots, serving every" >&2
  echo "  route unauthenticated. See docs/adr/0009-staying-on-krakend-2-x.md." >&2
  echo "  Pin an exact 2.y.z, or supersede ADR-0009 first." >&2
  exit 1
fi

count="$(echo "$pins" | wc -l | tr -d ' ')"
echo "check-krakend-major: OK ($count distinct image pins, all exact 2.x, plugins can load)"
