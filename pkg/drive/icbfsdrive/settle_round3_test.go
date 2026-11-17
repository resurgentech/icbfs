package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// T83-1 round 3: the second review's findings (N1 to N6) on 9a067f4,
// each first as the review's probe (red there), then as built.

// storeSnapshot is every version of every object in store, the drive's
// past-changed mark aside: what a change that writes nothing leaves as
// it was.
func storeSnapshot(t *testing.T, store objstore.Store) string {
	t.Helper()
	objs, err := store.HistoryUnder(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, o := range objs {
		if strings.HasSuffix(o.Key, "-past") {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s %v", o.Key, o.VersionID, o.Deleted))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// folderAt is the entry names of the folder with key folder as of when,
// sorted, read by its key (it needn't be under the root now).
func folderAt(t *testing.T, d *Drive, folder string, when time.Time) string {
	t.Helper()
	fsys, err := d.at(system(), drive.At{Time: when})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fsys.ReadDir(system(), folder)
	if err != nil {
		return "error: " + err.Error()
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// writeUndated writes path with no source time.
func writeUndated(t *testing.T, d *Drive, path string) {
	t.Helper()
	if _, err := d.Write(system(), path, strings.NewReader(path), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatalf("write %s with no source time: %v", path, err)
	}
}

// TestUndatedWriteIsAState (N1): an ordinary write with no source time
// into the folder is a state of it at the time it was stored, though the
// folder's dates table doesn't list its root block's version (no undated
// write's is): a late write's settle keeps it in the present and dates
// it as it was, before (u, before the late write) and after (v, between
// the late write and its settle); and so does a settle cut in its window
// and run again with its record. Round 2 took such a version for an
// unfinished one: the entry left the present and the past was backdated.
func TestUndatedWriteIsAState(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(fmt.Sprintf("cut %v", cut), func(t *testing.T) {
			store := &failStore{Store: miniotest.Store(t)}
			name := fmt.Sprintf("undated%v", cut)
			d := newDriveWith(t, store, cutCaseSettings(name), Config{DeferLateFolders: true})
			ctx := system()
			writeAt(t, d, ctx, 2020, "/dir/a.txt")
			writeAt(t, d, ctx, 2022, "/dir/b.txt")
			writeUndated(t, d, "/dir/u.txt")
			folder := folderID(t, d, "/dir")
			late := &drive.LateChanges{}
			if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			writeUndated(t, d, "/dir/v.txt")
			changes := late.Take()
			k := &keeper{}
			if cut {
				// The store away from the window's second root put on.
				listed, puts := false, 0
				store.mu.Lock()
				store.watch = func(op, key string) error {
					switch {
					case op == "HISTORY" && key == folder:
						listed = true
					case listed && op == "PUT" && key == folder:
						if puts++; puts >= 2 {
							return errFail
						}
					}
					return nil
				}
				store.mu.Unlock()
				if _, err := d.SettleFolder(ctx, changes, k.opts()); !errors.Is(err, errFail) {
					t.Fatalf("a cut settle: %v", err)
				}
				store.mu.Lock()
				store.watch = nil
				store.mu.Unlock()
				if k.held() == nil {
					t.Fatal("a settle cut in its window, and no record held")
				}
				d = newDriveWith(t, store, cutCaseSettings(name), Config{DeferLateFolders: true})
			}
			if _, err := d.SettleFolder(ctx, changes, k.opts()); err != nil {
				t.Fatal(err)
			}
			if got := liveNames(t, d, folder); got != "a.txt,b.txt,u.txt,v.txt,x.txt" {
				t.Errorf("present after the settle: %s", got)
			}
			pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2025: "a.txt,b.txt,x.txt"})
			if got := sortedAt(t, d, "/dir", time.Now()); got != "a.txt,b.txt,u.txt,v.txt,x.txt" {
				t.Errorf("as of now: %s", got)
			}
			settleLeftovers(t, d, folder, 0)
		})
	}
}

// TestUndatedSplitThenLate (the review's P5, which held on 9a067f4 for
// the present, the only thing it looked at): in a split folder, dated a,
// b, c and undated u, v, w (the root rewritten undated when it splits),
// then a late write of x at 2021, settled: the present has all, the past
// x from 2021 and u, v, w only now.
//
// Round 3's KNOWN fault, gone in round 4: the late write is written into
// the present, dated at the folder's newest state, never behind it.
func TestUndatedSplitThenLate(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "undatedsplit", Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 4}, Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2022, "/dir/b.txt")
	writeAt(t, d, ctx, 2023, "/dir/c.txt")
	for _, n := range []string{"u", "v", "w"} {
		writeUndated(t, d, "/dir/"+n+".txt")
	}
	folder := folderID(t, d, "/dir")
	writeSettled(t, d, ctx, 2021, "/dir/x.txt")
	if got := liveNames(t, d, folder); got != "a.txt,b.txt,c.txt,u.txt,v.txt,w.txt,x.txt" {
		t.Errorf("present: %s", got)
	}
	pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
	settleLeftovers(t, d, folder, 64)
}

// TestLateWriteThatSplitsABlock was round 3's KNOWN fault (the settle
// refused, "f4.txt" twice): a folder split in blocks of four (f0 to f4 by
// 2020, f5 in 2022); a late write of x at 2021 lands in a full block and
// splits it. Written into the present, dated 2022, the split's root and
// halves are all of one date, every view is a state the folder had, and
// the settle puts x into 2021.
func TestLateWriteThatSplitsABlock(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "latesplits", Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 4}, Config{DeferLateFolders: true})
	ctx := system()
	for i, y := range []int{2016, 2017, 2018, 2019, 2020, 2022} {
		writeAt(t, d, ctx, y, fmt.Sprintf("/dir/f%d.txt", i))
	}
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	before := map[int]string{2020: "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt", 2021: "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt", 2022: "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt,f5.txt,x.txt"}
	pastIs(t, d, before)
	if _, err := d.SettleFolder(ctx, late.Take(), drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := liveNames(t, d, folderID(t, d, "/dir")); got != "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt,f5.txt,x.txt" {
		t.Errorf("present: %s", got)
	}
	pastIs(t, d, map[int]string{2020: "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt", 2021: "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt,x.txt", 2022: "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt,f5.txt,x.txt"})
}

