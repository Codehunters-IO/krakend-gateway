#!/usr/bin/env bash
# Guards the first Confirmation item of ADR-0005: the edge accepts tokens from
# exactly ONE issuer, and nothing introduces a second one by accident.
#
# Why this needs a guard at all. The edge used to be heading for route→realm
# binding (ADR-0002 and its 2026-09-29 amendment), where several issuers were
# the point. ADR-0005 reversed that: one application realm, one issuer, and the
# multi-realm machinery is gone. Nothing in `krakend check` notices a second
# issuer reappearing — a native `auth/validator` block pasted into an endpoint,
# or a second key in jwt.json, both parse clean and both widen who can get in.
#
# Deliberately pure jq over the SOURCE settings, not the rendered config: this
# runs before the krakend render in `make check`, so it still answers on a box
# whose krakend binary has drifted from the pinned 2.13.4 (a `brew upgrade` to
# 3.x makes the render fail outright — config version 3 vs 4).
#
# Hardening still owed, and where it goes. ADR-0005 asks for the issuer's realm
# to be the platform realm (`codehunters`). The live config still points at
# `forgeos`, so asserting that today would be a red build against a decision
# nobody has implemented yet. Export JWT_PLATFORM_REALM to turn that half on —
# do it in the same change that moves the realm, and the guard becomes the full
# Confirmation item instead of half of it.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SETTINGS_DIR="$ROOT_DIR/config/settings"
TEMPLATE="$ROOT_DIR/config/krakend.tmpl"
EXPECTED_REALM="${JWT_PLATFORM_REALM:-}"

if ! command -v jq >/dev/null 2>&1; then
  echo "check-jwt-single-issuer: jq is required but not found on PATH" >&2
  exit 1
fi

# Every settings file is searched, not just jwt.json: an issuer can also enter
# through an endpoint's extra_config (KrakenD's own auth/validator takes one).
# Output is "<file>\t<json path>\t<value>" per declaration.
collect() {
  local key="$1" f rel
  for f in "$SETTINGS_DIR"/*.json; do
    [[ -f "$f" ]] || continue
    rel="config/settings/$(basename "$f")"
    jq -r --arg key "$key" --arg rel "$rel" '
      [paths(scalars) as $p | select(($p[-1] | tostring) == $key)
        | [$rel, ($p | map(tostring) | join(".")), (getpath($p) | tostring)]
        | @tsv]
      | .[]' "$f"
  done
}

issuers="$(collect issuer)"
jwks="$(collect jwks_url)"
issuer_count="$(printf '%s' "$issuers" | grep -c . || true)"
jwks_count="$(printf '%s' "$jwks" | grep -c . || true)"

if [[ "$issuer_count" -ne 1 ]]; then
  echo "check-jwt-single-issuer: FAILED" >&2
  echo "  expected exactly 1 issuer declaration across config/settings, found $issuer_count." >&2
  if [[ "$issuer_count" -eq 0 ]]; then
    echo "  An edge with no issuer validates nothing it claims to validate." >&2
  else
    echo "  A second issuer widens who the edge lets in, and ADR-0005 decided" >&2
    echo "  there is one application realm. Declared:" >&2
    sed 's/^/    /' <<<"$issuers" >&2
  fi
  echo "  See docs/adr/0005-one-platform-realm-and-master-as-operator-realm.md" >&2
  exit 1
fi

issuer_value="$(printf '%s' "$issuers" | cut -f3)"

# A literal realm URL in the template would bypass the settings entirely, and
# this guard with it. The template must only interpolate .jwt.issuer.
if grep -nE '"(issuer|jwks_url)"[[:space:]]*:[[:space:]]*"[^{]' "$TEMPLATE" >/dev/null 2>&1; then
  echo "check-jwt-single-issuer: FAILED" >&2
  echo "  config/krakend.tmpl hardcodes an issuer or jwks_url instead of" >&2
  echo "  interpolating .jwt.issuer / .jwt.jwks_url, which puts it out of reach" >&2
  echo "  of this guard and of the per-environment settings." >&2
  grep -nE '"(issuer|jwks_url)"[[:space:]]*:[[:space:]]*"[^{]' "$TEMPLATE" >&2
  exit 1
fi

realm_of() {
  # http://host:8083/realms/<name>/... -> <name>
  sed -nE 's#.*/realms/([^/]+).*#\1#p' <<<"$1"
}

issuer_realm="$(realm_of "$issuer_value")"
if [[ -z "$issuer_realm" ]]; then
  echo "check-jwt-single-issuer: FAILED" >&2
  echo "  the issuer does not look like a Keycloak realm URL: $issuer_value" >&2
  echo "  Expected something ending in /realms/<realm>." >&2
  exit 1
fi

# One issuer and one JWKS endpoint pointing at a different realm is the same
# hole wearing a different hat: the signature would be checked against keys
# from a realm the iss claim never names.
if [[ "$jwks_count" -ne 1 ]]; then
  echo "check-jwt-single-issuer: FAILED" >&2
  echo "  expected exactly 1 jwks_url declaration, found $jwks_count." >&2
  sed 's/^/    /' <<<"$jwks" >&2
  exit 1
fi

jwks_value="$(printf '%s' "$jwks" | cut -f3)"
jwks_realm="$(realm_of "$jwks_value")"

if [[ "$jwks_realm" != "$issuer_realm" ]]; then
  echo "check-jwt-single-issuer: FAILED" >&2
  echo "  issuer and jwks_url name different realms, so tokens would be checked" >&2
  echo "  against keys from a realm their iss claim never names." >&2
  echo "    issuer:   $issuer_value  (realm $issuer_realm)" >&2
  echo "    jwks_url: $jwks_value  (realm ${jwks_realm:-unparseable})" >&2
  exit 1
fi

if [[ -n "$EXPECTED_REALM" ]]; then
  if [[ "$issuer_realm" != "$EXPECTED_REALM" ]]; then
    echo "check-jwt-single-issuer: FAILED" >&2
    echo "  the edge issuer names realm '$issuer_realm', but the platform realm" >&2
    echo "  is '$EXPECTED_REALM' (JWT_PLATFORM_REALM)." >&2
    echo "  ADR-0005 makes the platform realm the only application realm." >&2
    exit 1
  fi
  echo "check-jwt-single-issuer: OK (one issuer, realm $issuer_realm, matches JWT_PLATFORM_REALM)"
  exit 0
fi

echo "check-jwt-single-issuer: OK (one issuer and one matching jwks_url, realm $issuer_realm)"
echo "check-jwt-single-issuer: NOTE — the realm itself is unchecked. ADR-0005 makes"
echo "  'codehunters' the platform realm while this config still names '$issuer_realm'."
echo "  Set JWT_PLATFORM_REALM in the change that moves the realm to close that half."
