#!/usr/bin/env bash
# Fails on a settings key that config/krakend.tmpl never reads.
#
# Dead configuration is worse than missing configuration, because it reads as
# protection. config/settings/rate_limit.json carried endpoint_max_rate: 100
# and endpoint_client_max_rate: 20 for months; no template path read either,
# so the only per-endpoint limits in force were the two declared in
# endpoints.yaml. README.md documented both dead keys as live limits, and a
# design spec reasoned about one of them as "current". Nothing failed: KrakenD
# never sees a key the template does not interpolate, so `krakend check` is
# silent by construction.
#
# The rule: for every config/settings/*.json object, each top-level key must
# appear in the template as `.<namespace>.<key>`. A namespace the template
# marshals whole (`{{ marshal .client_tls }}`) is exempt — every key in it is
# read by definition.
#
# Escape hatch: a key read by a Go plugin rather than by the template is a
# legitimate exception. Add it to ALLOWLIST below WITH a reason. An empty
# allowlist is the honest state today; keep it that way if you can.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SETTINGS_DIR="$ROOT_DIR/config/settings"
TEMPLATE="$ROOT_DIR/config/krakend.tmpl"

# Entries are "<namespace>.<key>  # reason". None needed so far.
ALLOWLIST=()

if ! command -v python3 >/dev/null 2>&1; then
  echo "check-settings-orphan-keys: python3 is required but not found on PATH" >&2
  exit 1
fi

allowed() {
  local needle="$1" entry
  for entry in "${ALLOWLIST[@]:-}"; do
    [[ "${entry%%[[:space:]]*}" == "$needle" ]] && return 0
  done
  return 1
}

orphans=()
checked=0

for file in "$SETTINGS_DIR"/*.json; do
  [[ -f "$file" ]] || continue
  ns="$(basename "$file" .json)"

  # Whole-object marshal: every key is read, nothing to check.
  if grep -qE "marshal[[:space:]]+\.$ns([^._[:alnum:]]|$)" "$TEMPLATE"; then
    continue
  fi

  keys="$(python3 - "$file" <<'PY'
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except json.JSONDecodeError as exc:
    sys.exit(f"invalid json: {exc}")
if isinstance(doc, dict):
    print("\n".join(k for k in doc if not k.startswith("@")))
PY
)"

  while IFS= read -r key; do
    [[ -n "$key" ]] || continue
    checked=$((checked + 1))
    if ! grep -qF ".$ns.$key" "$TEMPLATE" && ! allowed "$ns.$key"; then
      orphans+=("config/settings/$ns.json: $key  (template never reads .$ns.$key)")
    fi
  done <<<"$keys"
done

if (( ${#orphans[@]} )); then
  echo "check-settings-orphan-keys: FAILED" >&2
  echo "  These keys are declared in settings and read by nothing:" >&2
  printf '    %s\n' "${orphans[@]}" >&2
  echo "  Dead configuration reads as protection it does not provide. Either wire" >&2
  echo "  the key into config/krakend.tmpl, or delete it — and delete whatever" >&2
  echo "  documentation claims it is in force." >&2
  echo "  If a Go plugin reads it instead of the template, add it to ALLOWLIST in" >&2
  echo "  $(basename "${BASH_SOURCE[0]}") with a reason." >&2
  exit 1
fi

echo "check-settings-orphan-keys: OK ($checked keys, every one read by the template)"
