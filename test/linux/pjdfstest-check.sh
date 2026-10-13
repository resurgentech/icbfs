#!/bin/bash
# Judges a pjdfstest run (prove's output) against pjdfstest-known.txt.
#
#   pjdfstest-check.sh <prove log> [known list]
#
# Exits 0 when every failing assertion is a listed, accepted difference.
# Anything else fails the run: an unlisted failing assertion, a test file
# that crashed or didn't finish (non-zero Wstat, parse errors), or no
# summary at all (the mount died). Listed assertions that now pass are
# reported too, so the list can be trimmed.
set -u
LOG="${1:?usage: pjdfstest-check.sh <prove log> [known list]}"
KNOWN="${2:-$(dirname "${BASH_SOURCE[0]}")/pjdfstest-known.txt}"

# expand "4-5, 8-9" into one number per line
expand() {
  tr ',' '\n' | tr -d ' ' | grep -v '^$' | while IFS=- read -r a b; do
    seq "$a" "${b:-$a}"
  done
}

if ! grep -q '^Result: ' "$LOG"; then
  echo "pjdfstest: no result in $LOG — the run didn't finish" >&2
  exit 1
fi
if grep -q '^All tests successful' "$LOG"; then
  failed=""
else
  # file:assertion for every failing assertion in prove's summary report.
  # "Failed test(s):" lists can wrap onto indented continuation lines;
  # "TODO passed:" lists are informational and skipped.
  failed=$(sed -n '/^Test Summary Report/,$p' "$LOG" | awk '
    /^\/.*\.t / {
      file = $1; sub(/.*\/tests\//, "", file); mode = ""
      if ($0 !~ /Wstat: 0 / || $0 ~ /Parse errors/) print file ":CRASH"
      next
    }
    /^  Failed tests?:/ { mode = "f"; sub(/^  Failed tests?: */, ""); print file ":" $0; next }
    /^  Parse errors/   { print file ":CRASH"; mode = ""; next }
    /^  [A-Z]/          { mode = ""; next }
    /^ +[0-9]/ && mode == "f" { sub(/^ +/, ""); print file ":" $0; next }
  ' | while IFS=: read -r file list; do
    if [ "$list" = CRASH ]; then echo "$file:CRASH"; continue; fi
    echo "$list" | expand | sed "s|^|$file:|"
  done | sort -u)
fi

known=$(grep -v '^#' "$KNOWN" | grep -v '^[[:space:]]*$' | while read -r file list _; do
  echo "$list" | expand | sed "s|^|$file:|"
done | sort -u)

unexpected=$(comm -23 <(echo "$failed" | grep -v '^$') <(echo "$known" | grep -v '^$'))
accepted=$(comm -12 <(echo "$failed" | grep -v '^$') <(echo "$known" | grep -v '^$'))
fixed=$(comm -13 <(echo "$failed" | grep -v '^$') <(echo "$known" | grep -v '^$'))

n() { echo -n "$1" | grep -c . ; }
echo "pjdfstest: $(n "$accepted") accepted (pjdfstest-known.txt), $(n "$unexpected") unexpected failures"
if [ -n "$fixed" ]; then
  echo "listed in $(basename "$KNOWN") but now passing — remove them:"
  echo "$fixed" | sed 's/^/  /'
fi
if [ -n "$unexpected" ]; then
  echo "unexpected failures:"
  echo "$unexpected" | sed 's/^/  /'
  exit 1
fi
exit 0
