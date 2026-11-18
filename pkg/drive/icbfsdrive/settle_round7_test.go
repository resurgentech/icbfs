package icbfsdrive

// T83-1 round 7: rows of interloc's docs/STATES.md section 1 that the
// final review found unmet or unproved (CH-13, CH-4's prove-only settle,
// LF-28, ST-5 in a split folder, ST-16, ST-19, ST-21, ST-24, ST-26).

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestCH13_LateChangeToTheFolderRecord (CH-13, CH-5, 1.6-1): /dir a
// (2011-01), b (2011-03), c (2011-09: its newest state T); then, late, its
// mode set to 0700 dated 2011-05 and its origin set dated 2011-06. On a
// drive that defers late folders both are written into the present,
// dated T: the present and the states from T have them, the states
// before T (those in [D, T) included) keep the record they had; no row
// is reported; nothing is stored behind a later-dated version. A settle
// of a late file afterwards keeps each state's own record (CH-5). Both
// block sizes.
func TestCH13_LateChangeToTheFolderRecord(t *testing.T) {
	for _, entries := range []uint32{0, 4} {
		t.Run(fmt.Sprintf("blocks of %d", entries), func(t *testing.T) {
			d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: fmt.Sprintf("ch13%d", entries), Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries},
				Config{DeferLateFolders: true})
			r5Write(t, d, "/dir/a.txt", rvMonth(2011, 1, 1))
			r5Write(t, d, "/dir/b.txt", rvMonth(2011, 3, 1))
			r5Write(t, d, "/dir/c.txt", rvMonth(2011, 9, 1))
			mode := uint32(0o700)
			late := &drive.LateChanges{}
			if _, err := d.SetInfo(drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 5, 1)), drive.ByPath("/dir"), drive.ItemInfo{Mode: &mode}); err != nil {
				t.Fatal(err)
			}
			origin := drive.Origin{Drive: "clio", ID: "folder-1"}
			if err := d.SetOrigin(drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 6, 1)), drive.ByPath("/dir"), origin); err != nil {
				t.Fatal(err)
			}
			if rows := late.Take(); len(rows) != 0 {
				t.Errorf("the late record changes reported rows: %s", rowsUntil(rows))
			}
			stat := func(at time.Time) string {
				it, err := d.Stat(system(), drive.ByPath("/dir"), drive.At{Time: at})
				if err != nil {
					return errShort(err)
				}
				o := "-"
				if it.Origin != nil {
					o = it.Origin.ID
				}
				return fmt.Sprintf("%o %s", it.Mode&0o7777, o)
			}
			check := func(when string) {
				t.Helper()
				for _, c := range []struct {
					at   time.Time
					want string
				}{
					{rvMonth(2011, 4, 1), "755 -"},
					{rvMonth(2011, 7, 1), "755 -"},        // in [D, T): the record it had
					{rvMonth(2011, 9, 1), "700 folder-1"}, // T
					{rvMonth(2011, 10, 1), "700 folder-1"},
					{time.Time{}, "700 folder-1"}, // the present
				} {
					if got := stat(c.at); got != c.want {
						t.Errorf("%s: /dir as of %v: %s, want %s", when, c.at.Format("2006-01-02"), got, c.want)
					}
				}
				if behind, err := d.FolderWrittenBehind(system(), drive.ByPath("/dir")); err != nil || behind {
					t.Errorf("%s: written behind %v, %v", when, behind, err)
				}
			}
			check("after the late record changes")
			xs := r5Write(t, d, "/dir/x.txt", rvMonth(2011, 2, 15))
			if _, err := d.SettleFolder(system(), xs, drive.SettleOptions{Path: "/dir"}); err != nil {
				t.Errorf("a late x's settle: %v", err)
			}
			check("after a late x settled")
			if got := sortedAt(t, d, "/dir", rvMonth(2011, 4, 1)); got != "a.txt,b.txt,x.txt" {
				t.Errorf("as of 2011-04 after the settle: %s", got)
			}
		})
	}
}

