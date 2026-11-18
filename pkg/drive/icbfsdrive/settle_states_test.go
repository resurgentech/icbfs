package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// Tests named for the rows of interloc's docs/STATES.md: section 1.3
// (one settle), rows LF-13, LF-17 and CH-12, and section 1.6's bound on
// the rows a caller holds. Each runs in seconds against one MinIO.

// setWatch sets (or, nil, clears) the store's watch.
func (s *failStore) setWatch(w func(op, key string) error) {
	s.mu.Lock()
	s.watch = w
	s.mu.Unlock()
}

// isWrite is whether a request writes the store.
func isWrite(op string) bool {
	return strings.HasPrefix(op, "PUT") || strings.HasPrefix(op, "ERASE") || op == "DELETE" || op == "UNDELETE"
}

// splitCaseAfter is splitCase's /dir after its settle, mid-year: late.txt
// from 2021 on.
var splitCaseAfter = map[int]string{
	2020: "fa.txt,fb.txt,fc.txt,fd.txt,fe.txt",
	2021: "fa.txt,fb.txt,fc.txt,fd.txt,fe.txt,late.txt",
	2023: "fa.txt,fb.txt,fc.txt,fd.txt,fe.txt,ff.txt,fg.txt,late.txt",
}

// TestST9_LF17_ImmediateStopCutsTheSettle (ST-9, LF-17, ST-8, ST-4): an
// immediate stop (the caller's context canceled) at the settle's window,
// its commit, its erases or its blocks below the root ends the request in
// flight and starts no other; the record stays, and a rerun with it, as
// after a kill, finishes the settle. A graceful stop (the caller lets the
// settle run: nothing is canceled) runs to Done. A store away below the
// root (ST-4) leaves the record too.
func TestST9_LF17_ImmediateStopCutsTheSettle(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	cfg := Config{DeferLateFolders: true}
	d0, ch0 := splitCase(t, store, "st9base", cfg)
	if _, err := d0.SettleFolder(system(), ch0, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		name, at, stop string
	}{
		{"ST-8 graceful in the window", "window", "graceful"},
		{"ST-9 immediate in the window", "window", "immediate"},
		{"ST-9 immediate at the commit", "commit", "immediate"},
		{"ST-9 immediate at the erases", "erase", "immediate"},
		{"ST-4 immediate below the root", "blocks", "immediate"},
		{"ST-4 store away below the root", "blocks", "store"},
	} {
		t.Run(c.name, func(t *testing.T) {
			name := fmt.Sprintf("st9c%d", i)
			d, changes := splitCase(t, store, name, cfg)
			folder := changes[0].Folder
			before := liveNames(t, d, folder)
			ctx, cancel := context.WithCancel(system())
			defer cancel()
			k := &keeper{}
			opts := k.opts()
			opts.Path = "/dir"
			var recorded atomic.Bool
			rec := opts.Record
			opts.Record = func(r drive.SettleRecord) error {
				err := rec(r)
				recorded.Store(true)
				return err
			}
			var after []string
			hit, puts, committed := "", 0, false
			store.setWatch(func(op, key string) error {
				if hit != "" {
					after = append(after, op+" "+key)
					if c.stop == "store" {
						return errFail
					}
					return nil
				}
				if !recorded.Load() {
					return nil
				}
				win := key == folder || key == folder+".metadata"
				ver := key == folder+objstore.VersionSuffix
				if win && op == "PUT" {
					puts++
				}
				if ver && puts > 0 {
					committed = true
				}
				at := false
				switch c.at {
				case "blocks":
					at = strings.HasPrefix(op, "PUT") && !win && !ver
				case "window":
					at = win && op == "PUT" && puts == 2
				case "commit":
					at = ver && puts > 0
				case "erase":
					at = committed && (strings.HasPrefix(op, "ERASE") || op == "DELETE")
				}
				if !at {
					return nil
				}
				hit = op + " " + key
				switch c.stop {
				case "immediate":
					cancel()
				case "store":
					return errFail
				}
				return nil
			})
			_, err := d.SettleFolder(ctx, changes, opts)
			store.setWatch(nil)
			if hit == "" {
				t.Fatalf("the settle never reached its %s", c.at)
			}
			if c.stop == "graceful" {
				if err != nil || k.done != 1 || k.held() != nil {
					t.Fatalf("a graceful stop: %v, done %d, record held %v; want Done", err, k.done, k.held() != nil)
				}
				pastIs(t, d, splitCaseAfter)
				return
			}
			if err == nil {
				t.Fatalf("stopped at %s, and the settle ran to its end", hit)
			}
			if c.stop == "immediate" {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("stopped at %s: %v, want context.Canceled", hit, err)
				}
				if len(after) > 0 {
					t.Errorf("stopped at %s, and %d requests after it: %v", hit, len(after), after)
				}
			}
			if k.held() == nil || k.done != 0 {
				t.Fatalf("stopped at %s: record held %v, done %d; want held", hit, k.held() != nil, k.done)
			}
			// The rerun, from a new process, with the record.
			d = newDriveWith(t, store, icbfs.Settings{Name: name}, cfg)
			opts = k.opts()
			opts.Path = "/dir"
			if _, err := d.SettleFolder(system(), changes, opts); err != nil {
				t.Fatalf("the rerun: %v", err)
			}
			if k.held() != nil || k.done != 1 {
				t.Errorf("after the rerun: record held %v, done %d", k.held() != nil, k.done)
			}
			if got := liveNames(t, d, folder); got != before {
				t.Errorf("the folder now: %s, want %s as before", got, before)
			}
			pastIs(t, d, splitCaseAfter)
			settleLeftovers(t, d, folder, 1<<20)
		})
	}
}

