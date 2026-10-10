#!/usr/bin/env bash
# Fails when two role rules overlap on the same request.
#
# Role rules on endpoints are configuration, declared when a surface needs
# them. cmd/gen already validates each rule on its own: claim path shape,
# non-empty any_of, claim required on protected, the dot-in-clientId trap, and
# roles on a public endpoint. What it cannot see is the interaction between two
# rules, because that only appears after the template collapses path parameters
# into globs:
#
#   /api/stories/{id}  ->  /api/stories/*
#
# plugins/jwt-headers unions every rule whose glob matches the request, and
# any_of means the union grants the MORE PERMISSIVE of the two. So two rules
# that look independent in endpoints.yaml can quietly widen each other. This
# script is the only thing that looks at that.
#
# With no rules declared it passes and says so: nothing to overlap.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPEC="$ROOT_DIR/endpoints.yaml"

if ! command -v python3 >/dev/null 2>&1; then
  echo "check-endpoint-authorization: python3 is required but not found on PATH" >&2
  exit 1
fi

python3 - "$SPEC" <<'PY'
import re
import sys

import yaml

spec = yaml.safe_load(open(sys.argv[1]))
endpoints = spec["endpoints"]
problems = []

def glob_of(endpoint):
    # The same transformation config/krakend.tmpl applies -- INCLUDING the
    # product prefix, which the template picks up because it derives from
    # endpoints.json (post-Normalize) while this script reads endpoints.yaml
    # (pre-Normalize). Comparing unprefixed paths would analyse strings the
    # plugin never sees: every product currently declares prefix "", so the
    # mistake would pass by luck and break on the first prefixed product.
    prefix = (spec.get("products", {}).get(endpoint.get("product"), {}) or {}).get("prefix", "")
    return re.sub(r"\{[^}]+\}", "*", prefix + endpoint["path"])

rules = {}  # glob -> set of (claim, sorted roles)
for e in endpoints:
    roles = e.get("roles")
    if not roles:
        continue
    any_of = tuple(sorted(roles.get("any_of") or []))
    rules.setdefault(glob_of(e), set()).add((roles.get("claim") or "", any_of))

# Two rules on one glob are ordinary -- GET and POST on a path collapse to the
# same glob. Two DIFFERENT rules on one glob are not: the plugin unions the
# matching rules and any-of means the more permissive silently wins.
for glob, variants in sorted(rules.items()):
    if len(variants) > 1:
        problems.append(f"{glob}: conflicting rules on one glob: {sorted(variants)}")

# Subsumption. A literal rule and a globbed one are different globs that can
# still match the same request, and the union grants the more permissive of the
# two. Identical rules are harmless; differing ones are the same silent
# widening as above, one step less visible.
def matches(pattern, concrete):
    p, c = pattern.strip("/").split("/"), concrete.strip("/").split("/")
    if len(p) != len(c):
        return False
    return all(a == "*" or a == b for a, b in zip(p, c))

globs = sorted(rules)
for a in globs:
    for b in globs:
        if a == b or "*" not in b:
            continue
        if matches(b, a) and rules[a] != rules[b]:
            problems.append(
                f"{a}: also matched by {b}, which declares a different rule -- "
                "the union grants the more permissive of the two"
            )

if problems:
    print("check-endpoint-authorization: FAILED", file=sys.stderr)
    for p in problems:
        print(f"    {p}", file=sys.stderr)
    print("  Overlapping role rules do not intersect, they union: the plugin", file=sys.stderr)
    print("  grants entry if ANY matching rule admits the token. Make the", file=sys.stderr)
    print("  overlapping rules identical, or make their paths not overlap.", file=sys.stderr)
    raise SystemExit(1)

gated = sum(1 for e in endpoints if e.get("roles"))
if gated == 0:
    print("check-endpoint-authorization: OK (no role rules declared, nothing to overlap)")
else:
    plural = "glob" if len(rules) == 1 else "globs"
    print(f"check-endpoint-authorization: OK ({gated} endpoints gated on roles, "
          f"{len(rules)} {plural}, no overlap)")
PY