// TestCH13_NotDeferringNothingChanges (CH-13, its last clause): on a
// drive that does not defer late folders a late change to a folder's own
// record goes as it went before round 7: written at its own date, the
// newer record put back on top, so the present keeps the record it had.
func TestCH13_NotDeferringNothingChanges(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "ch13nd", Size: 1 << 30, RootMode: 0o777, Created: year(2010)}, Config{})
	for i, m := range []int{1, 3, 9} {
		if _, err := d.Write(drive.WithSourceTime(system(), rvMonth(2011, m, 1)), fmt.Sprintf("/dir/f%d.txt", i), strings.NewReader("f"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	mode := uint32(0o700)
	if _, err := d.SetInfo(drive.WithSourceTime(system(), rvMonth(2011, 5, 1)), drive.ByPath("/dir"), drive.ItemInfo{Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at   time.Time
		want uint32
	}{{rvMonth(2011, 4, 1), 0o755}, {rvMonth(2011, 6, 1), 0o700}, {time.Time{}, 0o755}} {
		it, err := d.Stat(system(), drive.ByPath("/dir"), drive.At{Time: c.at})
		if err != nil {
			t.Fatal(err)
		}
		if it.Mode&0o7777 != c.want {
			t.Errorf("as of %v: %o, want %o", c.at.Format("2006-01-02"), it.Mode&0o7777, c.want)
		}
	}
}

// TestCH4_ProveOnlyWritesNothing (CH-4): a settle run with ProveOnly
// reads, plays and proves, and writes nothing: no write request, no
// record, no Done, every view as before; nil for rows that would settle,
// ErrSettleRefused for rows that would be refused. With Cut: ErrInvalid.
// Then the settle itself, as proved.
func TestCH4_ProveOnlyWritesNothing(t *testing.T) {
	raw := miniotest.Store(t)
	store := &failStore{Store: raw}
	d, changes := cutCase(t, store, "ch4", Config{DeferLateFolders: true})
	before := versionSnapshot(t, raw)
	kp := &keeper{}
	opts := kp.opts()
	opts.Path, opts.ProveOnly = "/dir", true
	recorded := false
	rec := opts.Record
	opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
	var writes []string
	store.setWatch(func(op, key string) error {
		if isWrite(op) {
			writes = append(writes, op+" "+key)
		}
		return nil
	})
	s, err := d.SettleFolder(system(), changes, opts)
	store.setWatch(nil)
	if err != nil {
		t.Fatalf("prove only: %v", err)
	}
	if len(writes) > 0 || recorded || kp.done != 0 {
		t.Errorf("prove only: writes %v, record %v, done %d", writes, recorded, kp.done)
	}
	if got := versionSnapshot(t, raw); got != before {
		t.Errorf("prove only: the store changed")
	}
	if s.Changes != len(changes) || s.Written != 0 {
		t.Errorf("prove only: %+v, want Changes %d and nothing written", s, len(changes))
	}
	if got := sortedAt(t, d, "/dir", midYear(2021)); got != "a.txt" {
		t.Errorf("prove only: mid-2021 is %s; the settle wrote", got)
	}
	// A folder whose settle would be refused: its root block written
	// behind a later-dated version (LF-27), as TestSettleRefusedWritesNothing.
	rd, rchanges := cutCase(t, store, "ch4refused", Config{DeferLateFolders: true})
	rfolder := rchanges[0].Folder
	body, _, err := raw.Get(context.Background(), rfolder)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	behind, err := raw.Put(context.Background(), rfolder, bytes.NewReader(data), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objstore.UpdateDates(context.Background(), raw, rfolder, objstore.Dates{behind.VersionID: midYear(2021)}); err != nil {
		t.Fatal(err)
	}
	rbefore := versionSnapshot(t, raw)
	writes = nil
	store.setWatch(func(op, key string) error {
		if isWrite(op) {
			writes = append(writes, op+" "+key)
		}
		return nil
	})
	_, err = rd.SettleFolder(system(), rchanges, drive.SettleOptions{Path: "/dir", ProveOnly: true})
	store.setWatch(nil)
	if !errors.Is(err, drive.ErrSettleRefused) || len(writes) > 0 || versionSnapshot(t, raw) != rbefore {
		t.Errorf("prove only, a settle that would be refused: %v, writes %v; want ErrSettleRefused and none", err, writes)
	}
	if _, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir", ProveOnly: true, Cut: &drive.SettleRecord{Folder: changes[0].Folder}}); !errors.Is(err, drive.ErrInvalid) {
		t.Errorf("prove only with Cut: %v, want ErrInvalid", err)
	}
	if _, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
}

// TestLF28_DatingAndUndoFailTheRowsAreReported (LF-28, LF-3): a late
// write whose dating fails (three tries) and whose undo fails too leaves
// its entry in the folder's present; the caller's LateChanges still gets
// its rows, so the daemon can store them. A late write whose undo works
// leaves nothing in the present, and reports none.
func TestLF28_DatingAndUndoFailTheRowsAreReported(t *testing.T) {
	for _, undoFails := range []bool{true, false} {
		t.Run(fmt.Sprintf("undo fails %v", undoFails), func(t *testing.T) {
			raw := miniotest.Store(t)
			store := &failStore{Store: raw}
			name := fmt.Sprintf("lf28%v", undoFails)
			d := newDriveWith(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010)}, Config{DeferLateFolders: true})
			r5Write(t, d, "/dir/a.txt", rvMonth(2011, 1, 1))
			r5Write(t, d, "/dir/c.txt", rvMonth(2011, 9, 1))
			n, err := d.resolve(system(), d.fs, drive.ByPath("/dir"))
			if err != nil {
				t.Fatal(err)
			}
			folder := n.key
			// The root block's put goes through; then the dating fails (every
			// request), and the undo's requests fail too, or go through.
			put, undoing := false, false
			store.setWatch(func(op, key string) error {
				switch {
				case !put && op == "PUT" && key == folder:
					put = true
				case put && !undoFails && (op == "HEAD" || op == "ERASE"):
					undoing = true
					return nil
				case put:
					return errFail
				}
				return nil
			})
			late := &drive.LateChanges{}
			_, err = d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 5, 1)), "/dir/b.txt", strings.NewReader("b"), drive.Condition{}, drive.WriteOptions{})
			store.setWatch(nil)
			rows := late.Take()
			t.Logf("the late write: %v; rows %s (undo ran %v)", err, rowsUntil(rows), undoing)
			if err == nil || !put {
				t.Fatalf("the write did not fail after its root put: %v", err)
			}
			d = newDriveWith(t, store, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
			present := liveNames(t, d, folder)
			inPresent := strings.Contains(present, "b.txt")
			if undoFails != inPresent {
				t.Fatalf("undo fails %v, but the present is %s", undoFails, present)
			}
			if !inPresent {
				if len(rows) != 0 {
					t.Errorf("undone, nothing in the present (%s), yet rows %s", present, rowsUntil(rows))
				}
				return
			}
			if len(rows) != 1 || rows[0].Op != drive.LateAdd || rows[0].Name != "b.txt" || rows[0].Folder != folder ||
				!rows[0].At.Equal(rvMonth(2011, 5, 1)) || !rows[0].Until.Equal(rvMonth(2011, 9, 1)) {
				t.Fatalf("b.txt is in the present (%s); rows %s, want its add from 2011-05 until 2011-09", present, rowsUntil(rows))
			}
		})
	}
}