// TestST1_ReadsCutOrStoppedWriteNothing (ST-1): a settle stopped in its
// reads (a store error from a request on, the process as good as killed;
// or an immediate stop) writes nothing and hands out no record.
func TestST1_ReadsCutOrStoppedWriteNothing(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	for i, c := range []struct {
		name string
		at   int
		stop bool
	}{{"store away at its first request", 1, false}, {"store away at its fifth", 5, false}, {"immediate stop at its first", 1, true}, {"immediate stop at its fifth", 5, true}} {
		t.Run(c.name, func(t *testing.T) {
			d, changes := splitCase(t, store, fmt.Sprintf("st1c%d", i), Config{DeferLateFolders: true})
			snap := versionSnapshot(t, store)
			ctx, cancel := context.WithCancel(system())
			defer cancel()
			n := 0
			var writes []string
			store.setWatch(func(op, key string) error {
				n++
				if isWrite(op) {
					writes = append(writes, op+" "+key)
				}
				if n >= c.at {
					if c.stop {
						cancel()
						return nil
					}
					return errFail
				}
				return nil
			})
			recorded := false
			_, err := d.SettleFolder(ctx, changes, drive.SettleOptions{Path: "/dir", Record: func(drive.SettleRecord) error { recorded = true; return nil }})
			store.setWatch(nil)
			if err == nil {
				t.Fatalf("stopped at request %d, and no error", c.at)
			}
			if recorded || len(writes) > 0 {
				t.Errorf("stopped in its reads (%v): record handed out %v, writes %v", err, recorded, writes)
			}
			if got := versionSnapshot(t, store); got != snap {
				t.Errorf("the store changed")
			}
		})
	}
}

// TestST12_DoneFailsTheRerunOnlyDropsTheRecord (ST-12): the caller cannot
// drop the record (Done fails) after the settle committed and erased; the
// rerun with the record finds everything done, writes nothing, and tells
// Done.
func TestST12_DoneFailsTheRerunOnlyDropsTheRecord(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	cfg := Config{DeferLateFolders: true}
	d, changes := splitCase(t, store, "st12", cfg)
	k := &keeper{}
	opts := k.opts()
	opts.Path = "/dir"
	opts.Done = func(drive.SettleRecord) error { return errString("the daemon's database is away") }
	if _, err := d.SettleFolder(system(), changes, opts); err == nil || !strings.Contains(err.Error(), "database is away") {
		t.Fatalf("Done failed: %v", err)
	}
	if k.held() == nil {
		t.Fatalf("Done failed, and no record held")
	}
	pastIs(t, d, splitCaseAfter)
	d = newDriveWith(t, store, icbfs.Settings{Name: "st12"}, cfg)
	var writes []string
	store.setWatch(func(op, key string) error {
		if isWrite(op) {
			writes = append(writes, op+" "+key)
		}
		return nil
	})
	opts = k.opts()
	opts.Path = "/dir"
	s, err := d.SettleFolder(system(), changes, opts)
	store.setWatch(nil)
	if err != nil {
		t.Fatalf("the rerun: %v", err)
	}
	if k.held() != nil || k.done != 1 {
		t.Errorf("after the rerun: record held %v, done %d", k.held() != nil, k.done)
	}
	if len(writes) > 0 || s.Written != 0 {
		t.Errorf("the rerun wrote %d states and made %d writes: %v", s.Written, len(writes), writes)
	}
	pastIs(t, d, splitCaseAfter)
}

