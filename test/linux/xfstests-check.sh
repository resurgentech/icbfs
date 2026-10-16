#!/bin/bash
# Judges an xfstests ./check log: every failed test must be listed, with a
# reason, in xfstests-known.txt. Prints the counts, unexpected failures,
# and listed tests that now pass; exits non-zero on an unexpected failure
# or a run that didn't finish.
#
#   test/linux/xfstests-check.sh <check.log>
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG="${1:?usage: xfstests-check.sh <check.log>}"
KNOWN="$HERE/xfstests-known.txt"

if ! grep -q "^Ran: " "$LOG"; then
  echo "xfstests: the run didn't finish (no \"Ran:\" line in $LOG)" >&2
  tail -20 "$LOG" >&2
  exit 1
fi
ran=$(grep "^Ran: " "$LOG" | tail -1 | wc -w)
notrun=$(grep "^Not run: " "$LOG" | tail -1 | wc -w)
failed=$(grep "^Failures: " "$LOG" | tail -1 | sed 's/^Failures: //')
known=$(grep -v '^#' "$KNOWN" 2>/dev/null | awk 'NF {print $1}' | sort -u)

unexpected=()
for t in $failed; do
  grep -qx "$t" <<<"$known" || unexpected+=("$t")
done
fixed=()
for t in $known; do
  grep -qw "$t" <<<"$(grep "^Ran: " "$LOG" | tail -1)" && ! grep -qw "$t" <<<"$failed" && fixed+=("$t")
done

echo "xfstests: $((ran - 1)) ran, $((notrun > 0 ? notrun - 2 : 0)) not run, $(wc -w <<<"$failed") failed (${#unexpected[@]} not in xfstests-known.txt)"
[ ${#fixed[@]} -eq 0 ] || echo "listed as failing but passed now: ${fixed[*]}"
if [ ${#unexpected[@]} -gt 0 ]; then
  echo "unexpected failures: ${unexpected[*]}"
  exit 1
fi
