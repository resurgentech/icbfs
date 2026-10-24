#!/bin/bash
# The Linux copy tests, run as root on the test VM (run-linux.sh puts it
# there): each tool copies the test tree (tree.json, built fresh before
# every copy) onto a local disk (tmpfs) and onto the icbfs mount, and
# metatree compares the two copies. A difference is something icbfs keeps
# differently from a local disk.
#
#   copy-tests-linux.sh METATREE TREE_JSON MOUNT RESULTS_DIR
set -uo pipefail
METATREE="$1" TREE="$2" MNT="$3" OUT="$4"
LOCAL=/mnt/meta-local
mkdir -p "$LOCAL" "$OUT"
findmnt "$LOCAL" >/dev/null || mount -t tmpfs -o size=1g tmpfs "$LOCAL"

# Each tool copies $1 (a tree) to $2 (a new directory), keeping all it can.
tool_cp_a()        { cp -a "$1/." "$2/"; }
tool_cp_rp()       { cp -rp "$1/." "$2/"; }
tool_rsync_aAXH()  { rsync -aAXH "$1/" "$2/"; }
tool_rsync_aAXHU() { rsync -aAXHU "$1/" "$2/"; }
tool_tar()         { (cd "$1" && tar --xattrs --xattrs-include='*' --acls -cpf - .) | (cd "$2" && tar --xattrs --xattrs-include='*' --acls -xpf -); }
TOOLS="cp_a cp_rp rsync_aAXH rsync_aAXHU tar"

fresh_source() {
  rm -rf "$LOCAL/src"
  "$METATREE" build "$TREE" "$LOCAL/src" || { echo "building the tree failed" >&2; exit 2; }
}

# The tree on a local disk must be exactly the description, or the test
# proves nothing.
fresh_source
if ! "$METATREE" check "$TREE" "$LOCAL/src" > "$OUT/source-check.txt" 2>&1; then
  echo "FAIL  the tree on tmpfs isn't its description: $OUT/source-check.txt" >&2
  exit 2
fi

fail=0
printf '%-14s %-12s %s\n' tool "vs tmpfs" "kept of the description (on icbfs)"
for t in $TOOLS; do
  # Times from here on were set by the copies, not kept from the tree
  # (whose times are years old): a minute's margin for clocks.
  since=$(date -u -d '-1 minute' +%Y-%m-%dT%H:%M:%SZ)
  rm -rf "$LOCAL/ref-$t" "$MNT/$t"
  mkdir -p "$LOCAL/ref-$t" "$MNT/$t"
  fresh_source
  "tool_$t" "$LOCAL/src" "$LOCAL/ref-$t" > "$OUT/$t.ref.log" 2>&1
  ref_rc=$?
  fresh_source # its access times untouched by the first copy
  "tool_$t" "$LOCAL/src" "$MNT/$t" > "$OUT/$t.icbfs.log" 2>&1
  icbfs_rc=$?
  # Each copy read once: reading moves access times.
  "$METATREE" dump "$LOCAL/ref-$t" > "$OUT/$t.ref.json"
  "$METATREE" dump "$MNT/$t" > "$OUT/$t.icbfs.json"
  "$METATREE" diff -since "$since" "$OUT/$t.ref.json" "$OUT/$t.icbfs.json" > "$OUT/$t.diff.txt" 2>/dev/null
  n=$(grep -c . "$OUT/$t.diff.txt")
  "$METATREE" check "$TREE" "$OUT/$t.icbfs.json" > "$OUT/$t.check.txt" 2>/dev/null
  kept=$(grep -c . "$OUT/$t.check.txt")
  status="same"
  if [ "$n" -gt 0 ] || [ "$ref_rc" != "$icbfs_rc" ]; then
    status="$n differ"
    fail=1
  fi
  [ "$ref_rc" != "$icbfs_rc" ] && status="$status (exit $ref_rc on tmpfs, $icbfs_rc on icbfs)"
  printf '%-14s %-12s %s\n' "$t" "$status" "$kept fields not kept (the tool's own choice if tmpfs matches)"
done
exit $fail