// TestST13_PartKFailsTheEarlierPartsStaySettled (ST-13): in parts, a part
// that fails leaves the parts before it settled (their records Done) and
// says how many of the changes those were (Settled.Changes), so the
// caller deletes those rows and keeps the rest; settling the rest then
// finishes.
// STATES: ST-17
func TestST13_PartKFailsTheEarlierPartsStaySettled(t *testing.T) {
	saved := MaxSettleChanges
	MaxSettleChanges = 2
	t.Cleanup(func() { MaxSettleChanges = saved })
	store := &failStore{Store: miniotest.Store(t)}
	d := newDriveWith(t, store, cutCaseSettings("st13"), Config{DeferLateFolders: true})
	for i, y := range []int{2016, 2018, 2020, 2022, 2024} {
		r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", i), year(y))
	}
	var rows []drive.LateChange
	for i := 0; i < 5; i++ {
		rows = append(rows, r5Write(t, d, fmt.Sprintf("/dir/l%d.txt", i), year(2016).AddDate(0, i*3, 1))...)
	}
	k := &keeper{}
	opts := k.opts()
	opts.Path = "/dir"
	records := 0
	rec := opts.Record
	opts.Record = func(r drive.SettleRecord) error {
		if records++; records == 3 {
			return errString("the daemon's database is away")
		}
		return rec(r)
	}
	s, err := d.SettleFolder(system(), rows, opts)
	if err == nil {
		t.Fatalf("the third part's record refused, and no error")
	}
	if s.Changes != 4 || k.done != 2 || k.held() != nil {
		t.Fatalf("the third part failed: Changes %d, done %d, record held %v; want 4, 2, none", s.Changes, k.done, k.held() != nil)
	}
	// Mid-2017 is after every late file's date (l4's is 2017-01-02) and
	// before their Until (2024).
	if got, want := sortedAt(t, d, "/dir", midYear(2017)), "f0.txt,l0.txt,l1.txt,l2.txt,l3.txt"; got != want {
		t.Errorf("mid-2017 after parts 1 and 2: %s, want %s", got, want)
	}
	opts = k.opts()
	opts.Path = "/dir"
	if s, err := d.SettleFolder(system(), rows[s.Changes:], opts); err != nil || s.Changes != 1 {
		t.Fatalf("the rest: %v, Changes %d", err, s.Changes)
	}
	if got, want := sortedAt(t, d, "/dir", midYear(2017)), "f0.txt,l0.txt,l1.txt,l2.txt,l3.txt,l4.txt"; got != want {
		t.Errorf("mid-2017 after the rest: %s, want %s", got, want)
	}

	// An immediate stop as the first part ends: no other part begins.
	var late []drive.LateChange
	for i := 0; i < 5; i++ {
		late = append(late, r5Write(t, d, fmt.Sprintf("/dir/m%d.txt", i), year(2017).AddDate(0, i*2, 1))...)
	}
	ctx, cancel := context.WithCancel(system())
	defer cancel()
	k = &keeper{}
	opts = k.opts()
	opts.Path = "/dir"
	done := opts.Done
	var after []string
	stopped := false
	opts.Done = func(r drive.SettleRecord) error {
		cancel()
		stopped = true
		return done(r)
	}
	store.setWatch(func(op, key string) error {
		if stopped {
			after = append(after, op+" "+key)
		}
		return nil
	})
	s, err = d.SettleFolder(ctx, late, opts)
	store.setWatch(nil)
	if !errors.Is(err, context.Canceled) || s.Changes != 2 || k.done != 1 || len(after) > 0 {
		t.Errorf("stopped as part 1 ended: %v, Changes %d, done %d, requests after %v; want canceled, 2, 1, none", err, s.Changes, k.done, after)
	}
	// ST-17: the part done is settled (m0, m1 from their dates); the next
	// part's rows (m2 from 2017-05-02 on) are not.
	if got := sortedAt(t, d, "/dir", time.Date(2017, 6, 1, 0, 0, 0, 0, time.UTC)); !strings.Contains(got, "m0.txt") || !strings.Contains(got, "m1.txt") || strings.Contains(got, "m2.txt") {
		t.Errorf("stopped as part 1 ended: 2017-06 is %s; want m0 and m1 settled, m2 not", got)
	}
}

