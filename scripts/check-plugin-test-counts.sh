#!/usr/bin/env bash
# Fails when README.md's per-plugin test count disagrees with the number of
# Test* functions on disk.
#
# The table claims a number the reader cannot check, and the number had already
# drifted twice before this guard existed: session-resolver read 36 against 40
# real functions, jwt-headers 22 against 40. An understated count is the
# dangerous direction -- it is read as "this plugin is thinly tested", which
# invites a reviewer to wave through a change the suite would actually have
# caught, or to re-add coverage that is already there.
#
# This repository already fails the build for dead configuration and for
# unreachable overrides. A documented count nobody verifies is the same defect
# in prose: it reads as evidence and is not.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
README="$ROOT_DIR/README.md"

wrong=()
checked=0

for dir in "$ROOT_DIR"/plugins/*/; do
  [[ -f "$dir/go.mod" ]] || continue
  plugin="$(basename "$dir")"

  # The row is `| `name` | N | ...`; take the number from the second column.
  claimed="$(sed -nE "s/^\| \`$plugin\` \| +([0-9]+) \|.*/\1/p" "$README" | head -1)"
  if [[ -z "$claimed" ]]; then
    wrong+=("$plugin: no row in README.md's plugin test table")
    continue
  fi

  actual="$(grep -chE '^func Test' "$dir"/*_test.go 2>/dev/null | paste -sd+ - | bc)"
  checked=$((checked + 1))
  if [[ "$claimed" != "$actual" ]]; then
    wrong+=("$plugin: README says $claimed, $actual Test* functions on disk")
  fi
done

if (( ${#wrong[@]} )); then
  echo "check-plugin-test-counts: FAILED" >&2
  printf '    %s\n' "${wrong[@]}" >&2
  echo "  Update the table in README.md ('Funciones Test*'). A count that reads" >&2
  echo "  as evidence of coverage has to be the real one." >&2
  exit 1
fi

echo "check-plugin-test-counts: OK ($checked plugins, every README count matches the suite)"
