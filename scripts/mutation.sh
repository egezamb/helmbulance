#!/usr/bin/env bash
# Mutation report: gremlins changes the code in small ways (a < into a <=, a == into a !=) and
# counts how many of the changes the unit tests notice. A change no test notices is a place where
# the code could be wrong with every unit test green. Prints a Markdown table.
#
#   scripts/mutation.sh [directory for the JSON results]
#
# Report only: the exit code says whether gremlins ran, not how good the score is. One run per
# package, because a run on the whole module counts tested packages as not covered.
#
# Needs Go and jq.
set -euo pipefail
cd "$(dirname "$0")/.."

GREMLINS_VERSION=v0.6.0
# gremlins gives a mutant as long as its first run of the unchanged tests took, times this. A mutant
# needs a rebuild and that first run may not, so the default leaves too little: with a warm build
# cache every mutant came out as "timed out".
TIMEOUT_COEFFICIENT=30

out=${1:-$(mktemp -d)}
mkdir -p "$out"
out=$(cd "$out" && pwd)
: > "$out/lived.md"
GOBIN="$out/bin" go install "github.com/go-gremlins/gremlins/cmd/gremlins@$GREMLINS_VERSION"

# The unit tests must pass as they are. This also builds everything, so that the first run of each
# package below measures the tests and not a cold build.
go test -count=1 ./... > "$out/baseline.log" 2>&1 || { cat "$out/baseline.log" >&2; exit 1; }

echo "### Mutation report"
echo
echo "Small changes to the code that the unit tests notice (killed) or do not (lived)."
echo
echo "| Package | Killed | Lived | Timed out | Not covered by a unit test |"
echo "|---|---|---|---|---|"
module=$(go list -m)
go list -f '{{.ImportPath}} {{len .TestGoFiles}}' ./... | while read -r pkg tests; do
  rel=${pkg#"$module"/}
  if [ "$tests" = 0 ]; then
    echo "| $rel | no unit tests | | | |"
    continue
  fi
  json="$out/${rel//\//_}.json"
  # -count=1: a cached test result would make that first run take no time at all.
  GOFLAGS="${GOFLAGS:-} -count=1" "$out/bin/gremlins" unleash --timeout-coefficient "$TIMEOUT_COEFFICIENT" \
    --output "$json" "./$rel" > "${json%.json}.log" 2>&1 < /dev/null
  jq -r --arg rel "$rel" '
    ([.files[]?.mutations[]? | select(.status == "TIMED OUT")] | length) as $timedout
    | "| \($rel) | \(.mutants_killed) | \(.mutants_lived) | \($timedout) | \(.mutants_not_covered) |"' "$json"
  jq -r --arg rel "$rel" '
    .files[]? | .file_name as $file | .mutations[] | select(.status == "LIVED")
    | "- `\($rel)/\($file):\(.line)` \(.type)"' "$json" >> "$out/lived.md"
done

echo
echo "Lived:"
echo
if [ -s "$out/lived.md" ]; then
  sort -t: -k1,1 -k2,2n "$out/lived.md"
else
  echo "none"
fi