// TestCH12_EachInvalidRowNamesItsFault (CH-12): a row with no time, a
// time before 1970 or in the future, no Until, an Until before 1970 or
// in the future, or an Until not after its time is refused before
// anything is read, with an error that names its own fault and no other.
func TestCH12_EachInvalidRowNamesItsFault(t *testing.T) {
	counter := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d, changes := cutCase(t, counter, "ch12", Config{DeferLateFolders: true})
	good := changes[0]
	future := time.Now().Add(time.Hour)
	faults := []string{"its time (At) is not set", "its time (At) is before 1970", "its time (At) is in the future",
		"its Until is not set", "its Until is before 1970", "its Until is in the future", "its Until is not after its time"}
	phrase := func(f string) string { return f }
	for i, c := range []struct {
		name string
		row  func(*drive.LateChange)
	}{
		{"no time", func(r *drive.LateChange) { r.At = time.Time{} }},
		{"a time before 1970", func(r *drive.LateChange) { r.At = time.Date(1969, 7, 20, 0, 0, 0, 0, time.UTC) }},
		{"a time in the future", func(r *drive.LateChange) { r.At, r.Until = future, future.Add(time.Hour) }},
		{"no Until", func(r *drive.LateChange) { r.Until = time.Time{} }},
		{"an Until before 1970", func(r *drive.LateChange) { r.Until = time.Date(1969, 7, 20, 0, 0, 0, 0, time.UTC) }},
		{"an Until in the future", func(r *drive.LateChange) { r.Until = future }},
		{"an Until at its time", func(r *drive.LateChange) { r.Until = r.At }},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := good
			c.row(&row)
			counter.take()
			_, err := d.SettleFolder(system(), []drive.LateChange{good, row}, drive.SettleOptions{Path: "/dir"})
			if n, all := counter.take(); n != 0 {
				t.Errorf("%d requests before the refusal: %s", n, all)
			}
			if !errors.Is(err, drive.ErrInvalid) {
				t.Fatalf("%v, want ErrInvalid", err)
			}
			msg := err.Error()
			if want := phrase(faults[i]); !strings.Contains(msg, want) {
				t.Errorf("%q does not name its fault %q", msg, want)
			}
			if !strings.Contains(msg, "late change 1") {
				t.Errorf("%q does not name the row", msg)
			}
			for j, f := range faults {
				if j != i && strings.Contains(msg, phrase(f)) {
					t.Errorf("%q names another fault too: %q", msg, phrase(f))
				}
			}
		})
	}
}