// TestST5_FirstPutFindsAnotherWriterInASplitFolder (ST-5, LF-9): a split
// folder (blocks of four), so the settle has new blocks below its root;
// another writer's version lands before the window's first put. The
// settle stops (ErrSettleStopped), erases every new block it wrote,
// tells Done (the record goes), and the other writer's file stays.
func TestST5_FirstPutFindsAnotherWriterInASplitFolder(t *testing.T) {
	raw := miniotest.Store(t)
	store := &failStore{Store: raw}
	cfg := Config{DeferLateFolders: true}
	d, changes := splitCase(t, store, "st5split", cfg)
	folder := changes[0].Folder
	kp := &keeper{}
	opts := kp.opts()
	opts.Path = "/dir"
	var held drive.SettleRecord
	rec := opts.Record
	opts.Record = func(r drive.SettleRecord) error { held = r; return rec(r) }
	other := newDriveWith(t, raw, icbfs.Settings{Name: "st5split"}, Config{})
	wrote := false
	store.setWatch(func(op, key string) error {
		if held.Folder != "" && !wrote && op == "PUT" && (key == folder || key == folder+".metadata") {
			wrote = true
			if _, err := other.Write(drive.WithSourceTime(system(), year(2026)), "/dir/zz.txt", strings.NewReader("zz"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				return fmt.Errorf("the other writer: %v", err)
			}
		}
		return nil
	})
	_, err := d.SettleFolder(system(), changes, opts)
	store.setWatch(nil)
	if !wrote || !errors.Is(err, drive.ErrSettleStopped) {
		t.Fatalf("other writer %v; the settle: %v, want ErrSettleStopped", wrote, err)
	}
	if len(held.Create) == 0 {
		t.Fatalf("the settle made no new blocks below the root: the case does not reach ST-5's erase")
	}
	for _, k := range held.Create {
		h, err := raw.History(context.Background(), k)
		if err != nil {
			t.Fatal(err)
		}
		if len(h) > 0 {
			t.Errorf("the settle's new block %s still has %d version(s)", k, len(h))
		}
	}
	if kp.held() != nil || kp.done != 1 {
		t.Errorf("record held %v, done %d; want dropped", kp.held() != nil, kp.done)
	}
	if got := liveNames(t, d, folder); !strings.Contains(got, "zz.txt") {
		t.Errorf("the other writer's zz.txt is gone: %s", got)
	}
}

// TestST16_StopWhileErasingItsOwnBlocks (ST-16, ST-5, ST-11): as ST-5's
// test, and an immediate stop during the erase of the settle's own new
// blocks: the error says context.Canceled and ErrSettleStopped, the record
// stays (Done not told), and the rerun with it stops (LF-20) without
// erasing the other writer's version. The other writer dated (on time)
// and undated. (The final review's TestFR_ST16_StopWhileErasingItsOwnBlocks.)
func TestST16_StopWhileErasingItsOwnBlocks(t *testing.T) {
	for _, c := range []struct {
		name string
		when time.Time
	}{{"dated", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}, {"undated", time.Time{}}} {
		t.Run(c.name, func(t *testing.T) {
			raw := miniotest.Store(t)
			store := &failStore{Store: raw}
			cfg := Config{DeferLateFolders: true}
			name := "st16" + c.name
			d, changes := splitCase(t, store, name, cfg)
			folder := changes[0].Folder
			ctx, cancel := context.WithCancel(system())
			defer cancel()
			kp := &keeper{}
			opts := kp.opts()
			opts.Path = "/dir"
			recorded, wrote, stopped := false, false, ""
			rec := opts.Record
			opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
			other := newDriveWith(t, raw, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 4}, Config{})
			store.setWatch(func(op, key string) error {
				switch {
				case recorded && !wrote && op == "PUT" && (key == folder || key == folder+".metadata"):
					wrote = true
					wctx := system()
					if !c.when.IsZero() {
						wctx = drive.WithSourceTime(wctx, c.when)
					}
					if _, err := other.Write(wctx, "/dir/zz.txt", strings.NewReader("zz"), drive.Condition{}, drive.WriteOptions{}); err != nil {
						return fmt.Errorf("the other writer: %v", err)
					}
				case wrote && stopped == "" && op == "ERASE-all":
					stopped = op + " " + key
					cancel()
				}
				return nil
			})
			_, err := d.SettleFolder(ctx, changes, opts)
			store.setWatch(nil)
			if !wrote || stopped == "" {
				t.Fatalf("the other writer wrote %v; stopped at %q; err %v", wrote, stopped, err)
			}
			if !errors.Is(err, context.Canceled) || !errors.Is(err, drive.ErrSettleStopped) {
				t.Errorf("stopped while erasing its own blocks: %v; want context.Canceled and ErrSettleStopped", err)
			}
			if kp.held() == nil || kp.done != 0 {
				t.Fatalf("stopped while erasing its own blocks: record held %v, done %d; want held", kp.held() != nil, kp.done)
			}
			d = newDriveWith(t, store, icbfs.Settings{Name: name}, cfg)
			opts = kp.opts()
			opts.Path = "/dir"
			_, err = d.SettleFolder(system(), changes, opts)
			if got := liveNames(t, d, folder); !strings.Contains(got, "zz.txt") {
				t.Errorf("the other writer's zz.txt is gone from the present after the rerun: %s", got)
			}
			if !errors.Is(err, drive.ErrSettleStopped) {
				t.Errorf("the rerun: %v, want ErrSettleStopped (LF-20)", err)
			}
		})
	}
}

// stopCtx is a context its test ends with the error it chooses: a
// deadline that passes at a request of the test's choosing.
type stopCtx struct {
	context.Context
	mu   sync.Mutex
	done chan struct{}
	err  error
}

func newStopCtx(parent context.Context) *stopCtx {
	return &stopCtx{Context: parent, done: make(chan struct{})}
}

func (c *stopCtx) Done() <-chan struct{} { return c.done }