// sameAtCase is a folder of a@2020 and b@2022 on a drive that defers
// late folders.
func sameAtCase(t *testing.T, name string) *Drive {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings(name), Config{DeferLateFolders: true})
	writeAt(t, d, system(), 2020, "/dir/a.txt")
	writeAt(t, d, system(), 2022, "/dir/b.txt")
	return d
}

// TestTwoLateFilesAtOneTime (reported by another lane on icbfs main
// 8aff656, where the per-file repair drops every state dated at the late
// write's time): two late files written into one folder at one source
// time (two Clio documents sharing a timestamp to the second). In one
// settle, and settled one at a time (the second settle finds the first's
// state at that time, and the second late write's own root version dated
// alike): both files in the past from that time, in a view of the past
// just after it and later, and in the present.
func TestTwoLateFilesAtOneTime(t *testing.T) {
	when := year(2021).Add(37 * time.Second)
	lateAt := func(d *Drive, late *drive.LateChanges, p string) {
		t.Helper()
		ctx := drive.WithSourceTime(drive.WithLateChanges(system(), late), when)
		if _, err := d.Write(ctx, p, strings.NewReader(p), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, d *Drive) {
		t.Helper()
		folder := folderID(t, d, "/dir")
		for _, c := range []struct {
			when time.Time
			want string
		}{
			{when.Add(-time.Second), "a.txt"},
			{when, "a.txt,late-a.txt,late-b.txt"},
			{when.Add(time.Second), "a.txt,late-a.txt,late-b.txt"},
			{midYear(2021), "a.txt,late-a.txt,late-b.txt"},
			{midYear(2022), "a.txt,b.txt,late-a.txt,late-b.txt"},
		} {
			if got := sortedAt(t, d, "/dir", c.when); got != c.want {
				t.Errorf("/dir as of %s: %s, want %s", c.when.Format(time.RFC3339), got, c.want)
			}
		}
		if got := liveNames(t, d, folder); got != "a.txt,b.txt,late-a.txt,late-b.txt" {
			t.Errorf("present: %s", got)
		}
		settleLeftovers(t, d, folder, 0)
	}
	t.Run("one settle", func(t *testing.T) {
		d := sameAtCase(t, "sameatone")
		late := &drive.LateChanges{}
		lateAt(d, late, "/dir/late-a.txt")
		lateAt(d, late, "/dir/late-b.txt")
		if _, err := d.SettleFolder(system(), late.Take(), drive.SettleOptions{}); err != nil {
			t.Fatal(err)
		}
		check(t, d)
	})
	t.Run("two settles", func(t *testing.T) {
		d := sameAtCase(t, "sameattwo")
		for _, p := range []string{"/dir/late-a.txt", "/dir/late-b.txt"} {
			late := &drive.LateChanges{}
			lateAt(d, late, p)
			if _, err := d.SettleFolder(system(), late.Take(), drive.SettleOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		check(t, d)
	})
}

// lostStore: the armed Put of key is made, and its answer lost (an
// error): after skip such Puts of key go through.
type lostStore struct {
	objstore.Store
	mu   sync.Mutex
	key  string
	skip int
}

func (s *lostStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	o, err := s.Store.Put(ctx, key, body, md, ifMatch)
	s.mu.Lock()
	hit := s.key != "" && key == s.key
	if hit {
		if s.skip > 0 {
			s.skip--
			hit = false
		} else {
			s.key = ""
		}
	}
	s.mu.Unlock()
	if hit && err == nil {
		return nil, errors.New("timeout: the answer was lost")
	}
	return o, err
}

// TestSettleLostAnswerInWindow (N2): the window's first, second or third
// root put is made and its answer lost; the settle tries it again, which
// works. The version the lost answer left (dated nowhere: no view of a
// date can tell it from the present) goes: the present and the past are
// right, nothing newer than the present before is left but the final
// states, and a later settle of the folder goes right too.
func TestSettleLostAnswerInWindow(t *testing.T) {
	for skip := 0; skip < 3; skip++ {
		t.Run(fmt.Sprint(skip), func(t *testing.T) {
			store := &lostStore{Store: miniotest.Store(t)}
			d, changes := cutCase(t, store, fmt.Sprintf("lost%d", skip), Config{DeferLateFolders: true})
			folder := changes[0].Folder
			store.mu.Lock()
			store.key, store.skip = folder, skip
			store.mu.Unlock()
			k := &keeper{}
			if _, err := d.SettleFolder(system(), changes, k.opts()); err != nil {
				t.Fatal(err)
			}
			if k.done != 1 || k.held() != nil {
				t.Errorf("done %d, record held %+v", k.done, k.held())
			}
			if got := liveNames(t, d, folder); got != "a.txt,b.txt,c.txt,x.txt" {
				t.Errorf("present: %s", got)
			}
			pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
			if got := sortedAt(t, d, "/dir", time.Now()); got != "a.txt,b.txt,c.txt,x.txt" {
				t.Errorf("as of now: %s", got)
			}
			settleLeftovers(t, d, folder, 0)
			writeSettled(t, d, system(), 2023, "/dir/y.txt")
			if got := liveNames(t, d, folder); got != "a.txt,b.txt,c.txt,x.txt,y.txt" {
				t.Errorf("present after a second settle: %s", got)
			}
			pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2023: "a.txt,b.txt,x.txt,y.txt", 2024: "a.txt,b.txt,c.txt,x.txt,y.txt"})
		})
	}
}

// cutInWindow settles changes on d over store, with the store away from
// the window's second root put on: a settle cut in its window. It
// returns the record the caller holds.
func cutInWindow(t *testing.T, d *Drive, store *failStore, changes []drive.LateChange) *keeper {
	t.Helper()
	folder := changes[0].Folder
	listed, puts := false, 0
	store.mu.Lock()
	store.watch = func(op, key string) error {
		switch {
		case op == "HISTORY" && key == folder:
			listed = true
		case listed && op == "PUT" && key == folder:
			if puts++; puts >= 2 {
				return errFail
			}
		}
		return nil
	}
	store.mu.Unlock()
	k := &keeper{}
	_, err := d.SettleFolder(system(), changes, k.opts())
	store.mu.Lock()
	store.watch = nil
	store.mu.Unlock()
	if !errors.Is(err, errFail) || k.held() == nil {
		t.Fatalf("a settle cut in its window: %v, record %+v", err, k.held())
	}
	return k
}

// TestSettleCutThenAnotherDrive (N3): a settle cut in its window leaves
// the folder's present a past state; the process "restarts": a new Drive
// over the same store, with an empty history cache, knows nothing of it
// but what the caller kept, the record. Settled again there with the
// record, the folder is right; the first drive holds nothing, and its
// next write anywhere goes through (round 2 kept the cut settle in the
// first drive's memory, and another drive built on the cut present).
func TestSettleCutThenAnotherDrive(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	cfg := Config{DeferLateFolders: true}
	a, changes := cutCase(t, store, "anotherdrive", cfg)
	folder := changes[0].Folder
	k := cutInWindow(t, a, store, changes)
	if got := liveNames(t, a, folder); got == "a.txt,b.txt,c.txt,x.txt" {
		t.Fatalf("the cut left the present as it was: no cut to finish")
	}
	b := newDriveWith(t, store, cutCaseSettings("anotherdrive"), Config{DeferLateFolders: true, ID: "another"})
	if _, err := b.SettleFolder(system(), changes, k.opts()); err != nil {
		t.Fatal(err)
	}
	if k.held() != nil {
		t.Errorf("record held after the settle: %+v", k.held())
	}
	if got := liveNames(t, b, folder); got != "a.txt,b.txt,c.txt,x.txt" {
		t.Errorf("present: %s", got)
	}
	pastIs(t, b, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
	settleLeftovers(t, b, folder, 0)
	if _, err := a.Write(at(system(), 2025), "/other/e.txt", strings.NewReader("e"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
		t.Errorf("the first drive's next write: %v", err)
	}
}

// TestCutSettleBlocksNothing (N4): after a settle cut in its window, the
// folder is deleted by another drive; the first drive's writes anywhere
// go through (round 2 refused every change on the drive), and the
// caller's settle again with its record finishes the settle by the
// folder's key (the owner's rule 7: never ErrFolderGone while a record is
// open) and releases the record.
func TestCutSettleBlocksNothing(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	a, changes := cutCase(t, store, "blocksnothing", Config{DeferLateFolders: true})
	k := cutInWindow(t, a, store, changes)
	b := newDriveWith(t, store, cutCaseSettings("blocksnothing"), Config{ID: "other"})
	if err := b.Delete(system(), drive.ByPath("/dir"), true, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		// With no source time: b's delete just made the root folder's
		// newest state now, so a dated write into it would be late.
		if _, err := a.Write(system(), fmt.Sprintf("/other/e%d.txt", i), strings.NewReader("e"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Errorf("the first drive's write %d anywhere: %v", i, err)
		}
	}
	if _, err := a.SettleFolder(system(), changes, k.opts()); err != nil {
		t.Errorf("the cut settle of a folder deleted since: %v, want it finished", err)
	}
	if k.held() != nil {
		t.Errorf("the record is held: %+v", k.held())
	}
}

// TestSettleFolderGone (N5): a deferred late change (x at 2021) whose
// folder is trashed, or purged, before its settle: SettleFolder says
// ErrFolderGone (an ErrNotFound, told apart from a failure), writes
// nothing, hands out no record and holds nothing. What a view of the
// trashed folder's past shows, the late change never settled, is pinned:
// the cost of breaking the caller's rule (a folder's queue settled before
// the folder is removed).
func TestSettleFolderGone(t *testing.T) {
	for _, how := range []string{"trashed", "purged"} {
		t.Run(how, func(t *testing.T) {
			store := miniotest.Store(t)
			d := newDriveWith(t, store, cutCaseSettings("gone"+how), Config{DeferLateFolders: true})
			ctx := system()
			writeAt(t, d, ctx, 2020, "/dir/a.txt")
			writeAt(t, d, ctx, 2022, "/dir/b.txt")
			late := &drive.LateChanges{}
			if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			changes := late.Take()
			if err := d.Delete(at(ctx, 2024), drive.ByPath("/dir"), true, drive.Condition{}); err != nil {
				t.Fatal(err)
			}
			if how == "purged" {
				if err := d.Purge(ctx, drive.ItemRef{ID: changes[0].Folder}, true); err != nil {
					t.Fatal(err)
				}
			}
			before := storeSnapshot(t, store)
			k := &keeper{}
			_, err := d.SettleFolder(ctx, changes, k.opts())
			if !errors.Is(err, drive.ErrFolderGone) || !errors.Is(err, drive.ErrNotFound) {
				t.Fatalf("the settle of a %s folder: %v, want ErrFolderGone", how, err)
			}
			if k.held() != nil || k.done != 0 {
				t.Errorf("a record handed out: %+v", k.held())
			}
			if after := storeSnapshot(t, store); after != before {
				t.Errorf("the settle of a %s folder wrote:\nbefore:\n%s\nafter:\n%s", how, before, after)
			}
			if how == "trashed" {
				// The cost of a queue left unsettled: x is in the folder's past
				// from the folder's newest state when it was written (2022) on,
				// not from 2021; every view a state the folder had.
				folder := changes[0].Folder
				for y, want := range map[int]string{2020: "a.txt", 2021: "a.txt", 2022: "a.txt,b.txt,x.txt", 2023: "a.txt,b.txt,x.txt"} {
					if got := folderAt(t, d, folder, midYear(y)); got != want {
						t.Errorf("the trashed folder as of mid-%d: %s, want %s", y, got, want)
					}
				}
			}
			// The drive holds nothing: its next write goes through.
			writeAt(t, d, ctx, 2025, "/other/e.txt")
		})
	}
}

// TestTwoWritersOneFolder (N6, the caller's rule): the rule is the
// caller's, not enforced, and this pins its one known exception: while a
// settle of a folder is cut, a person writes into the folder with no
// source time (as icbfs-cli does); the caller's settle again with its
// record takes that write for the cut settle's, and it is undone: the
// present is the one the record names, with the late change in place.
func TestTwoWritersOneFolder(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	d, changes := cutCase(t, store, "twowriters", Config{DeferLateFolders: true})
	folder := changes[0].Folder
	k := cutInWindow(t, d, store, changes)
	other := newDriveWith(t, store, cutCaseSettings("twowriters"), Config{ID: "by-hand"})
	if _, err := other.Write(system(), "/dir/byhand.txt", strings.NewReader("h"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SettleFolder(system(), changes, k.opts()); err != nil {
		t.Fatal(err)
	}
	if got := liveNames(t, d, folder); got != "a.txt,b.txt,c.txt,x.txt" {
		t.Errorf("present: %s, want the recorded present (the write by hand undone)", got)
	}
}