// TestLF13_FolderNotAtItsPathSaysSo (LF-13): a settle is given the
// folder's id and the path its caller holds; a folder that is not there
// with that id (nothing there, another folder there, the folder moved
// away) answers ErrFolderNotAtPath, not "gone" and not an ErrNotFound,
// and nothing outside that path is read (no walk of the drive). Given
// its new path it settles. No path, and no record: ErrInvalid. A store
// error on the way is that error, never "not there".
// STATES: ST-20
func TestLF13_FolderNotAtItsPathSaysSo(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	cfg := Config{DeferLateFolders: true}
	d, changes := cutCase(t, store, "lf13", cfg)
	others := map[string]bool{}
	for i := 0; i < 12; i++ {
		writeAt(t, d, system(), 2025, fmt.Sprintf("/o/p%d/f.txt", i))
		n, err := d.resolve(system(), d.fs, drive.ByPath(fmt.Sprintf("/o/p%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if i != 3 && i != 5 { // on the paths given below
			others[n.key] = true
		}
	}
	notThere := func(t *testing.T, d *Drive, path string) {
		t.Helper()
		walked := map[string]bool{}
		store.setWatch(func(op, key string) error {
			if others[strings.TrimSuffix(strings.TrimSuffix(key, objstore.VersionSuffix), ".metadata")] {
				walked[key] = true
			}
			return nil
		})
		_, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: path})
		store.setWatch(nil)
		if !errors.Is(err, drive.ErrFolderNotAtPath) {
			t.Errorf("path %q: %v, want ErrFolderNotAtPath", path, err)
		}
		if errors.Is(err, drive.ErrNotFound) {
			t.Errorf("path %q: %v says not found", path, err)
		}
		if len(walked) > 0 {
			t.Errorf("path %q: read %d keys of other folders (a walk): %v", path, len(walked), walked)
		}
	}
	notThere(t, d, "/nothing")
	notThere(t, d, "/o/p3")
	notThere(t, d, "/o/p3/f.txt")
	// ST-19: no path, and no record: refused before anything is read.
	var noPath []string
	store.setWatch(func(op, key string) error { noPath = append(noPath, op+" "+key); return nil })
	_, err := d.SettleFolder(system(), changes, drive.SettleOptions{})
	store.setWatch(nil)
	if !errors.Is(err, drive.ErrInvalid) || len(noPath) > 0 {
		t.Errorf("no path: %v after %d requests %v; want ErrInvalid before any", err, len(noPath), noPath)
	}
	// Moved: not at the old path; at the new one, settled.
	if _, err := d.Move(system(), drive.ByPath("/dir"), "/o/p5/dir", false); err != nil {
		t.Fatal(err)
	}
	d = newDriveWith(t, store, icbfs.Settings{Name: "lf13"}, cfg)
	notThere(t, d, "/dir")
	// A store error on the way to the path: that error.
	d2 := newDriveWith(t, store, icbfs.Settings{Name: "lf13"}, cfg)
	p5, err := d.resolve(system(), d.fs, drive.ByPath("/o/p5"))
	if err != nil {
		t.Fatal(err)
	}
	store.setWatch(func(op, key string) error {
		if strings.HasPrefix(key, p5.key) {
			return errFail
		}
		return nil
	})
	_, err = d2.SettleFolder(system(), changes, drive.SettleOptions{Path: "/o/p5/dir"})
	store.setWatch(nil)
	if err == nil || errors.Is(err, drive.ErrFolderNotAtPath) || errors.Is(err, drive.ErrNotFound) {
		t.Errorf("a store error on the way: %v, want that error", err)
	}
	if _, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/o/p5/dir"}); err != nil {
		t.Fatalf("at its new path: %v", err)
	}
	if got := sortedAt(t, d, "/o/p5/dir", midYear(2021)); got != "a.txt,x.txt" {
		t.Errorf("mid-2021 after the settle: %s", got)
	}
}