func (c *stopCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *stopCtx) stop(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

// TestST24_ADeadlineIsAnImmediateStop (ST-24, G2): the caller's deadline
// passing in the window (before its third put), in the commit, or in the
// erases acts as an immediate stop: no request after the one in flight,
// the error is context.DeadlineExceeded, the record stays, and a rerun
// with it from a new Drive finishes the settle.
func TestST24_ADeadlineIsAnImmediateStop(t *testing.T) {
	for _, c := range []struct {
		name string
		hit  func(op, key, folder string, puts int) bool
	}{
		{"window", func(op, key, folder string, puts int) bool {
			return op == "PUT" && (key == folder || key == folder+".metadata") && puts == 3
		}},
		{"commit", func(op, key, folder string, _ int) bool { return op == "PUT" && key == folder+objstore.VersionSuffix }},
		{"erases", func(op, key, folder string, _ int) bool { return strings.HasPrefix(op, "ERASE") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &failStore{Store: miniotest.Store(t)}
			cfg := Config{DeferLateFolders: true}
			name := "st24" + c.name
			d, changes := cutCase(t, store, name, cfg)
			folder := changes[0].Folder
			before := liveNames(t, d, folder)
			ctx := newStopCtx(system())
			kp := &keeper{}
			opts := kp.opts()
			opts.Path = "/dir"
			recorded := false
			rec := opts.Record
			opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
			puts, hit := 0, ""
			var after []string
			store.setWatch(func(op, key string) error {
				if hit != "" {
					after = append(after, op+" "+key)
					return nil
				}
				if !recorded {
					return nil
				}
				if op == "PUT" && (key == folder || key == folder+".metadata") {
					puts++
				}
				if c.hit(op, key, folder, puts) {
					hit = op + " " + key
					ctx.stop(context.DeadlineExceeded)
				}
				return nil
			})
			_, err := d.SettleFolder(ctx, changes, opts)
			store.setWatch(nil)
			if hit == "" {
				t.Fatalf("the deadline never came: %v", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("the deadline at %s: %v, want context.DeadlineExceeded", hit, err)
			}
			if len(after) > 0 {
				t.Errorf("the deadline at %s: %d requests after it: %v", hit, len(after), after)
			}
			if kp.held() == nil || kp.done != 0 {
				t.Fatalf("the deadline at %s: record held %v, done %d", hit, kp.held() != nil, kp.done)
			}
			d = newDriveWith(t, store, cutCaseSettings(name), cfg)
			opts = kp.opts()
			opts.Path = "/dir"
			if _, err := d.SettleFolder(system(), changes, opts); err != nil {
				t.Fatalf("the rerun: %v", err)
			}
			if kp.held() != nil || kp.done != 1 {
				t.Errorf("after the rerun: held %v, done %d", kp.held() != nil, kp.done)
			}
			if got := liveNames(t, d, folder); got != before {
				t.Errorf("the folder now %s, want %s", got, before)
			}
			pastIs(t, d, cutCaseSettled)
		})
	}
}

// cutCaseSettled is cutCase's /dir after its settle, mid-year.
var cutCaseSettled = map[int]string{
	2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2023: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt",
}

// TestST26_TableEmptiedUnderASettleFails (ST-26, G3): another process
// sharing the history cache's file empties the settle's working table
// (every run's rows: what its clearing of a run it took for dead does)
// while the settle reads the folder's states, at its first, a middle and
// its last read. The settle notices and fails with an error that is
// neither ErrSettleRefused nor ErrSettleStopped (not counted), hands out
// no record and writes nothing; the next settle settles.
func TestST26_TableEmptiedUnderASettleFails(t *testing.T) {
	ctx := context.Background()
	raw := miniotest.Store(t)
	store := &failStore{Store: raw}
	path := filepath.Join(t.TempDir(), "history.db")
	cache, err := objstore.OpenHistoryCache(ctx, store, path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	empty := func() error {
		for _, table := range []string{"settle_entries", "settle_read", "settle_records", "settle_states", "settle_blocks", "settle_runs"} {
			if _, err := db.Exec(`DELETE FROM ` + table); err != nil {
				return err
			}
		}
		return nil
	}
	cfg := Config{DeferLateFolders: true, History: cache}
	// counting is a watch counting the requests of a settle's reads: from
	// its first listing of the folder's root block (its working table made)
	// on, until recorded says the record is out.
	counting := func(folder string, recorded *bool, at func(i int)) func(op, key string) error {
		i := 0
		return func(op, key string) error {
			if *recorded {
				return nil
			}
			if i > 0 || op == "HISTORY" && key == folder {
				i++
				at(i)
			}
			return nil
		}
	}
	// The reads of a settle of a folder like it, before its record.
	pd, pchanges := cutCase(t, store, "st26p", cfg)
	reads, precorded := 0, false
	store.setWatch(counting(pchanges[0].Folder, &precorded, func(i int) { reads = i }))
	if _, err := pd.SettleFolder(system(), pchanges, drive.SettleOptions{Path: "/dir", Record: func(drive.SettleRecord) error { precorded = true; return nil }}); err != nil {
		t.Fatalf("the probe: %v", err)
	}
	store.setWatch(nil)
	d, changes := cutCase(t, store, "st26", cfg)
	for _, k := range []int{1, reads / 2, reads} {
		t.Run(fmt.Sprintf("emptied at read %d of %d", k, reads), func(t *testing.T) {
			snap := versionSnapshot(t, raw)
			kp := &keeper{}
			opts := kp.opts()
			opts.Path = "/dir"
			recorded := false
			rec := opts.Record
			opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
			var emptyErr error
			store.setWatch(counting(changes[0].Folder, &recorded, func(i int) {
				if i == k {
					emptyErr = empty()
				}
			}))
			_, err := d.SettleFolder(system(), changes, opts)
			store.setWatch(nil)
			if emptyErr != nil {
				t.Fatal(emptyErr)
			}
			t.Logf("emptied at read %d: %v", k, err)
			if err == nil || errors.Is(err, drive.ErrSettleRefused) || errors.Is(err, drive.ErrSettleStopped) {
				t.Errorf("the table emptied under the settle: %v; want an error that is not counted", err)
			}
			if recorded || versionSnapshot(t, raw) != snap {
				t.Errorf("the table emptied under the settle: record %v, store changed %v", recorded, versionSnapshot(t, raw) != snap)
			}
		})
	}
	if _, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatalf("the next settle: %v", err)
	}
	pastIs(t, d, cutCaseSettled)
}

// TestST21_RowsOutOfOrder (ST-21): rows given out of the order of their
// dates are played in the order of their dates; Changes counts the
// leading rows, as given, that are settled: too few, never too many; a
// row settled and given again plays as nothing. (The final review's
// TestFR_ST21_RowsOutOfOrder, its expectation the row as rewritten.)
func TestST21_RowsOutOfOrder(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	d := newDriveWith(t, store, cutCaseSettings("st21"), Config{DeferLateFolders: true})
	for i, y := range []int{2016, 2018, 2020} {
		r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", i), year(y))
	}
	l1 := r5Write(t, d, "/dir/l1.txt", rvMonth(2017, 3, 1))
	l0 := r5Write(t, d, "/dir/l0.txt", rvMonth(2016, 3, 1))
	rows := append(append([]drive.LateChange(nil), l1...), l0...)
	s, err := d.SettleFolder(system(), rows, drive.SettleOptions{Path: "/dir"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedAt(t, d, "/dir", midYear(2016)); got != "f0.txt,l0.txt" {
		t.Errorf("mid-2016: %s, want l0 played in by its date", got)
	}
	if got := sortedAt(t, d, "/dir", rvMonth(2017, 6, 1)); got != "f0.txt,l0.txt,l1.txt" {
		t.Errorf("mid-2017: %s", got)
	}
	if s.Changes != 2 {
		t.Errorf("both settled: Changes %d, want 2", s.Changes)
	}
	// In parts of one: the earlier-dated row (m0, given second) is the
	// first part and settles; the second part's record is refused. Changes
	// counts from the first row given (m1, not settled): 0, too few, never
	// more; the rest given again settles, m0 playing as nothing.
	saved := MaxSettleChanges
	MaxSettleChanges = 1
	t.Cleanup(func() { MaxSettleChanges = saved })
	m1 := r5Write(t, d, "/dir/m1.txt", rvMonth(2019, 3, 1))
	m0 := r5Write(t, d, "/dir/m0.txt", rvMonth(2018, 3, 1))
	rows = append(append([]drive.LateChange(nil), m1...), m0...)
	n := 0
	s, err = d.SettleFolder(system(), rows, drive.SettleOptions{Path: "/dir", Record: func(drive.SettleRecord) error {
		if n++; n == 2 {
			return errString("away")
		}
		return nil
	}})
	if err == nil || s.Changes != 0 {
		t.Errorf("in parts, the second refused: %v, Changes %d; want an error and 0", err, s.Changes)
	}
	if got := sortedAt(t, d, "/dir", rvMonth(2018, 6, 1)); !strings.Contains(got, "m0.txt") {
		t.Errorf("mid-2018 after the first part: %s, want m0", got)
	}
	if _, err := d.SettleFolder(system(), rows[s.Changes:], drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatalf("the rest again: %v", err)
	}
	if got := sortedAt(t, d, "/dir", midYear(2018)); got != "f0.txt,f1.txt,l0.txt,l1.txt,m0.txt" {
		t.Errorf("mid-2018 after the rest: %s", got)
	}
	if got := sortedAt(t, d, "/dir", rvMonth(2019, 6, 1)); got != "f0.txt,f1.txt,l0.txt,l1.txt,m0.txt,m1.txt" {
		t.Errorf("mid-2019 after the rest: %s", got)
	}
}

// TestCH8_ALateAddOnANameHeldForPartOfItsSpan (CH-8): x.txt, an on-time
// item, holds its name from 2020 until it moves aside in 2022; other.txt
// comes in 2024 (the folder's newest state, T). A late x.txt dated 2019
// is written into the present. After the settle each state holds one
// item a name: the late x in 2019 and 2023 (the name free), the on-time
// x in 2021 (it held the name then), the late x now.
func TestCH8_ALateAddOnANameHeldForPartOfItsSpan(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("ch8"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2018, "/dir/a.txt")
	if _, err := d.Write(at(ctx, 2020), "/dir/x.txt", strings.NewReader("first"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(at(ctx, 2022), drive.ByPath("/dir/x.txt"), "/dir/aside.txt", false); err != nil {
		t.Fatal(err)
	}
	writeAt(t, d, ctx, 2024, "/dir/other.txt")
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2019), "/dir/x.txt", strings.NewReader("late"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rows := late.Take()
	if len(rows) != 1 || !rows[0].Until.Equal(year(2024)) {
		t.Fatalf("rows %s, want one add until 2024", rowsUntil(rows))
	}
	if _, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		when        time.Time
		names, xHas string
	}{
		{midYear(2019), "a.txt,x.txt", "late"},
		{midYear(2021), "a.txt,x.txt", "first"},
		{midYear(2023), "a.txt,aside.txt,x.txt", "late"},
		{time.Now(), "a.txt,aside.txt,other.txt,x.txt", "late"},
	} {
		if got := sortedAt(t, d, "/dir", c.when); got != c.names {
			t.Errorf("/dir as of %v: %s, want %s", c.when.Format("2006-01"), got, c.names)
		}
		if x := read(t, ctx, d, drive.ByPath("/dir/x.txt"), drive.At{Time: c.when}); x != c.xHas {
			t.Errorf("/dir/x.txt as of %v: %q, want %q", c.when.Format("2006-01"), x, c.xHas)
		}
	}
}

// TestCH10_ARowWhoseFileIsRemovedBeforeTheSettle (CH-10): a late add of
// x (2021, until 2024), then, before the settle, x removed: on time
// (2025), or late itself (2022, its own row). The removal is a change of
// its own; the settle plays what the rows say: x in [2021, 2025) for the
// on-time removal, in [2021, 2022) for the late one; the present without
// x either way.
func TestCH10_ARowWhoseFileIsRemovedBeforeTheSettle(t *testing.T) {
	for _, c := range []struct {
		name   string
		remove int
		in     []int // the years whose mid-year shows x
	}{{"on time", 2025, []int{2021, 2022, 2023, 2024}}, {"late", 2022, []int{2021}}} {
		t.Run(c.name, func(t *testing.T) {
			d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("ch10"+strings.ReplaceAll(c.name, " ", "")), Config{DeferLateFolders: true})
			ctx := system()
			writeAt(t, d, ctx, 2020, "/dir/a.txt")
			writeAt(t, d, ctx, 2024, "/dir/c.txt")
			late := &drive.LateChanges{}
			lctx := drive.WithLateChanges(ctx, late)
			if _, err := d.Write(at(lctx, 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := d.Delete(at(lctx, c.remove), drive.ByPath("/dir/x.txt"), false, drive.Condition{}); err != nil {
				t.Fatal(err)
			}
			rows := late.Take()
			if _, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/dir"}); err != nil {
				t.Fatalf("rows %s: %v", rowsUntil(rows), err)
			}
			for y := 2020; y <= 2025; y++ {
				want := false
				for _, iy := range c.in {
					want = want || iy == y
				}
				if got := strings.Contains(sortedAt(t, d, "/dir", midYear(y)), "x.txt"); got != want {
					t.Errorf("mid-%d: x %v, want %v (rows %s)", y, got, want, rowsUntil(rows))
				}
			}
			if got := sortedAt(t, d, "/dir", time.Now()); strings.Contains(got, "x.txt") {
				t.Errorf("now: %s, x still there", got)
			}
		})
	}
}

// TestST22_AnotherWriterDuringTheReadsIsRefused (ST-22): another writer
// adds a file to the folder while the settle reads its states (at its
// second read, a middle one and the one before its last). The proof
// fails: refused (ErrSettleRefused), no record, nothing of the settle
// written; the other writer's file stays. One that lands at the last
// read (the present's own) is past what the proof reads: the settle's
// first window put finds it (ST-5: stopped, its record dropped, nothing
// of it left). (One that lands before the first read is no event: the
// settle reads it as the present.)
func TestST22_AnotherWriterDuringTheReadsIsRefused(t *testing.T) {
	raw := miniotest.Store(t)
	store := &failStore{Store: raw}
	cfg := Config{DeferLateFolders: true}
	// The reads of a settle of a folder like it, before its record.
	pd, pchanges := cutCase(t, store, "st22p", cfg)
	reads, precorded, pstarted := 0, false, false
	store.setWatch(func(op, key string) error {
		if !precorded && (pstarted || op == "HISTORY" && strings.TrimSuffix(key, ".metadata") == pchanges[0].Folder) {
			pstarted = true
			reads++
		}
		return nil
	})
	if _, err := pd.SettleFolder(system(), pchanges, drive.SettleOptions{Path: "/dir", Record: func(drive.SettleRecord) error { precorded = true; return nil }}); err != nil {
		t.Fatalf("the probe: %v", err)
	}
	store.setWatch(nil)
	for _, k := range []int{2, reads / 2, reads - 1, reads} {
		t.Run(fmt.Sprintf("written at read %d of %d", k, reads), func(t *testing.T) {
			name := fmt.Sprintf("st22%d", k)
			d, changes := cutCase(t, store, name, cfg)
			folder := changes[0].Folder
			other := newDriveWith(t, raw, cutCaseSettings(name), Config{})
			recorded, started, i, wrote := false, false, 0, false
			var settleWrites []string
			store.setWatch(func(op, key string) error {
				if wrote && isWrite(op) {
					settleWrites = append(settleWrites, op+" "+key)
				}
				if recorded || wrote {
					return nil
				}
				if started || op == "HISTORY" && strings.TrimSuffix(key, ".metadata") == folder {
					started = true
					if i++; i == k {
						wrote = true
						if _, err := other.Write(drive.WithSourceTime(system(), year(2026)), "/dir/zz.txt", strings.NewReader("zz"), drive.Condition{}, drive.WriteOptions{}); err != nil {
							return fmt.Errorf("the other writer: %v", err)
						}
					}
				}
				return nil
			})
			kp := &keeper{}
			opts := kp.opts()
			opts.Path = "/dir"
			rec := opts.Record
			opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
			_, err := d.SettleFolder(system(), changes, opts)
			store.setWatch(nil)
			if !wrote {
				t.Fatalf("the other writer never wrote: %v", err)
			}
			t.Logf("written at read %d: %v", k, err)
			switch {
			case k == reads && errors.Is(err, drive.ErrSettleStopped):
				// ST-5: its first window put found the write; nothing of it stays.
				if kp.held() != nil || kp.done != 1 || len(settleWrites) != 1 {
					t.Errorf("stopped at its first put: record held %v, done %d, writes %v", kp.held() != nil, kp.done, settleWrites)
				}
			case !errors.Is(err, drive.ErrSettleRefused) || recorded || len(settleWrites) > 0:
				t.Errorf("another writer during the reads: %v, record %v, the settle's writes %v; want refused, none", err, recorded, settleWrites)
			}
			if got := liveNames(t, d, folder); !strings.Contains(got, "zz.txt") {
				t.Errorf("the other writer's zz.txt is gone: %s", got)
			}
		})
	}
}

// TestCH6_TheSameChangeAgain (CH-6): a retried item writes the same late
// file again (the same name, the same date): it is already in the
// present, so no row is reported; the rows of the first write, settled,
// and then settled again, write nothing the second time.
func TestCH6_TheSameChangeAgain(t *testing.T) {
	counter := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d := newDriveWith(t, counter, cutCaseSettings("ch6"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2024, "/dir/c.txt")
	rows := r5Write(t, d, "/dir/x.txt", year(2021))
	if len(rows) != 1 || rows[0].Op != drive.LateAdd {
		t.Fatalf("the first write: rows %s", rowsUntil(rows))
	}
	if again := r5Write(t, d, "/dir/x.txt", year(2021)); len(again) != 0 {
		t.Errorf("the same change again: rows %s, want none", rowsUntil(again))
	}
	if _, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	writesSince(counter)
	s, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/dir", Record: func(drive.SettleRecord) error {
		t.Errorf("settled again: a record handed out")
		return nil
	}})
	if n, all := writesSince(counter); err != nil || n != 0 || s.Written != 0 {
		t.Errorf("settled again: %v, %+v, %d writes:%s", err, s, n, all)
	}
	pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2024: "a.txt,c.txt,x.txt"})
}

// TestCH7_TwoLateChangesToOneNameByDate (CH-7): x added late at 2022 and
// removed late at 2023 (the folder's newest state 2024), the rows given
// to the settle in the reverse of their dates: played by date, x is in
// 2022 only. (Two changes to one name at one time, in the order of the
// write: TestSettleSameAtTwoWrites.)
func TestCH7_TwoLateChangesToOneNameByDate(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("ch7"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2024, "/dir/c.txt")
	add := r5Write(t, d, "/dir/x.txt", year(2022))
	late := &drive.LateChanges{}
	if err := d.Delete(at(drive.WithLateChanges(ctx, late), 2023), drive.ByPath("/dir/x.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	rm := late.Take()
	if len(add) != 1 || len(rm) != 1 {
		t.Fatalf("rows: add %s, remove %s", rowsUntil(add), rowsUntil(rm))
	}
	if _, err := d.SettleFolder(ctx, append(rm, add...), drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	pastIs(t, d, map[int]string{2021: "a.txt", 2022: "a.txt,x.txt", 2023: "a.txt", 2024: "a.txt,c.txt"})
}

// TestCH9_RowsWithDifferentUntil (CH-9): x written late at 2021 when the
// folder's newest state was 2022 (Until 2022), x then removed on time in
// 2023, and y written late at 2021-06 when the newest was 2024 (Until
// 2024). Settled in one call, each only into its own [At, Until): x in
// 2021 (and, written, 2022), never 2023 or after; y from 2021-06 on.
func TestCH9_RowsWithDifferentUntil(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("ch9"), Config{DeferLateFolders: true})
	ctx := system()
	writeAt(t, d, ctx, 2020, "/dir/a.txt")
	writeAt(t, d, ctx, 2022, "/dir/b.txt")
	x := r5Write(t, d, "/dir/x.txt", year(2021))
	if err := d.Delete(at(ctx, 2023), drive.ByPath("/dir/x.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	writeAt(t, d, ctx, 2024, "/dir/c.txt")
	y := r5Write(t, d, "/dir/y.txt", rvMonth(2021, 6, 1))
	if len(x) != 1 || len(y) != 1 || !x[0].Until.Equal(year(2022)) || !y[0].Until.Equal(year(2024)) {
		t.Fatalf("rows: x %s, y %s; want Until 2022 and 2024", rowsUntil(x), rowsUntil(y))
	}
	if _, err := d.SettleFolder(ctx, append(x, y...), drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt,y.txt", 2022: "a.txt,b.txt,x.txt,y.txt",
		2023: "a.txt,b.txt,y.txt", 2024: "a.txt,b.txt,c.txt,y.txt"})
}

// TestCH15_ALateChangeToAFilesRecord (CH-15): a late change to a file's
// own record is written with its date, as before this design: the states
// from its date until the file's next version have it; the present keeps
// the record it had; no row.
func TestCH15_ALateChangeToAFilesRecord(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), cutCaseSettings("ch15"), Config{DeferLateFolders: true})
	ctx := system()
	r5Write(t, d, "/dir/f.txt", rvMonth(2021, 1, 1))
	r5Write(t, d, "/dir/f.txt", rvMonth(2021, 9, 1))
	late := &drive.LateChanges{}
	if _, err := d.SetInfo(drive.WithSourceTime(drive.WithLateChanges(ctx, late), rvMonth(2021, 5, 1)), drive.ByPath("/dir/f.txt"),
		drive.ItemInfo{App: map[string][]byte{"clio.k": []byte("may")}}); err != nil {
		t.Fatal(err)
	}
	if rows := late.Take(); len(rows) != 0 {
		t.Errorf("rows %s, want none", rowsUntil(rows))
	}
	for _, c := range []struct {
		at   time.Time
		want string
	}{{rvMonth(2021, 4, 1), ""}, {rvMonth(2021, 6, 1), "may"}, {time.Time{}, ""}} {
		it, err := d.Stat(ctx, drive.ByPath("/dir/f.txt"), drive.At{Time: c.at})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(it.App["clio.k"]); got != c.want {
			t.Errorf("f.txt's clio.k as of %v: %q, want %q", c.at.Format("2006-01"), got, c.want)
		}
	}
}

// TestCH16_ProveOnlyPartByPart (CH-16): more rows than one part, proved
// only: each part is proved against the store as it is, nothing written
// between them; then settled, the same rows go through.
func TestCH16_ProveOnlyPartByPart(t *testing.T) {
	saved := MaxSettleChanges
	MaxSettleChanges = 2
	t.Cleanup(func() { MaxSettleChanges = saved })
	store := &failStore{Store: miniotest.Store(t)}
	d := newDriveWith(t, store, cutCaseSettings("ch16"), Config{DeferLateFolders: true})
	for i, y := range []int{2016, 2018, 2020, 2024} {
		r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", i), year(y))
	}
	var rows []drive.LateChange
	for i := 0; i < 5; i++ {
		rows = append(rows, r5Write(t, d, fmt.Sprintf("/dir/l%d.txt", i), year(2016).AddDate(0, i*5, 1))...)
	}
	parts, writes := 0, 0
	store.setWatch(func(op, key string) error {
		if op == "HISTORY" && key == rows[0].Folder {
			parts++
		}
		if isWrite(op) {
			writes++
		}
		return nil
	})
	s, err := d.SettleFolder(system(), rows, drive.SettleOptions{Path: "/dir", ProveOnly: true})
	store.setWatch(nil)
	if err != nil || s.Changes != 5 || parts != 3 || writes != 0 {
		t.Fatalf("prove only in parts: %v, Changes %d, parts %d, writes %d; want nil, 5, 3, 0", err, s.Changes, parts, writes)
	}
	if _, err := d.SettleFolder(system(), rows, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	if got := sortedAt(t, d, "/dir", year(2018).AddDate(0, 3, 0)); got != "f0.txt,f1.txt,l0.txt,l1.txt,l2.txt,l3.txt,l4.txt" {
		t.Errorf("2018-04 after the settle: %s", got)
	}
}

// TestST27_AnotherWriterChangesOnlyTheRecordDuringTheReads (ST-27): as
// ST-22's test, the other writer changing only the folder's own record
// (its app data, on time): refused before any record, at every read but
// the last (where ST-5 may take it), nothing of the settle written.
func TestST27_AnotherWriterChangesOnlyTheRecordDuringTheReads(t *testing.T) {
	raw := miniotest.Store(t)
	store := &failStore{Store: raw}
	cfg := Config{DeferLateFolders: true}
	pd, pchanges := cutCase(t, store, "st27p", cfg)
	reads, precorded, pstarted := 0, false, false
	store.setWatch(func(op, key string) error {
		if !precorded && (pstarted || op == "HISTORY" && strings.TrimSuffix(key, ".metadata") == pchanges[0].Folder) {
			pstarted = true
			reads++
		}
		return nil
	})
	if _, err := pd.SettleFolder(system(), pchanges, drive.SettleOptions{Path: "/dir", Record: func(drive.SettleRecord) error { precorded = true; return nil }}); err != nil {
		t.Fatalf("the probe: %v", err)
	}
	store.setWatch(nil)
	for k := 2; k <= reads; k++ {
		t.Run(fmt.Sprintf("written at read %d of %d", k, reads), func(t *testing.T) {
			name := fmt.Sprintf("st27%d", k)
			d, changes := cutCase(t, store, name, cfg)
			folder := changes[0].Folder
			other := newDriveWith(t, raw, cutCaseSettings(name), Config{})
			recorded, started, i, wrote := false, false, 0, false
			var settleWrites []string
			store.setWatch(func(op, key string) error {
				if wrote && isWrite(op) {
					settleWrites = append(settleWrites, op+" "+key)
				}
				if recorded || wrote {
					return nil
				}
				if started || op == "HISTORY" && strings.TrimSuffix(key, ".metadata") == folder {
					started = true
					if i++; i == k {
						wrote = true
						if _, err := other.SetInfo(drive.WithSourceTime(system(), year(2026)), drive.ByPath("/dir"), drive.ItemInfo{App: map[string][]byte{"clio.k": []byte("other")}}); err != nil {
							return fmt.Errorf("the other writer: %v", err)
						}
					}
				}
				return nil
			})
			kp := &keeper{}
			opts := kp.opts()
			opts.Path = "/dir"
			rec := opts.Record
			opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
			_, err := d.SettleFolder(system(), changes, opts)
			store.setWatch(nil)
			if !wrote {
				t.Fatalf("the other writer never wrote: %v", err)
			}
			switch {
			case k == reads && errors.Is(err, drive.ErrSettleStopped):
				if kp.held() != nil || kp.done != 1 || len(settleWrites) != 1 {
					t.Errorf("stopped at its first put: record held %v, done %d, writes %v", kp.held() != nil, kp.done, settleWrites)
				}
			case !errors.Is(err, drive.ErrSettleRefused) || recorded || len(settleWrites) > 0:
				t.Errorf("the record changed during the reads: %v, record %v, the settle's writes %v; want refused, none", err, recorded, settleWrites)
			}
			it, serr := d.Stat(system(), drive.ByPath("/dir"), drive.At{})
			if serr != nil || string(it.App["clio.k"]) != "other" {
				t.Errorf("the other writer's record is gone: %v %q", serr, it.App["clio.k"])
			}
		})
	}
}

// TestST28_ARowNamingTheWrongItemIsProved (ST-28, a known gap, pinned so
// a change to it is seen): a row whose name is right and whose item is
// not (only a damaged database gives one) passes the proof: the proof
// checks names against the present only from Until on.
func TestST28_ARowNamingTheWrongItemIsProved(t *testing.T) {
	d, changes := cutCase(t, &failStore{Store: miniotest.Store(t)}, "st28", Config{DeferLateFolders: true})
	bad := append([]drive.LateChange(nil), changes...)
	bad[0].Item = "not-the-item"
	if _, err := d.SettleFolder(system(), bad, drive.SettleOptions{Path: "/dir", ProveOnly: true}); err != nil {
		t.Errorf("the known gap closed? a row naming the wrong item: %v (update ST-28)", err)
	}
}
