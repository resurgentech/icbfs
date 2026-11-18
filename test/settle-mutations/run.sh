#!/usr/bin/env bash
# The settle's mutations (T83-1, from the final review of 2026-11-17 and
# round 7; FIRSTCLIP and FIRSTLATE, T83-8's rule 1.6-8; PS31, T83-9: the
# settle's progress reports removed): each patch breaks one row of interloc's docs/STATES.md
# section 1 in one or two lines, and that row's test must then fail.
# Run from the repository root on a clean tree (the recipe
# test-settle-review does):
#   test/settle-mutations/run.sh [NAME...]
# Exit 0 only if every mutation was caught (its tests went red).
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
dir=test/settle-mutations
declare -A TESTS=(
  [ST3]='TestSettleRecord$'
  [ST5]='TestST5_FirstPutFindsAnotherWriterInASplitFolder$'
  [ST6]='TestSettleStoppedByAnOutsideWrite$'
  [ST7]='TestSettleLostAnswerInWindow$'
  [ST9]='TestST9_LF17_ImmediateStopCutsTheSettle$|TestST24_ADeadlineIsAnImmediateStop$|TestST9_ImmediateStopAtEveryRequest$'
  [ST11]='TestRerunStopsOnAWriteSinceTheCut$'
  [ST14b]='TestST26_ALiveRunIsNeverEmptied$'
  [ST16]='TestST16_StopWhileErasingItsOwnBlocks$'
  [ST19]='TestLF13_FolderNotAtItsPathSaysSo$'
  [ST20]='TestLF13_FolderNotAtItsPathSaysSo$'
  [ST22]='TestST22_AnotherWriterDuringTheReadsIsRefused$'
  [ST27]='TestST27_AnotherWriterChangesOnlyTheRecordDuringTheReads$'
  [ST26]='TestST26_TableEmptiedUnderASettleFails$|TestST26_HeldSaysWhenTheTableWasEmptied$'
  [ST26AGE]='TestST26_ALiveRunIsNeverEmptied$'
  [LF27]='TestSettleRefusedWritesNothing$'
  [LF28]='TestLF28_DatingAndUndoFailTheRowsAreReported$'
  [CH4]='TestCH4_ProveOnlyWritesNothing$'
  [CH13]='TestCH13_LateChangeToTheFolderRecord$'
  [RECORDSUM]='TestCH5_SettleKeepsARecordOfManyAppKeys$'
  [ICBFSAT]='TestFR_Icbfsat$|TestUndatedThenLate$'
  [FIRSTCLIP]='TestST29_ARowFromBeforeTheFirstStateIsPlayedFromIt$|TestST30_ARowWhoseSpanFromTheFirstStateIsEmpty$'
  [FIRSTLATE]='TestLF36_AnItemOlderThanItsFolderIsOnTimeInAFolderBeingBuilt$|TestLF39_AnItemOlderThanItsFolderFromACallerThatRefusesLateChanges$'
  [PS31]='TestPS31_ASettleReportsEachTimeItMoves$'
)
names=("$@")
[ ${#names[@]} -eq 0 ] && names=(ST3 ST5 ST6 ST7 ST9 ST11 ST14b ST16 ST19 ST20 ST22 ST27 ST26 ST26AGE LF27 LF28 CH4 CH13 RECORDSUM ICBFSAT FIRSTCLIP FIRSTLATE PS31)
out=$(mktemp -d)
applied=""
trap '[ -n "$applied" ] && git apply -R "$applied"' EXIT
missed=0
for m in "${names[@]}"; do
  p="$dir/$m.patch"
  git apply "$p" || { echo "== $m: the patch does not apply"; missed=1; continue; }
  applied=$p
  ICBFS_PRE_MERGE=1 go test -count=1 -timeout 30m -run "${TESTS[$m]}" ./pkg/drive/icbfsdrive ./internal/objstore >"$out/$m.txt" 2>&1
  rc=$?
  git apply -R "$p"
  applied=""
  if [ $rc -eq 0 ]; then
    echo "== $m: GREEN, the break went unseen (${TESTS[$m]})"
    missed=1
  else
    echo "== $m: red (${TESTS[$m]})"
  fi
  grep -E '^(--- FAIL|panic)|_test.go:[0-9]+:' "$out/$m.txt" | head -5
done
echo "outputs: $out"
exit $missed