// TestBound16_BatchesInOrderSettleAsOneCall (1.6's bound: the caller
// hands a folder's rows over in batches and never holds them all):
// successive calls with consecutive batches of the rows, in their order
// (At, then the order stored), leave every view of the folder exactly as
// one call with all the rows does (each entry's name, type and size,
// and the folder's own record, at every state's time, a second before
// and an hour after, mid-year, and now). A batch cut in its window is
// finished by a rerun with its record given that batch's rows alone, or
// that batch's and every later one's at once; the rest follow.
func TestBound16_BatchesInOrderSettleAsOneCall(t *testing.T) {
	if os.Getenv("ICBFS_PRE_MERGE") == "" {
		t.Skip("about 90 s under -race: set ICBFS_PRE_MERGE=1 (just pre-merge); in ci, TestST13_PartKFailsTheEarlierPartsStaySettled and TestSettleInParts settle in parts")
	}
	store := &failStore{Store: miniotest.Store(t)}
	cfg := Config{DeferLateFolders: true}
	settings := icbfs.Settings{Name: "bound16", Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 4}
	d := newDriveWith(t, store, settings, cfg)
	onTime := []time.Time{rvMonth(2016, 1, 1), rvMonth(2017, 1, 1), rvMonth(2018, 1, 1), rvMonth(2019, 1, 1), rvMonth(2020, 1, 1), rvMonth(2023, 1, 1)}
	var dates []time.Time
	build := func(dir string) []drive.LateChange {
		for i, at := range onTime {
			r5Write(t, d, fmt.Sprintf("%s/f%d.txt", dir, i), at)
		}
		var rows []drive.LateChange
		late := func(name string, at time.Time) {
			rows = append(rows, r5Write(t, d, dir+"/"+name, at)...)
		}
		late("l0.txt", rvMonth(2016, 3, 1))
		late("l1.txt", rvMonth(2017, 5, 1))
		late("l2.txt", rvMonth(2017, 5, 1)) // the same time as l1, written after it
		rm := &drive.LateChanges{}
		if err := d.Delete(drive.WithSourceTime(drive.WithLateChanges(system(), rm), rvMonth(2018, 6, 1)), drive.ByPath(dir+"/f1.txt"), false, drive.Condition{}); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, rm.Take()...)
		late("l3.txt", rvMonth(2019, 7, 1))
		late("l4.txt", rvMonth(2016, 1, 15))
		late("l5.txt", rvMonth(2020, 2, 1))
		n, err := d.resolve(system(), d.fs, drive.ByPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		// This folder's rows (a removal may report its trash's too), as the
		// daemon reads them back: by At, then in the order stored.
		var own []drive.LateChange
		for _, r := range rows {
			if r.Folder == n.key {
				own = append(own, r)
			}
		}
		sort.SliceStable(own, func(i, j int) bool { return own[i].At.Before(own[j].At) })
		if len(own) != 7 {
			t.Fatalf("%s: %d rows, want 7: %+v", dir, len(own), rows)
		}
		return own
	}
	type folder struct {
		path string
		rows []drive.LateChange
	}
	var fs []folder
	for _, p := range []string{"/one", "/batches", "/cutexact", "/cutmore"} {
		fs = append(fs, folder{p, build(p)})
	}
	for _, r := range fs[0].rows {
		dates = append(dates, r.At, r.Until)
	}
	dates = append(dates, onTime...)
	settle := func(d *Drive, f folder, rows []drive.LateChange, k *keeper) error {
		opts := drive.SettleOptions{Path: f.path}
		if k != nil {
			opts = k.opts()
			opts.Path = f.path
		}
		_, err := d.SettleFolder(system(), rows, opts)
		return err
	}
	if err := settle(d, fs[0], fs[0].rows, nil); err != nil {
		t.Fatal(err)
	}
	const batch = 2
	for i := 0; i < len(fs[1].rows); i += batch {
		if err := settle(d, fs[1], fs[1].rows[i:min(i+batch, len(fs[1].rows))], nil); err != nil {
			t.Fatalf("batch at %d: %v", i, err)
		}
	}
	// The second batch cut in its window, then rerun with its record.
	for _, f := range fs[2:] {
		rows := f.rows
		if err := settle(d, f, rows[:batch], nil); err != nil {
			t.Fatal(err)
		}
		key := rows[0].Folder
		puts := 0
		store.setWatch(func(op, k string) error {
			if op == "PUT" && (k == key || k == key+".metadata") {
				puts++
			}
			if puts >= 2 {
				return errFail
			}
			return nil
		})
		k := &keeper{}
		err := settle(d, f, rows[batch:2*batch], k)
		store.setWatch(nil)
		if err == nil || k.held() == nil {
			t.Fatalf("%s: the second batch not cut in its window: %v, record held %v", f.path, err, k.held() != nil)
		}
		d = newDriveWith(t, store, icbfs.Settings{Name: "bound16"}, cfg)
		rest := rows[2*batch:]
		if f.path == "/cutexact" {
			if err := settle(d, f, rows[batch:2*batch], k); err != nil {
				t.Fatalf("%s: the rerun with its batch: %v", f.path, err)
			}
			for i := 0; i < len(rest); i += batch {
				if err := settle(d, f, rest[i:min(i+batch, len(rest))], nil); err != nil {
					t.Fatal(err)
				}
			}
		} else if err := settle(d, f, rows[batch:], k); err != nil {
			t.Fatalf("%s: the rerun with its batch and the rest: %v", f.path, err)
		}
		if k.held() != nil {
			t.Errorf("%s: record held after the rerun", f.path)
		}
	}
	var at []time.Time
	seen := map[time.Time]bool{}
	for _, t0 := range dates {
		if !seen[t0] {
			seen[t0] = true
			at = append(at, t0.Add(-time.Second), t0, t0.Add(time.Hour))
		}
	}
	for y := 2015; y <= 2026; y++ {
		at = append(at, midYear(y))
	}
	at = append(at, time.Time{})
	view := func(dir string, when time.Time) string {
		t.Helper()
		ctx := system()
		n, err := d.resolve(ctx, d.fs, drive.ByPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		fsys, err := d.at(ctx, drive.At{Time: when})
		if err != nil {
			t.Fatal(err)
		}
		entries, err := fsys.ReadDir(ctx, n.key)
		if errors.Is(err, icbfs.ErrNotFound) || objstore.IsNotFound(err) {
			return "(none)"
		}
		if err != nil {
			t.Fatalf("%s as of %v: %v", dir, when, err)
		}
		var out []string
		for _, e := range entries {
			a, err := fsys.Stat(ctx, e.UUID, e.Type)
			if err != nil {
				t.Fatalf("%s/%s as of %v: %v", dir, e.Name, when, err)
			}
			// Each file holds its own path: its size less its folder's.
			out = append(out, fmt.Sprintf("%s:%d:%d", e.Name, e.Type, a.Size-int64(len(dir))))
		}
		sort.Strings(out)
		da, err := fsys.Stat(ctx, n.key, icbfs.TypeDir)
		if err != nil {
			t.Fatalf("%s's record as of %v: %v", dir, when, err)
		}
		return fmt.Sprintf("%s mode %o mtime %s", strings.Join(out, ","), da.Mode, da.Mtime.UTC().Format(time.RFC3339Nano))
	}
	for _, when := range at {
		want := view("/one", when)
		for _, f := range fs[1:] {
			if got := view(f.path, when); got != want {
				t.Errorf("%s as of %s:\n  %s\nwant (one call):\n  %s", f.path, when.Format(time.RFC3339), got, want)
			}
		}
	}
}

// settleAt is d.SettleFolder given the folder's path, as a daemon gives it
// from its own table, when the test gives none (tests written before
// SettleFolder required it): found by a search of the drive's tree,
// through a drive of its own over the store underneath any test wrapper,
// so that the test's counts and cuts see the settle's requests only. A
// folder found nowhere is given no path.
func settleAt(d *Drive, ctx context.Context, changes []drive.LateChange, opts drive.SettleOptions) (drive.Settled, error) {
	if opts.Path == "" && len(changes) > 0 {
		opts.Path = folderPath(d, changes[0].Folder)
	}
	return d.SettleFolder(ctx, changes, opts)
}

// folderPath is folder's path in d, found by a search ("" if none).
func folderPath(d *Drive, folder string) string {
	store := d.cfg.Store
	for unwrapped := false; !unwrapped; {
		switch w := store.(type) {
		case *failStore:
			store = w.Store
		case *requestCounter:
			store = w.Store
		case *rvStore:
			store = w.Store
		case *cutStore:
			store = w.Store
		case *seqStore:
			store = w.Store
		case *lostStore:
			store = w.Store
		default:
			unwrapped = true
		}
	}
	dir, err := os.MkdirTemp("", "icbfs-settle-path-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	cfg := d.cfg
	cfg.Store, cfg.ID, cfg.CacheDir, cfg.History = store, d.cfg.ID+"-finder", dir, nil
	finder, err := New(context.Background(), cfg)
	if err != nil {
		panic(err)
	}
	defer finder.Close()
	if folder == finder.rootKey {
		return "/"
	}
	paths, err := finder.findPaths(system(), finder.fs, folder, 1)
	if err != nil || len(paths) == 0 {
		return ""
	}
	return paths[0]
}
