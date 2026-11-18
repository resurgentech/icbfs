#!/usr/bin/env bash
# split-gotest.sh PKG PATTERN... : runs PKG's tests as several `go test`
# processes at once, one per PATTERN (a -run pattern on top-level names,
# no "/") and one more for every test no PATTERN matches (-skip of them
# all), so the split is total by construction. Then it checks that the
# tests `go test -list` names for PKG are exactly the ones the processes
# ran, each once (count and names), and fails if not. Flags for go test:
# GOTEST_FLAGS (default "-race -timeout 25m"). Output: each process's
# ok/FAIL line, and for a failing process its failing tests' lines (or
# its last lines when no test failed: a build error, a panic, a timeout).
set -uo pipefail
pkg=$1
shift
flags=${GOTEST_FLAGS:--race -timeout 25m}
dir=$(mktemp -d)
trap 'rm -r "$dir"' EXIT
skip=$(IFS='|'; echo "$*")
n=0
pids=()
for p in "$@"; do
  n=$((n + 1))
  # shellcheck disable=SC2086
  go test $flags -v -run "$p" "$pkg" >"$dir/$n.out" 2>&1 &
  pids+=($!)
done
n=$((n + 1))
# shellcheck disable=SC2086
go test $flags -v -skip "$skip" "$pkg" >"$dir/$n.out" 2>&1 &
pids+=($!)
rc=0
for i in $(seq 1 "$n"); do
  f=$dir/$i.out
  if wait "${pids[$((i - 1))]}"; then
    grep -E '^ok ' "$f"
    continue
  fi
  rc=1
  if grep -q '^--- FAIL' "$f"; then
    awk '/^=== RUN   [^\/]+$/ {buf = ""}
         {buf = buf $0 "\n"}
         /^--- FAIL/ {printf "%s", buf; buf = ""}
         /^--- (PASS|SKIP)/ {buf = ""}
         /^FAIL\t/ {print}' "$f"
  else
    tail -n 60 "$f"
  fi
done
# The check: listed == ran, each once.
# shellcheck disable=SC2086
go test $flags -list '.*' "$pkg" 2>/dev/null | grep -E '^(Test|Example|Fuzz)' | sort >"$dir/listed"
cat "$dir"/*.out | sed -nE 's/^--- (PASS|FAIL|SKIP): ([^ ]+) .*/\2/p' | sort >"$dir/ran"
if ! cmp -s "$dir/listed" "$dir/ran"; then
  echo "split-gotest: $pkg: the splits did not run each listed test exactly once ($(wc -l <"$dir/listed") listed, $(wc -l <"$dir/ran") ran)" >&2
  echo "  listed, not run: $(comm -23 "$dir/listed" <(sort -u "$dir/ran") | tr '\n' ' ')" >&2
  echo "  run, not listed: $(comm -13 "$dir/listed" <(sort -u "$dir/ran") | tr '\n' ' ')" >&2
  echo "  run more than once: $(uniq -d "$dir/ran" | tr '\n' ' ')" >&2
  rc=1
else
  echo "split-gotest: $pkg: $(wc -l <"$dir/ran") tests in $n processes, each listed test run once"
fi
exit $rc
