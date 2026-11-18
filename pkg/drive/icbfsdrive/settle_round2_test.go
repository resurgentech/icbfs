package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// writeAt writes path (its parents made) in year y.
func writeAt(t *testing.T, d *Drive, ctx context.Context, y int, path string) drive.Item {
	t.Helper()
	it, err := d.Write(at(ctx, y), path, strings.NewReader(path), drive.Condition{}, drive.WriteOptions{Parents: true})
	if err != nil {
		t.Fatalf("write %s in %d: %v", path, y, err)
	}
	return it
}

// writeSettled writes path late in year y on a drive that defers late
// folders, and settles what it reported: what a late write did inside the
// write before T83-1 round 3, through the public calls.
func writeSettled(t *testing.T, d *Drive, ctx context.Context, y int, path string) drive.Item {
	t.Helper()
	late := &drive.LateChanges{}
	it := writeAt(t, d, drive.WithLateChanges(ctx, late), y, path)
	byFolder := map[string][]drive.LateChange{}
	var order []string
	for _, c := range late.Take() {
		if byFolder[c.Folder] == nil {
			order = append(order, c.Folder)
		}
		byFolder[c.Folder] = append(byFolder[c.Folder], c)
	}
	for _, f := range order {
		if _, err := settleAt(d, ctx, byFolder[f], drive.SettleOptions{}); err != nil {
			t.Fatalf("settle the folder of %s: %v", path, err)
		}
	}
	return it
}

// putSettled is writeSettled with body as the file's content.
func putSettled(t *testing.T, d *Drive, ctx context.Context, y int, path, body string) {
	t.Helper()
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), y), path, strings.NewReader(body), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatalf("write %s late in %d: %v", path, y, err)
	}
	if changes := late.Take(); len(changes) > 0 {
		if _, err := settleAt(d, ctx, changes, drive.SettleOptions{}); err != nil {
			t.Fatalf("settle the folder of %s: %v", path, err)
		}
	}
}

