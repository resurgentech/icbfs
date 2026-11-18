package icbfsdrive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// Tests of rule 1.6-8 of interloc's docs/STATES.md (T83-8): no entry of
// a folder is dated before the folder's first state, and no state of a
// folder is written from before it. An item older than its folder (made
// earlier, moved in later; a folder older than its parent) is on time in
// a folder being built. Rows LF-36, LF-37, LF-39, CH-17, ST-29, ST-30.

// folderKey is the key of the folder at p now.
func folderKey(t *testing.T, d *Drive, p string) string {
	t.Helper()
	n, err := d.resolve(system(), d.fs, drive.ByPath(p))
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return n.key
}

// folderDates is the dates of every version key's .version lists (a
// folder's root block and .metadata, a file's content and .metadata),
// the earliest, and the latest.
func folderDates(t *testing.T, d *Drive, key string) (first, last time.Time, n int) {
	t.Helper()
	dates, err := objstore.ReadDates(context.Background(), d.cfg.Store, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range dates {
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	return first, last, len(dates)
}

// distinctDates is the distinct dates key's .version lists, in order.
func distinctDates(t *testing.T, d *Drive, key string) string {
	t.Helper()
	dates, err := objstore.ReadDates(context.Background(), d.cfg.Store, key)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []string
	for _, at := range dates {
		if s := at.UTC().Format(time.RFC3339); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return sortedList(out)
}

// namesOrAbsent is the folder's names as of when (a view by its key), or
// "absent" if it did not exist then.
func namesOrAbsent(t *testing.T, d *Drive, folder string, when time.Time) string {
	t.Helper()
	fsys, err := d.at(system(), drive.At{Time: when})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fsys.ReadDir(system(), folder)
	if err != nil {
		return "absent"
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return sortedList(out)
}

func sortedList(list []string) string {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return strings.Join(list, ",")
}

// pathOrAbsent is the folder at p's names as of when, by its path, or
// "absent".
func pathOrAbsent(t *testing.T, d *Drive, p string, when time.Time) string {
	t.Helper()
	page, err := d.List(system(), drive.ByPath(p), drive.ListOptions{At: drive.At{Time: when}})
	if errors.Is(err, drive.ErrNotFound) {
		return "absent"
	}
	if err != nil {
		t.Fatalf("list %s as of %v: %v", p, when, err)
	}
	var out []string
	for _, it := range page.Items {
		out = append(out, it.Name)
	}
	return sortedList(out)
}

// TestLF36_AnItemOlderThanItsFolderIsOnTimeInAFolderBeingBuilt (LF-36):
// /dir is made in 2020 (its first state, and no later one). A file made
// in 2018 and moved in later, and a folder made in 2017 (older than its
// parent), are written into it: each is on time, with no row and no
// settle; /dir's entries are dated at its first state, 2020 (no version
// of /dir before it, none after it); the item's own record and versions
// keep their own dates; /dir did not exist before 2020 by its path or by
// its key, and holds all three from 2020 on.
func TestLF36_AnItemOlderThanItsFolderIsOnTimeInAFolderBeingBuilt(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("lf36"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	dir := folderKey(t, d, "/dir")
	if rows := r5Write(t, d, "/dir/old.txt", year(2018)); len(rows) != 0 {
		t.Errorf("a file older than its folder being built: rows %s, want none (on time)", rowsUntil(rows))
	}
	late := &drive.LateChanges{}
	sub, err := d.Mkdir(at(drive.WithLateChanges(ctx, late), 2017), "/dir/sub", false)
	if err != nil {
		t.Fatalf("a folder older than its parent: %v", err)
	}
	if rows := late.Take(); len(rows) != 0 {
		t.Errorf("a folder older than its parent being built: rows %s, want none (on time)", rowsUntil(rows))
	}
	first, last, _ := folderDates(t, d, dir)
	if !first.Equal(year(2020)) || !last.Equal(year(2020)) {
		t.Errorf("/dir's versions dated %s to %s; want every one at its first state, 2020",
			first.Format(time.RFC3339), last.Format(time.RFC3339))
	}
	old := folderKey(t, d, "/dir/old.txt")
	if f, l, _ := folderDates(t, d, old); !f.Equal(year(2018)) || !l.Equal(year(2018)) {
		t.Errorf("old.txt's own versions dated %s to %s; want its own date, 2018", f.Format(time.RFC3339), l.Format(time.RFC3339))
	}
	if f, _, _ := folderDates(t, d, sub.ID); !f.Equal(year(2017)) {
		t.Errorf("sub's own record first dated %s; want its own date, 2017", f.Format(time.RFC3339))
	}
	for _, c := range []struct {
		y    int
		want string
	}{{2016, "absent"}, {2017, "absent"}, {2018, "absent"}, {2019, "absent"}, {2020, "a.txt,old.txt,sub"}, {2021, "a.txt,old.txt,sub"}} {
		if got := namesOrAbsent(t, d, dir, midYear(c.y)); got != c.want {
			t.Errorf("/dir by its key as of mid-%d: %s, want %s", c.y, got, c.want)
		}
		if got := pathOrAbsent(t, d, "/dir", midYear(c.y)); got != c.want {
			t.Errorf("/dir by its path as of mid-%d: %s, want %s", c.y, got, c.want)
		}
	}
	// Still being built: a second one is on time too.
	if rows := r5Write(t, d, "/dir/older.txt", year(2016)); len(rows) != 0 {
		t.Errorf("a second item older than the folder: rows %s, want none", rowsUntil(rows))
	}
	if got := liveNames(t, d, dir); got != "a.txt,old.txt,older.txt,sub" {
		t.Errorf("/dir now: %s", got)
	}
}

// TestLF37_ALateItemOlderThanItsFolderWithLaterStates (LF-37, ST-29): /dir
// first in 2020, a later state in 2022; a file of 2018 is late as any: a
// row At 2018, Until 2022, written into the present dated 2022. Its settle
// plays it into [2020, 2022) only: no version of /dir is dated before
// 2020, /dir did not exist before 2020, and holds the file from 2020 on.
func TestLF37_ALateItemOlderThanItsFolderWithLaterStates(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("lf37"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2022, "/dir/b.txt")
	dir := folderKey(t, d, "/dir")
	rows := r5Write(t, d, "/dir/old.txt", year(2018))
	if len(rows) != 1 || !rows[0].At.Equal(year(2018)) || !rows[0].Until.Equal(year(2022)) || rows[0].Op != drive.LateAdd {
		t.Fatalf("rows %s; want one add, At 2018, Until 2022", rowsUntil(rows))
	}
	if _, last, _ := folderDates(t, d, dir); !last.Equal(year(2022)) {
		t.Errorf("/dir's newest version dated %s; want the present's, 2022", last.Format(time.RFC3339))
	}
	if _, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := folderDates(t, d, dir); !first.Equal(year(2020)) {
		t.Errorf("after the settle /dir's first version is dated %s; want its first state, 2020 (none written before it)", first.Format(time.RFC3339))
	}
	for _, c := range []struct {
		y    int
		want string
	}{{2018, "absent"}, {2019, "absent"}, {2020, "a.txt,old.txt"}, {2021, "a.txt,old.txt"}, {2022, "a.txt,b.txt,old.txt"}} {
		if got := namesOrAbsent(t, d, dir, midYear(c.y)); got != c.want {
			t.Errorf("/dir as of mid-%d: %s, want %s", c.y, got, c.want)
		}
	}
}

// TestLF39_AnItemOlderThanItsFolderFromACallerThatRefusesLateChanges
// (LF-39): on a drive that does not defer late folders, a file of 2018
// into /dir, first and only state 2020, is written dated at 2020, not
// refused. Once /dir has a later state (2022), a file of 2018 is refused
// as before.
func TestLF39_AnItemOlderThanItsFolderFromACallerThatRefusesLateChanges(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("lf39"), Config{})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	dir := folderKey(t, d, "/dir")
	if _, err := d.Write(at(ctx, 2018), "/dir/old.txt", strings.NewReader("old"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatalf("an item older than its folder being built, from a caller that refuses late changes: %v; want it written", err)
	}
	if first, last, _ := folderDates(t, d, dir); !first.Equal(year(2020)) || !last.Equal(year(2020)) {
		t.Errorf("/dir's versions dated %s to %s; want all at 2020", first.Format(time.RFC3339), last.Format(time.RFC3339))
	}
	if got := namesOrAbsent(t, d, dir, midYear(2019)); got != "absent" {
		t.Errorf("/dir as of mid-2019: %s, want absent", got)
	}
	if got := namesOrAbsent(t, d, dir, midYear(2020)); got != "a.txt,old.txt" {
		t.Errorf("/dir as of mid-2020: %s", got)
	}
	writeAt(t, d, ctx, 2022, "/dir/b.txt")
	_, err := d.Write(at(ctx, 2018), "/dir/older.txt", strings.NewReader("older"), drive.Condition{}, drive.WriteOptions{})
	if !errors.Is(err, drive.ErrLateChange) {
		t.Errorf("into a folder with later states: %v; want refused (ErrLateChange)", err)
	}
	if got := liveNames(t, d, dir); got != "a.txt,b.txt,old.txt" {
		t.Errorf("/dir now: %s", got)
	}
}

// TestCH17_TwoChangesToOneNameBeforeTheFirstState (CH-17): /dir first in
// 2020, later states in 2022. x.txt added in 2017 and removed in 2018,
// both late (rows Until 2022), handed to the settle newest first: both
// land at 2020 and play by their own dates, the add then the removal, so
// x.txt is in no state. Then y.txt added and removed in one year, 2017,
// the add first in the write: the same. No version of /dir is dated
// before 2020.
func TestCH17_TwoChangesToOneNameBeforeTheFirstState(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("ch17"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2022, "/dir/b.txt")
	dir := folderKey(t, d, "/dir")
	del := func(p string, y int) []drive.LateChange {
		t.Helper()
		late := &drive.LateChanges{}
		if err := d.Delete(at(drive.WithLateChanges(ctx, late), y), drive.ByPath(p), false, drive.Condition{}); err != nil {
			t.Fatal(err)
		}
		return late.Take()
	}
	addX := r5Write(t, d, "/dir/x.txt", year(2017))
	rmX := del("/dir/x.txt", 2018)
	if len(addX) != 1 || len(rmX) != 1 {
		t.Fatalf("rows: add %s, remove %s", rowsUntil(addX), rowsUntil(rmX))
	}
	if _, err := d.SettleFolder(ctx, append(rmX, addX...), drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	addY := r5Write(t, d, "/dir/y.txt", year(2017))
	rmY := del("/dir/y.txt", 2017)
	if len(addY) != 1 || len(rmY) != 1 {
		t.Fatalf("rows: add %s, remove %s", rowsUntil(addY), rowsUntil(rmY))
	}
	if _, err := d.SettleFolder(ctx, append(addY, rmY...), drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		y    int
		want string
	}{{2016, "absent"}, {2017, "absent"}, {2019, "absent"}, {2020, "a.txt"}, {2021, "a.txt"}, {2022, "a.txt,b.txt"}} {
		if got := namesOrAbsent(t, d, dir, midYear(c.y)); got != c.want {
			t.Errorf("/dir as of mid-%d: %s, want %s", c.y, got, c.want)
		}
	}
	if first, _, _ := folderDates(t, d, dir); !first.Equal(year(2020)) {
		t.Errorf("/dir's first version dated %s; want 2020", first.Format(time.RFC3339))
	}
}

// TestST29_ARowFromBeforeTheFirstStateIsPlayedFromIt (ST-29): a row
// whose At (2010) is long before /dir's first state (2020), as an older
// build stored it, of a file in the present since a late write; /dir has
// states in 2020, 2021, 2022 and 2023 (a later block split aside). The
// settle writes no state before 2020, and every state from 2020 on holds
// the file, and only the states the folder had.
func TestST29_ARowFromBeforeTheFirstStateIsPlayedFromIt(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("st29"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2021, "/dir/b.txt")
	writeAt(t, d, ctx, 2022, "/dir/c.txt")
	writeAt(t, d, ctx, 2023, "/dir/d.txt")
	dir := folderKey(t, d, "/dir")
	rows := r5Write(t, d, "/dir/old.txt", year(2021).AddDate(0, 3, 0))
	if len(rows) != 1 {
		t.Fatalf("rows %s", rowsUntil(rows))
	}
	before := distinctDates(t, d, dir)
	rows[0].At = year(2010)
	if _, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := folderDates(t, d, dir); !first.Equal(year(2020)) {
		t.Errorf("/dir's first version after the settle dated %s; want 2020 (no state written before the first)", first.Format(time.RFC3339))
	}
	if after := distinctDates(t, d, dir); after != before {
		t.Errorf("/dir's states are dated %s after the settle, %s before: want the folder's own states only", after, before)
	}
	for _, c := range []struct {
		y    int
		want string
	}{{2016, "absent"}, {2019, "absent"}, {2020, "a.txt,old.txt"}, {2021, "a.txt,b.txt,old.txt"}, {2022, "a.txt,b.txt,c.txt,old.txt"}, {2023, "a.txt,b.txt,c.txt,d.txt,old.txt"}} {
		if got := namesOrAbsent(t, d, dir, midYear(c.y)); got != c.want {
			t.Errorf("/dir as of mid-%d: %s, want %s", c.y, got, c.want)
		}
	}
}

// TestST30_ARowWhoseSpanFromTheFirstStateIsEmpty (ST-30): /dir has one
// state, 2020, holding old.txt; a row add old.txt At 2018 Until 2020 (as
// an older build stored it; LF-38's rebuilt row) is settled with nothing
// written: no request writes the store, and the settle reports the row
// settled, for the caller to delete. Not refused. A row invalid as
// stored (Until not after At) is still refused before anything is read
// (CH-12).
func TestST30_ARowWhoseSpanFromTheFirstStateIsEmpty(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	d := newDriveWith(t, store, cutCaseSettings("st30"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2020, "/dir/old.txt")
	dir := folderKey(t, d, "/dir")
	old := folderKey(t, d, "/dir/old.txt")
	row := drive.LateChange{Folder: dir, At: year(2018), Until: year(2020), Op: drive.LateAdd, Name: "old.txt", Item: old, Type: drive.TypeFile}
	var writes []string
	store.setWatch(func(op, key string) error {
		if isWrite(op) && !strings.HasSuffix(key, "-past") {
			writes = append(writes, op+" "+key)
		}
		return nil
	})
	s, err := d.SettleFolder(ctx, []drive.LateChange{row}, drive.SettleOptions{Path: "/dir"})
	store.setWatch(nil)
	if err != nil {
		t.Fatalf("a row whose span from the first state is empty: %v; want settled, not refused", err)
	}
	if s.Changes != 1 || s.Written != 0 {
		t.Errorf("settled %+v; want the row settled (Changes 1) and nothing written", s)
	}
	if len(writes) > 0 {
		t.Errorf("the settle wrote %d time(s): %v; want none", len(writes), writes)
	}
	if first, _, _ := folderDates(t, d, dir); !first.Equal(year(2020)) {
		t.Errorf("/dir's first version dated %s; want 2020", first.Format(time.RFC3339))
	}
	if got := namesOrAbsent(t, d, dir, midYear(2019)); got != "absent" {
		t.Errorf("/dir as of mid-2019: %s, want absent", got)
	}
	bad := row
	bad.At, bad.Until = year(2020), year(2019)
	if _, err := d.SettleFolder(ctx, []drive.LateChange{bad}, drive.SettleOptions{Path: "/dir"}); !errors.Is(err, drive.ErrInvalid) {
		t.Errorf("a row whose Until is not after its At as stored: %v; want refused (CH-12)", err)
	}
}
