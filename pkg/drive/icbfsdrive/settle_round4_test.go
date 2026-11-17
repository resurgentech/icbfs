package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// storeSnapshot is every version and deletion in store, the drive's own
// past-changed mark aside.
func versionSnapshot(t *testing.T, store objstore.Store) string {
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

// TestLateWriteIntoThePresent: on a deferring drive, a late write is
// written into its folder's present, dated at the folder's newest state:
// the folder's versions stay in date order (no root block or .metadata
// version stored after a later-dated one), the change is reported with
// its own date and Until the folder's newest state, the new file's own
// versions keep the change's date, and a view before the newest state
// shows the folder as it was, one from it on the present.
func TestLateWriteIntoThePresent(t *testing.T) {
	d, changes := cutCase(t, miniotest.Store(t), "intopresent", Config{DeferLateFolders: true})
	if len(changes) != 1 || !changes[0].At.Equal(year(2021)) || !changes[0].Until.Equal(year(2024)) || changes[0].Op != drive.LateAdd || changes[0].Name != "x.txt" {
		t.Fatalf("late changes %+v, want the add of x.txt at 2021 until 2024", changes)
	}
	folder := changes[0].Folder
	ctx := context.Background()
	dates, err := objstore.ReadDates(ctx, d.cfg.Store, folder)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{folder, folder + ".metadata"} {
		h, err := d.cfg.Store.History(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, late := objstore.NotStates(h, dates); len(late) > 0 {
			t.Errorf("%s: versions stored behind later-dated ones: %v", key, late)
		}
		if got := dates.Time(&h[0]); !got.Equal(year(2024)) {
			t.Errorf("%s: the late write's version is dated %s, want 2024", key, got.Format(time.RFC3339))
		}
	}
	x, err := d.Stat(system(), drive.ByPath("/dir/x.txt"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	xd, err := objstore.ReadDates(ctx, d.cfg.Store, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	for id, at := range xd {
		if !at.Equal(year(2021)) {
			t.Errorf("x's version %s is dated %s, want 2021", id, at.Format(time.RFC3339))
		}
	}
	pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt", 2022: "a.txt,b.txt", 2023: "a.txt,b.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
	if got := sortedAt(t, d, "/dir", year(2024)); got != "a.txt,b.txt,c.txt,x.txt" {
		t.Errorf("/dir at exactly 2024: %s", got)
	}
	if got := sortedAt(t, d, "/dir", year(2024).Add(-time.Second)); got != "a.txt,b.txt" {
		t.Errorf("/dir a second before 2024: %s", got)
	}
}

// TestSettleRefusedWritesNothing: a folder the settle must refuse (a
// root block version stored behind a later-dated one, as a writer not
// following the rule leaves: here an older build's late write, made by
// hand) gets ErrSettleRefused, with no record handed out and nothing
// written; asked again, the same. Changes whose Until is missing or not
// after their time are refused as invalid before anything is read.
func TestSettleRefusedWritesNothing(t *testing.T) {
	store := miniotest.Store(t)
	d, changes := cutCase(t, store, "refused", Config{DeferLateFolders: true})
	folder := changes[0].Folder
	ctx := context.Background()
	body, _, err := store.Get(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	behind, err := store.Put(ctx, folder, bytes.NewReader(data), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objstore.UpdateDates(ctx, store, folder, objstore.Dates{behind.VersionID: midYear(2021)}); err != nil {
		t.Fatal(err)
	}
	before := versionSnapshot(t, store)
	for try := 1; try <= 2; try++ {
		recorded := false
		_, err := d.SettleFolder(system(), changes, drive.SettleOptions{Record: func(drive.SettleRecord) error { recorded = true; return nil }})
		if !errors.Is(err, drive.ErrSettleRefused) {
			t.Fatalf("try %d: %v, want ErrSettleRefused", try, err)
		}
		t.Logf("try %d: %v", try, err)
		if recorded {
			t.Errorf("try %d: a record handed out", try)
		}
		if after := versionSnapshot(t, store); after != before {
			t.Errorf("try %d: the refused settle wrote to the store:\nbefore:\n%s\nafter:\n%s", try, before, after)
		}
	}
	bad := append([]drive.LateChange(nil), changes...)
	bad[0].Until = time.Time{}
	if _, err := d.SettleFolder(system(), bad, drive.SettleOptions{}); !errors.Is(err, drive.ErrInvalid) {
		t.Errorf("an add with no until: %v, want ErrInvalid", err)
	}
	bad[0].Until = bad[0].At
	if _, err := d.SettleFolder(system(), bad, drive.SettleOptions{}); !errors.Is(err, drive.ErrInvalid) {
		t.Errorf("an add with until at its time: %v, want ErrInvalid", err)
	}
}

// outsideMove moves /dir/a.txt to /other/a.txt through another drive on
// the store under fs (no source time), as a person with a command-line
// client would.
func outsideMove(t *testing.T, store objstore.Store, name string, when time.Time) {
	t.Helper()
	other := newDriveWith(t, store, cutCaseSettings(name), Config{})
	ctx := system()
	if !when.IsZero() {
		ctx = drive.WithSourceTime(ctx, when)
	}
	if _, err := other.Move(ctx, drive.ByPath("/dir/a.txt"), "/other/a.txt", false); err != nil {
		t.Fatalf("the outside move: %v", err)
	}
}

// TestSettleStoppedByAnOutsideWrite (the round-3 review's B2, the case
// that reaches icbfs: a writer outside the caller's exclusion): another
// drive moves /dir/a.txt out between the settle's reads and its window.
// The window's first put, conditional on the version the settle read,
// finds the move's version: the settle stops (ErrSettleStopped) before
// it has written any state, erases nothing of the folder, takes away its
// own new blocks and releases its record; the move stands (one name, fs
// check clean). Settled again, the late file goes into the past and the
// move stays. A write that lands later in the window stops the settle
// there with its record held, and nothing erased.
func TestSettleStoppedByAnOutsideWrite(t *testing.T) {
	for _, c := range []struct {
		name string
		when time.Time
		put  int // the window's put before which the outside write lands
	}{{"undated, before the window", time.Time{}, 1}, {"dated 2025, before the window", year(2025), 1}, {"undated, inside the window", time.Time{}, 3}} {
		t.Run(c.name, func(t *testing.T) {
			raw := miniotest.Store(t)
			store := &failStore{Store: raw}
			name := "outside" + fmt.Sprint(c.put) + fmt.Sprint(c.when.Year())
			d, changes := cutCase(t, store, name, Config{DeferLateFolders: true})
			if _, err := d.Write(at(system(), 2024), "/other/o.txt", strings.NewReader("o"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
				t.Fatal(err)
			}
			folder := changes[0].Folder
			listed, puts, moved := false, 0, false
			store.mu.Lock()
			store.watch = func(op, key string) error {
				switch {
				case op == "HISTORY" && key == folder:
					listed = true
				case listed && !moved && op == "PUT" && (key == folder || key == folder+".metadata"):
					puts++
					if puts == c.put {
						moved = true
						outsideMove(t, raw, name, c.when)
					}
				}
				return nil
			}
			store.mu.Unlock()
			k := &keeper{}
			before := ""
			_, err := d.SettleFolder(system(), changes, k.opts())
			store.mu.Lock()
			store.watch = nil
			store.mu.Unlock()
			if !moved {
				t.Fatalf("the window never came")
			}
			if !errors.Is(err, drive.ErrSettleStopped) {
				t.Fatalf("the settle: %v, want ErrSettleStopped", err)
			}
			t.Logf("the settle: %v", err)
			h, err := raw.History(context.Background(), folder)
			if err != nil {
				t.Fatal(err)
			}
			if got := liveNames(t, d, folder); c.put == 1 && got != "b.txt,c.txt,x.txt" {
				t.Errorf("/dir now: %s, want the move's b.txt,c.txt,x.txt", got)
			}
			if got := sortedAt(t, d, "/other", time.Time{}); got != "a.txt,o.txt" {
				t.Errorf("/other now: %s", got)
			}
			rep, err := d.fs.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range rep.Problems {
				t.Errorf("fs check: %+v", p)
			}
			if c.put > 1 {
				// Inside the window: the record is held, and the outside
				// write's version is the folder's newest, nothing erased.
				if k.held() == nil {
					t.Errorf("stopped inside its window, and no record held")
				}
				_ = before
				t.Logf("stopped inside the window: %d root versions, record held %v", len(h), k.held() != nil)
				return
			}
			if k.held() != nil || k.done != 1 {
				t.Errorf("stopped before its window: record held %+v, done %d; want released", k.held(), k.done)
			}
			if _, err := d.SettleFolder(system(), changes, k.opts()); err != nil {
				t.Fatalf("settled again: %v", err)
			}
			if got := liveNames(t, d, folder); got != "b.txt,c.txt,x.txt" {
				t.Errorf("/dir now after settling again: %s", got)
			}
			pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
			if got := sortedAt(t, d, "/other", time.Time{}); got != "a.txt,o.txt" {
				t.Errorf("/other now after settling again: %s", got)
			}
			settleLeftovers(t, d, folder, 0)
		})
	}
}

// TestSettleFinishedByKeyWhenGone (the owner's rule 7): a settle cut in
// its window, then its folder trashed (by an outside writer: the daemon
// settles a folder before it removes it): the settle run again with the
// record is finished by the folder's key, never answers ErrFolderGone,
// and releases the record; restored, the folder is as it was, the late
// file in its past (read by its key). A folder purged with a settle cut: the record's new
// blocks go, and the record is released. With no record, a folder that
// is gone answers ErrFolderGone and nothing is written.
func TestSettleFinishedByKeyWhenGone(t *testing.T) {
	store := &cutStore{Store: miniotest.Store(t)}
	d, changes := cutCase(t, store, "gonewithrecord", Config{DeferLateFolders: true})
	folder := changes[0].Folder
	ctx := system()
	store.mu.Lock()
	store.folder, store.cut = folder, 3
	store.mu.Unlock()
	k := &keeper{}
	if _, err := d.SettleFolder(ctx, changes, k.opts()); err == nil {
		t.Fatalf("a cut settle: no error")
	}
	store.mu.Lock()
	store.folder = ""
	store.mu.Unlock()
	if k.held() == nil {
		t.Fatalf("a settle cut in its window holds no record")
	}
	if err := d.Delete(ctx, drive.ByPath("/dir"), true, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SettleFolder(ctx, changes, drive.SettleOptions{}); !errors.Is(err, drive.ErrFolderGone) {
		t.Errorf("gone, with no record: %v, want ErrFolderGone", err)
	}
	if _, err := d.SettleFolder(ctx, changes, k.opts()); err != nil {
		t.Fatalf("gone, with the record: %v", err)
	}
	if k.held() != nil {
		t.Errorf("the record is held after the settle with it")
	}
	// The trashed folder, by its key: as it was, the late file in its
	// past from 2021.
	if got := liveNames(t, d, folder); got != "a.txt,b.txt,c.txt,x.txt" {
		t.Errorf("/dir in the trash, now: %s", got)
	}
	for y, want := range map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"} {
		if got := folderAt(t, d, folder, midYear(y)); got != want {
			t.Errorf("/dir in the trash as of mid-%d: %s, want %s", y, got, want)
		}
	}

	// Purged with a settle cut.
	d2, changes2 := cutCase(t, store, "purgedwithrecord", Config{DeferLateFolders: true})
	folder2 := changes2[0].Folder
	store.mu.Lock()
	store.folder, store.cut, store.n = folder2, 3, 0
	store.mu.Unlock()
	k2 := &keeper{}
	if _, err := d2.SettleFolder(ctx, changes2, k2.opts()); err == nil {
		t.Fatalf("a cut settle: no error")
	}
	store.mu.Lock()
	store.folder = ""
	store.mu.Unlock()
	rec := k2.held()
	if rec == nil {
		t.Fatalf("no record held")
	}
	if err := d2.Purge(ctx, drive.ByPath("/dir"), false); err != nil {
		t.Fatalf("purge /dir: %v", err)
	}
	if h, err := store.Store.History(context.Background(), folder2); err != nil || len(liveOfObjects(h)) != 0 {
		t.Fatalf("purged /dir: its root block still has %d versions (%v)", len(liveOfObjects(h)), err)
	}
	if _, err := d2.SettleFolder(ctx, changes2, k2.opts()); err != nil {
		t.Fatalf("purged, with the record: %v", err)
	}
	if k2.held() != nil {
		t.Errorf("the record is held")
	}
	for _, key := range rec.Create {
		if h, err := store.Store.History(context.Background(), key); err != nil || len(h) != 0 {
			t.Errorf("the cut settle's new block %s: %d versions left (%v)", key, len(h), err)
		}
	}
}

func liveOfObjects(h []objstore.Object) []objstore.Object {
	var out []objstore.Object
	for _, o := range h {
		if !o.Deleted {
			out = append(out, o)
		}
	}
	return out
}

// TestLateWriteCost: what a write in a source's time costs on a drive
// that defers late folders: one read of the folder's .version record for
// each folder an entry change goes into, paid on time and late alike (it
// is how the drive knows which); a non-deferring drive pays the same
// read (it refuses a late change before writing). A late write costs one
// request more than an on-time one: the listing of the folder's root
// block (newestState: a root block version an undated write left is
// dated by the listing); it is otherwise the same write, dated at the
// folder's newest state, and puts nothing back on top.
func TestLateWriteCost(t *testing.T) {
	counter := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	deferring := newDriveWith(t, counter, icbfs.Settings{Name: "cost", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	plain := newDriveWith(t, counter, icbfs.Settings{Name: "cost", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{})
	ctx := system()
	for _, y := range []int{2020, 2022, 2024} {
		if _, err := deferring.Write(at(ctx, y), fmt.Sprintf("/dir/f%d.txt", y), strings.NewReader("f"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(pastMarkEvery + 100*time.Millisecond)
	deferring.past.flush()
	plain.past.flush()
	measure := func(d *Drive, lctx context.Context, y int, name string) (int, string) {
		t.Helper()
		counter.take()
		if _, err := d.Write(at(lctx, y), "/dir/"+name, strings.NewReader("n"), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		return counter.take()
	}
	late := &drive.LateChanges{}
	lctx := drive.WithLateChanges(ctx, late)
	onTime, onTimeAll := measure(deferring, lctx, 2025, "ontime.txt")
	lateN, lateAll := measure(deferring, lctx, 2021, "late.txt")
	plainN, plainAll := measure(plain, ctx, 2026, "plain.txt")
	t.Logf("a new file on time on a deferring drive: %d requests:%s", onTime, onTimeAll)
	t.Logf("a new file late on a deferring drive: %d requests:%s", lateN, lateAll)
	t.Logf("a new file on time on a non-deferring drive: %d requests:%s", plainN, plainAll)
	if lateN > onTime+1 {
		t.Errorf("a late write costs %d requests, an on-time one %d: want one more at most", lateN, onTime)
	}
	if onTime != plainN {
		t.Errorf("an on-time write costs %d on a deferring drive, %d on a plain one: want the same", onTime, plainN)
	}
	if !strings.Contains(onTimeAll, ".version") {
		t.Errorf("no read of a .version record on time:%s", onTimeAll)
	}
}
