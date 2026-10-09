#!/usr/bin/env bash
# Fails when the template and the compose environment disagree in EITHER
# direction: a variable the template reads that compose never passes, or a
# variable compose passes that nothing reads.
#
# This is scripts/check-settings-orphan-keys.sh with the arrow reversed, and it
# guards the same class of defect from the other side. That script's header says
# dead configuration "is worse than missing configuration, because it reads as
# protection"; an unreachable override is worse still, because the operator sets
# it, no error appears, and the rendered config keeps the default.
#
# The image renders the template at container start (see the Dockerfile), and
# compose passes an explicit allowlist under `environment:` — there is no
# env_file and no pass-through. So a variable missing from that list is a knob
# that exists in the template, is documented, is accepted on the command line,
# and does nothing. JWT_ROLES_ENFORCE was exactly that: the one control the
# per-app authorization rollout depends on, unreachable in the only deployment
# this repository ships.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$ROOT_DIR/config/krakend.tmpl"
COMPOSE="$ROOT_DIR/docker-compose.yml"
SETTINGS_DIR="$ROOT_DIR/config/settings"

# Variables the template reads that compose deliberately does not pass, each
# with the reason. Keep this empty if you can.
EXEMPT=()

exempt() {
  local needle="$1" entry
  for entry in "${EXEMPT[@]:-}"; do
    [[ "${entry%%[[:space:]]*}" == "$needle" ]] && return 0
  done
  return 1
}

missing=()
checked=0
while IFS= read -r var; do
  [[ -n "$var" ]] || continue
  checked=$((checked + 1))
  if ! grep -qE "(^|[^A-Za-z0-9_])$var([^A-Za-z0-9_]|$)" "$COMPOSE" && ! exempt "$var"; then
    missing+=("$var")
  fi
done < <(grep -oE 'env "[A-Za-z_][A-Za-z0-9_]*"' "$TEMPLATE" |
  sed -E 's/env "([A-Za-z_][A-Za-z0-9_]*)"/\1/' | sort -u)

if (( ${#missing[@]} )); then
  echo "check-template-env: FAILED" >&2
  echo "  config/krakend.tmpl reads these, and docker-compose.yml passes none of them:" >&2
  printf '    %s\n' "${missing[@]}" >&2
  echo "  The image renders the template inside the container, so a variable the" >&2
  echo "  compose environment block omits is an override that silently does" >&2
  echo "  nothing. Add it as VAR=\${VAR:-} — an empty value falls through the" >&2
  echo "  template's | default and changes no rendered output — or list it in" >&2
  echo "  EXEMPT above with the reason it is set elsewhere." >&2
  exit 1
fi

echo "check-template-env: OK ($checked template variables, every one reachable from compose)"

# The other direction. KEYCLOAK_ISSUER and KEYCLOAK_JWKS_URL sat in compose for
# months with nothing reading them: the template interpolated jwt.json
# literally, so pointing the gateway at another realm meant editing a versioned
# file rather than setting a variable, and the two that looked like the way to
# do it did nothing. Checking one direction only would have missed that.
#
# A variable counts as read when the template names it literally, or when it is
# a backend host name, which the template reads as `env $e.host_env` with the
# NAMES living in endpoints.json.
RENDERER_OWN=("FC_ENABLE" "FC_SETTINGS" "FC_OUT" "FC_PARTIALS")

read_by_template() {
  local var="$1" own
  for own in "${RENDERER_OWN[@]}"; do
    [[ "$own" == "$var" ]] && return 0
  done
  grep -q "env \"$var\"" "$TEMPLATE" && return 0
  grep -q "\"host_env\"[[:space:]]*:[[:space:]]*\"$var\"" "$SETTINGS_DIR"/*.json 2>/dev/null && return 0
  return 1
}

orphans=()
passed=0
while IFS= read -r var; do
  [[ -n "$var" ]] || continue
  passed=$((passed + 1))
  read_by_template "$var" || orphans+=("$var")
done < <(sed -nE 's/^[[:space:]]*-[[:space:]]*([A-Z_][A-Z0-9_]*)=.*/\1/p' "$COMPOSE" | sort -u)

if (( ${#orphans[@]} )); then
  echo "check-template-env: FAILED" >&2
  echo "  docker-compose.yml passes these, and nothing reads them:" >&2
  printf '    %s\n' "${orphans[@]}" >&2
  echo "  A variable nobody reads is an override that looks like the way to" >&2
  echo "  change something and is not. Wire it into config/krakend.tmpl, or" >&2
  echo "  remove it from compose." >&2
  exit 1
fi

echo "check-template-env: OK ($passed compose variables, every one read by something)"

# A flag rendered from a conditional expression can emit the wrong JSON type,
# and for roles_enforce that is a fail-OPEN: the field is *bool, a non-boolean
# makes parseConfig error, registerHandlers refuses, and KrakenD then serves
# WITHOUT jwt-headers at all -- no signature check, no issuer check -- which is
# this repository's documented behaviour for a plugin that rejects its own
# config (see the Dockerfile, and the failure table in docs/session-flow.md).
# So the rendered type is asserted here, not only the Go side of it.
if ! command -v jq >/dev/null 2>&1; then
  echo "check-template-env: jq is required for the rendered-type check" >&2
  exit 1
fi

RENDER="$(mktemp "$ROOT_DIR/.template-env.XXXXXX.json")"
trap 'rm -f "$RENDER"' EXIT

FC_ENABLE=1 \
FC_SETTINGS="$ROOT_DIR/config/settings" \
FC_OUT="$RENDER" \
"$ROOT_DIR/scripts/krakend-check.sh" "$TEMPLATE" >/dev/null 2>&1

bad="$(jq -r '
  ["roles_enforce", "add_ip_header", "introspection_enabled"] as $fields
  | ((.extra_config["plugin/http-server"]["krakend-jwt-headers"]) // {}) as $block
  | $fields[]
  | select($block[.] != null and ($block[.] | type) != "boolean")
  | "krakend-jwt-headers.\(.) rendered as \($block[.] | tojson), a \($block[.] | type) and not a boolean"
' "$RENDER")"

if [[ -n "$bad" ]]; then
  echo "check-template-env: FAILED" >&2
  printf '    %s\n' "$bad" >&2
  echo "  A boolean plugin field rendered as another JSON type. The plugin would" >&2
  echo "  refuse its own config, and KrakenD keeps serving without it." >&2
  exit 1
fi

echo "check-template-env: OK (rendered boolean plugin fields are booleans)"