// folderID is the id of the folder at p.
func folderID(t *testing.T, d *Drive, p string) string {
	t.Helper()
	it, err := d.Stat(system(), drive.ByPath(p), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	return it.ID
}

// TestSettleReadsAFreshCache (review finding 3): the folder's past is
// read into the history cache; ReplaceFolderHistory rewrites it (views
// told to look again); a late write follows with no view read between.
// Its settle reads the past as the store has it now, not as the cache
// kept it: no state lists an entry from a later year.
func TestSettleReadsAFreshCache(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "freshcache", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	ctx := system()
	ids := map[string]string{}
	for _, w := range []struct {
		y    int
		path string
	}{{2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}, {2024, "/dir/c.txt"}} {
		ids[w.path] = writeAt(t, d, ctx, w.y, w.path).ID
	}
	for _, y := range []int{2020, 2021, 2022, 2023, 2024} {
		names(t, d, "/dir", midYear(y)) // into the cache
	}
	e := func(n string) drive.FolderEntry {
		return drive.FolderEntry{Name: n, ID: ids["/dir/"+n], Type: drive.TypeFile}
	}
	hist := []drive.FolderState{
		{At: year(2020), Entries: []drive.FolderEntry{e("a.txt")}},
		{At: year(2021), Entries: []drive.FolderEntry{e("a.txt"), e("b.txt")}},
		{At: year(2023), Entries: []drive.FolderEntry{e("a.txt"), e("b.txt"), e("c.txt")}},
	}
	if err := d.ReplaceFolderHistory(ctx, drive.ByPath("/dir"), hist, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	writeSettled(t, d, ctx, 2022, "/dir/x.txt")
	pastIs(t, d, map[int]string{
		2020: "a.txt",
		2021: "a.txt,b.txt",
		2022: "a.txt,b.txt,x.txt",
		2023: "a.txt,b.txt,c.txt,x.txt",
		2024: "a.txt,b.txt,c.txt,x.txt",
	})
}

// TestDeferredPastAtTheLateDate pins what a view of the past shows while
// a late change is deferred (the owner, 2026-11-17: "Yeah, that's
// fine"): a deferred late write at 2021 into a folder of a@2020, b@2022,
// c@2024 is written into the present dated 2024: views before 2024 show
// the folder as it was, without x; from 2024 on, with it. Every view is a
// state the folder had.
func TestDeferredPastAtTheLateDate(t *testing.T) {
	d, changes := cutCase(t, miniotest.Store(t), "deferredpast", Config{DeferLateFolders: true})
	pastIs(t, d, map[int]string{
		2020: "a.txt",
		2021: "a.txt",
		2022: "a.txt,b.txt",
		2023: "a.txt,b.txt",
		2024: "a.txt,b.txt,c.txt,x.txt",
	})
	if _, err := settleAt(d, system(), changes, drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
}

// splitCase is a folder of ten files, one a year from 2016 but 2021, in
// blocks of four entries (split), and a late write at 2021.
func splitCase(t *testing.T, store objstore.Store, name string, cfg Config) (*Drive, []drive.LateChange) {
	t.Helper()
	d := newDriveWith(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 4}, cfg)
	ctx := system()
	for i, y := range []int{2016, 2017, 2018, 2019, 2020, 2022, 2023, 2024, 2025, 2026} {
		writeAt(t, d, ctx, y, fmt.Sprintf("/dir/f%c.txt", 'a'+i))
	}
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/late.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	return d, late.Take()
}

// replaceLeftUnreachable and replaceGrew are what the per-file repair
// this step replaced (ReplaceDirHistory, icbfs f9e4777) left in
// splitCase, measured: twelve unreachable objects, all .version records
// of blocks only the past reads (deleted on top), and 577 bytes more in
// the store's current objects (DiskUsage).
const (
	replaceLeftUnreachable = 12
	replaceGrew            = 577
)

// TestSettleLeavesNoMoreThanReplace (review finding 5): a settle of a
// split folder leaves no block current that nothing reaches (each
// written state's blocks but the last's, and each old block no state
// from now on reads, get a deletion on top, as ReplaceDirHistory did),
// and grows the store's current objects no more than the per-file
// repair did. The .version records it leaves unreachable are those of
// blocks only the past reads: the written states' (as the repair's),
// and the old blocks the states before the settle's first still read
// (which the repair, rewriting every state, erased); so two more here.
func TestSettleLeavesNoMoreThanReplace(t *testing.T) {
	d, changes := splitCase(t, miniotest.Store(t), "nomore", Config{DeferLateFolders: true})
	ctx := system()
	rep, err := d.fs.Check(ctx)
	if err != nil || rep.Unreachable != 0 {
		t.Fatalf("before: %d unreachable, %v", rep.Unreachable, err)
	}
	before, err := d.fs.DiskUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := settleAt(d, ctx, changes, drive.SettleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := d.fs.DiskUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rep, err = d.fs.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("settled %+v: %d unreachable (the repair: %d), current objects grew %d bytes (the repair: %d)",
		s, rep.Unreachable, replaceLeftUnreachable, after-before, replaceGrew)
	settleLeftovers(t, d, changes[0].Folder, replaceLeftUnreachable+2)
	if after-before > replaceGrew {
		t.Errorf("current objects grew %d bytes, the repair %d", after-before, replaceGrew)
	}
}

// TestTwoDrivesOneHistoryCache (review finding 6): a library opens every
// drive it serves over one history cache. Drive a's settle changes the
// past; drive b, over the same cache and filesystem, which had read that
// past, reads it as it is now once it next looks at the past-changed
// mark, though drive a told the cache it had seen that mark.
func TestTwoDrivesOneHistoryCache(t *testing.T) {
	ctx := context.Background()
	store := miniotest.Store(t)
	cache, err := objstore.OpenHistoryCache(ctx, store, filepath.Join(t.TempDir(), "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	s := icbfs.Settings{Name: "twodrives", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}
	a := newDriveWith(t, store, s, Config{History: cache, DeferLateFolders: true})
	sys := system()
	for _, w := range []struct {
		y    int
		path string
	}{{2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}, {2024, "/dir/c.txt"}} {
		writeAt(t, a, sys, w.y, w.path)
	}
	b := newDriveWith(t, store, s, Config{History: cache, Root: "/dir"})
	saved := pastCheck
	pastCheck = 0
	t.Cleanup(func() { pastCheck = saved })
	if got := sortedAt(t, b, "/", midYear(2021)); got != "a.txt" {
		t.Fatalf("drive b, mid-2021: %s", got)
	}
	writeSettled(t, a, sys, 2021, "/dir/x.txt")
	time.Sleep(pastMarkEvery + 100*time.Millisecond)
	a.past.flush()
	b.past.flush()
	if got := sortedAt(t, b, "/", midYear(2021)); got != "a.txt,x.txt" {
		t.Errorf("drive b, mid-2021, after drive a's settle and mark: %s, want a.txt,x.txt", got)
	}
	if got := sortedAt(t, a, "/dir", midYear(2021)); got != "a.txt,x.txt" {
		t.Errorf("drive a, mid-2021: %s", got)
	}
}

// TestSettleRemoveOfOneItem (review finding 7): a late removal of a name
// ends that item's lifetimes, not a later occupant's. b (item 1) since
// 2020; on time in 2023, b removed and written anew (item 2); then a
// late removal of b's item 1 at 2021: b is gone from 2021 to 2023, and
// item 2 is b from 2023 on, as the present says.
func TestSettleRemoveOfOneItem(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "removeone", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2019, "/dir/a.txt")
	b1 := writeAt(t, d, ctx, 2020, "/dir/b.txt")
	if err := d.Delete(at(ctx, 2023), drive.ByPath("/dir/b.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	b2 := writeAt(t, d, ctx, 2023, "/dir/b.txt")
	if b1.ID == b2.ID {
		t.Fatalf("b written anew is the same item")
	}
	folder := folderID(t, d, "/dir")
	// As a late removal written into the present at 2023 would report it.
	rm := []drive.LateChange{{Folder: folder, At: year(2021), Until: year(2023), Op: drive.LateRemove, Name: "b.txt", Item: b1.ID, Type: drive.TypeFile}}
	if _, err := settleAt(d, ctx, rm, drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	pastIs(t, d, map[int]string{2020: "a.txt,b.txt", 2021: "a.txt", 2022: "a.txt", 2023: "a.txt,b.txt"})
	if got := read(t, ctx, d, drive.ByPath("/dir/b.txt"), drive.At{Time: midYear(2023)}); got != "/dir/b.txt" {
		t.Errorf("b as of mid-2023: %q", got)
	}
}

// TestSettleRefusesTimes (review finding 7): a change at a zero time,
// before 1970 or in the future is refused, with nothing written.
func TestSettleRefusesTimes(t *testing.T) {
	d, changes := cutCase(t, miniotest.Store(t), "refusetimes", Config{DeferLateFolders: true})
	folder := changes[0].Folder
	for _, when := range []time.Time{{}, time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().Add(time.Hour)} {
		bad := append([]drive.LateChange{{Folder: folder, At: when, Op: drive.LateTouch}}, changes...)
		if _, err := settleAt(d, system(), bad, drive.SettleOptions{}); !errors.Is(err, drive.ErrInvalid) {
			t.Errorf("a change at %v: %v, want ErrInvalid", when, err)
		}
	}
	pastIs(t, d, map[int]string{2022: "a.txt,b.txt"}) // nothing settled
}

// TestSettleFolderChecksTheFolder (review finding 7): whoever asks, the
// folder settled is a directory under the drive's root: a file's id is
// refused, and so is a folder outside the root of a drive rooted below
// the filesystem's.
func TestSettleFolderChecksTheFolder(t *testing.T) {
	store := miniotest.Store(t)
	d, changes := cutCase(t, store, "checkfolder", Config{DeferLateFolders: true})
	sys := system()
	file, err := d.Stat(sys, drive.ByPath("/dir/a.txt"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	ch := changes[0]
	ch.Folder = file.ID
	if _, err := settleAt(d, sys, []drive.LateChange{ch}, drive.SettleOptions{}); !errors.Is(err, drive.ErrNotDir) {
		t.Errorf("a file's id: %v, want ErrNotDir", err)
	}
	writeAt(t, d, sys, 2020, "/other/o.txt")
	sub := newDriveWith(t, store, icbfs.Settings{Name: "checkfolder"}, Config{Root: "/other", DeferLateFolders: true})
	if _, err := sub.SettleFolder(sys, changes, drive.SettleOptions{Path: "/dir"}); !errors.Is(err, drive.ErrFolderNotAtPath) {
		t.Errorf("a folder outside the drive's root: %v, want ErrFolderNotAtPath", err)
	}
	if _, err := settleAt(d, sys, changes, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Errorf("the folder, its path given: %v", err)
	}
}

// TestSettleLowerErasures (the blocks below a split folder's root): a
// settle's record names the old blocks below the root it may clear; after
// the settle each of them is erased whole (no version, no .version
// record) or kept with a deletion on top and only versions dated before
// the first state it wrote; both happen here. The new blocks it records
// are reached by its states.
func TestSettleLowerErasures(t *testing.T) {
	store := miniotest.Store(t)
	d, changes := splitCase(t, store, "lowererase", Config{DeferLateFolders: true})
	folder := changes[0].Folder
	k := &keeper{}
	var rec drive.SettleRecord
	opts := k.opts()
	record := opts.Record
	opts.Record = func(r drive.SettleRecord) error { rec = r; return record(r) }
	s, err := settleAt(d, system(), changes, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Folder != folder || len(rec.Old) == 0 || len(rec.Create) == 0 {
		t.Fatalf("record %+v", rec)
	}
	ctx := context.Background()
	whole, kept := 0, 0
	for _, k := range rec.Old {
		h, err := store.History(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		vh, err := store.History(ctx, k+objstore.VersionSuffix)
		if err != nil {
			t.Fatal(err)
		}
		if len(h) == 0 {
			whole++
			if len(vh) != 0 {
				t.Errorf("%s erased whole: %d versions of its record left", k, len(vh))
			}
			continue
		}
		kept++
		if !h[0].Deleted {
			t.Errorf("%s kept: no deletion on top", k)
		}
		dates, err := objstore.ReadDates(ctx, store, k)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range h {
			if !o.Deleted && !dates.Time(&o).Before(changes[0].At) {
				t.Errorf("%s kept a version dated %v, from the settle's first state on", k, dates.Time(&o))
			}
		}
	}
	t.Logf("settled %+v: old blocks below the root erased whole %d, kept with a deletion on top %d", s, whole, kept)
	if whole == 0 || kept == 0 {
		t.Errorf("old blocks erased whole %d, kept %d: want both", whole, kept)
	}
	settleLeftovers(t, d, folder, replaceLeftUnreachable+2)
}

// TestSettleSplitByTheLateWrite (review finding 8): in a folder of two
// full blocks below its root, a late write lands in a full block and
// splits it (a key no dated state reaches); the settle puts the file in
// the past from its time, keeps the present, and leaves nothing behind.
// STATES: CH-11
func TestSettleSplitByTheLateWrite(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "latesplit", Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 4}, Config{DeferLateFolders: true})
	ctx := system()
	// Its history written whole, so its blocks are full (as a history
	// written state by state lays them out): f0 to f3, f4 to f7.
	var entries []drive.FolderEntry
	var hist []drive.FolderState
	for i := 0; i < 8; i++ {
		it := writeAt(t, d, ctx, 2016+i, fmt.Sprintf("/dir/f%d.txt", i))
		entries = append(entries, drive.FolderEntry{Name: fmt.Sprintf("f%d.txt", i), ID: it.ID, Type: drive.TypeFile})
		hist = append(hist, drive.FolderState{At: year(2016 + i), Entries: append([]drive.FolderEntry(nil), entries...)})
	}
	if err := d.ReplaceFolderHistory(ctx, drive.ByPath("/dir"), hist, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	folder := folderID(t, d, "/dir")
	_, keysBefore, err := d.fs.ReadDirKeys(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2019), "/dir/f0b.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, keysAfter, err := d.fs.ReadDirKeys(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(keysAfter) <= len(keysBefore) {
		t.Fatalf("the late write didn't split a block: %d blocks below the root before, %d after", len(keysBefore), len(keysAfter))
	}
	if _, err := settleAt(d, ctx, late.Take(), drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	all := "f0.txt,f0b.txt,f1.txt,f2.txt,f3.txt,f4.txt,f5.txt,f6.txt,f7.txt"
	pastIs(t, d, map[int]string{
		2016: "f0.txt",
		2017: "f0.txt,f1.txt",
		2018: "f0.txt,f1.txt,f2.txt",
		2019: "f0.txt,f0b.txt,f1.txt,f2.txt,f3.txt",
		2020: "f0.txt,f0b.txt,f1.txt,f2.txt,f3.txt,f4.txt",
		2021: "f0.txt,f0b.txt,f1.txt,f2.txt,f3.txt,f4.txt,f5.txt",
		2022: "f0.txt,f0b.txt,f1.txt,f2.txt,f3.txt,f4.txt,f5.txt,f6.txt",
		2023: all,
	})
	if got := liveNames(t, d, folder); got != all {
		t.Errorf("the folder now: %s", got)
	}
	settleLeftovers(t, d, folder, 64)
}

// TestSettleLateRemovesAndMovesSplit (review finding 8): in a split
// folder, a late delete and a late move out (to another folder) and in
// (from it) are reported and settled: each in the past from its time,
// the present as it is, both folders' pasts right.
func TestSettleLateRemovesAndMovesSplit(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "latemoves", Size: 1 << 30, RootMode: 0o777, Created: year(2005), DirBlockEntries: 4}, Config{DeferLateFolders: true})
	ctx := system()
	for i := 0; i < 10; i++ {
		writeAt(t, d, ctx, 2010+i, fmt.Sprintf("/dir/f%d.txt", i))
	}
	writeAt(t, d, ctx, 2010, "/other/o.txt")
	writeAt(t, d, ctx, 2024, "/other/p.txt")
	late := &drive.LateChanges{}
	lctx := drive.WithLateChanges(ctx, late)
	if err := d.Delete(at(lctx, 2013), drive.ByPath("/dir/f1.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(at(lctx, 2015), drive.ByPath("/dir/f2.txt"), "/other/f2.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(at(lctx, 2016), drive.ByPath("/other/o.txt"), "/dir/o.txt", false); err != nil {
		t.Fatal(err)
	}
	byFolder := map[string][]drive.LateChange{}
	for _, c := range late.Take() {
		byFolder[c.Folder] = append(byFolder[c.Folder], c)
	}
	dir, other := folderID(t, d, "/dir"), folderID(t, d, "/other")
	if len(byFolder[dir]) != 3 || len(byFolder[other]) != 2 {
		t.Fatalf("late changes: %+v", byFolder)
	}
	for _, f := range []string{dir, other} {
		if _, err := settleAt(d, ctx, byFolder[f], drive.SettleOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pastIs(t, d, map[int]string{
		2012: "f0.txt,f1.txt,f2.txt",
		2013: "f0.txt,f2.txt,f3.txt",
		2015: "f0.txt,f3.txt,f4.txt,f5.txt",
		2016: "f0.txt,f3.txt,f4.txt,f5.txt,f6.txt,o.txt",
		2020: "f0.txt,f3.txt,f4.txt,f5.txt,f6.txt,f7.txt,f8.txt,f9.txt,o.txt",
	})
	for _, c := range []struct {
		y    int
		want string
	}{{2014, "o.txt"}, {2015, "f2.txt,o.txt"}, {2016, "f2.txt"}, {2024, "f2.txt,p.txt"}} {
		if got := sortedAt(t, d, "/other", midYear(c.y)); got != c.want {
			t.Errorf("/other as of mid-%d: %s, want %s", c.y, got, c.want)
		}
	}
	settleLeftovers(t, d, dir, 64)
	settleLeftovers(t, d, other, 64)
}

// TestSettleSameAtTwoWrites (review finding 8): two late writes at one
// time into one folder, each numbering its changes from 0, settled in
// one call in the order reported: both in the past from that time.
// STATES: CH-7
func TestSettleSameAtTwoWrites(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "sameat", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2024, "/dir/c.txt")
	late := &drive.LateChanges{}
	lctx := drive.WithLateChanges(ctx, late)
	if _, err := d.Move(at(lctx, 2022), drive.ByPath("/dir/a.txt"), "/dir/b.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(at(lctx, 2022), "/dir/a.txt", strings.NewReader("a2"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	changes := late.Take()
	seqs := map[int]int{}
	for _, c := range changes {
		seqs[c.Seq]++
	}
	if seqs[0] != 2 {
		t.Fatalf("two writes, each from 0: %+v", changes)
	}
	if _, err := settleAt(d, ctx, changes, drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	pastIs(t, d, map[int]string{2021: "a.txt", 2022: "a.txt,b.txt", 2024: "a.txt,b.txt,c.txt"})
	if got := read(t, ctx, d, drive.ByPath("/dir/a.txt"), drive.At{Time: midYear(2022)}); got != "a2" {
		t.Errorf("a as of mid-2022: %q, want the second write's", got)
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/b.txt"), drive.At{Time: midYear(2022)}); got != "/dir/a.txt" {
		t.Errorf("b as of mid-2022: %q, want the moved file's", got)
	}
}

// TestSettleRequestsBeyondOnePage (review finding 8): the step's gate
// (TestSettleRequestsBoundedByLaterStates, its numbers unchanged) with
// the store's listing page cut to 50 entries, so the folder's history is
// listed in more than one page: at 20 states its root block's listing
// (the root, its .metadata and .version, about 60 versions) takes two
// pages, at 200 states nine. The settle's requests are the same at
// both, and its heap within the gate's allowance. (With the real page,
// 1000, the same holds at 1100 states, measured once by hand: 568
// requests at 20 and at 1100, see the step's report.)
func TestSettleRequestsBeyondOnePage(t *testing.T) {
	if raceOn {
		t.Skip("measured without the race detector: `just test-settle`")
	}
	if testing.Short() {
		t.Skip("builds two folders of 300 entries")
	}
	saved := objstore.HistoryPageSize
	objstore.HistoryPageSize = 50
	t.Cleanup(func() { objstore.HistoryPageSize = saved })
	small := measureSettle(t, 20)
	large := measureSettle(t, 200)
	t.Logf("listing page %d: settle requests %d at 20 states, %d at 200; states written %d and %d; peak heap over the heap before: %s at 20, %s at 200",
		objstore.HistoryPageSize, small.requests, large.requests, small.written, large.written, kib(small.peak), kib(large.peak))
	if small.requests != large.requests {
		t.Errorf("a settle's requests: %d at 20 states, %d at 200; want the same:\n20: %s\n200: %s",
			small.requests, large.requests, summarize(small.seq), summarize(large.seq))
	}
	if small.written != large.written {
		t.Errorf("states written: %d at 20, %d at 200", small.written, large.written)
	}
	const allowance = 512 << 10 // TestSettleRequestsBoundedByLaterStates's
	if large.peak > small.peak+allowance {
		t.Errorf("peak heap during the settle: %s at 20 states, %s at 200; want no more than %s more",
			kib(small.peak), kib(large.peak), kib(allowance))
	}
	for _, c := range []settleCase{small, large} {
		checkWindow(t, c)
	}
}
