package icbfsdrive

// Round 5 of T83-1: the fourth review's findings R2 to R6 and D3, as
// tests that fail on 1c53181 and pass after the fixes.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
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

// r5Write writes p at at through a deferring drive, collecting its late
// changes.
func r5Write(t *testing.T, d *Drive, p string, at time.Time) []drive.LateChange {
	t.Helper()
	late := &drive.LateChanges{}
	ctx := drive.WithLateChanges(system(), late)
	if !at.IsZero() {
		ctx = drive.WithSourceTime(ctx, at)
	}
	if _, err := d.Write(ctx, p, strings.NewReader(p), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
		t.Fatalf("%s at %v: %v", p, at, err)
	}
	return late.Take()
}

// r5Twice is the names in a listing that appear twice.
func r5Twice(list string) string {
	names := strings.Split(list, ",")
	sort.Strings(names)
	var out []string
	for i := 1; i < len(names); i++ {
		if names[i] == names[i-1] {
			out = append(out, names[i])
		}
	}
	return strings.Join(out, ",")
}

// TestKilledWriteLeavesTheFolderSettleable (R3): a dated write into /dir
// (f0..f7 dated 2011-01 to 2011-08) killed at each of its requests in turn
// (that request and every later one fail, the undo's too: the process
// died), on time (x1 at 2011-10) and late (x1 at 2011-02), in a folder of
// one block and of blocks of 4. A new drive then tries x1 again, writes x2
// on time (2011-11) and x3 late (2011-02-15), and settles every late
// change those report (as the daemon queues them). Every time:
// the settle is not refused; the view "as of now" is the present; no view
// holds a name twice; f0..f7, x2 and x3 are where their dates put them;
// x1 is there from no later than x2's date on (its own date if the write
// that put it in the folder is the one that was dated, else the next
// write's: a killed write's leftover is dated by the write made on top of
// it, never by the store's clock).
func TestKilledWriteLeavesTheFolderSettleable(t *testing.T) {
	if !envSet("T83_ALWAYS") {
		t.Skip("T83_ALWAYS unset: `just test-settle` runs it (without -race)")
	}
	bucket := newBuckets(t)
	for _, entries := range []uint32{0, 4} {
		for _, lateX1 := range []bool{false, true} {
			x1At := rvMonth(2011, 10, 1)
			if lateX1 {
				x1At = rvMonth(2011, 2, 1)
			}
			base := func(d *Drive) {
				for i := 0; i < 8; i++ {
					r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", i), rvMonth(2011, 1+i, 1))
				}
			}
			x1 := func(d *Drive) error {
				_, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), &drive.LateChanges{}), x1At), "/dir/x1.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{})
				return err
			}
			st := newRVStore(bucket(t))
			d := newDriveWith(t, st, icbfs.Settings{Name: fmt.Sprintf("kill%d%v", entries, lateX1), Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries}, Config{DeferLateFolders: true})
			base(d)
			st.take()
			if err := x1(d); err != nil {
				t.Fatal(err)
			}
			n, _, _, _ := st.take()
			for k := 1; k <= n; k++ {
				t.Run(fmt.Sprintf("blocks of %d, x1 late %v, killed at %d of %d", entries, lateX1, k, n), func(t *testing.T) {
					t.Parallel()
					st := newRVStore(bucket(t))
					name := fmt.Sprintf("k%d%v%d", entries, lateX1, k)
					d := newDriveWith(t, st, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries}, Config{DeferLateFolders: true})
					base(d)
					st.setFail(k, "lost")
					_ = x1(d)
					st.setFail(0, "")
					d = newDriveWith(t, st, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
					// The daemon queues every late change these writes report.
					late := &drive.LateChanges{}
					if _, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), x1At), "/dir/x1.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
						t.Fatalf("x1 again: %v", err)
					}
					rows := late.Take()
					rows = append(rows, r5Write(t, d, "/dir/x2.txt", rvMonth(2011, 11, 1))...)
					rows = append(rows, r5Write(t, d, "/dir/x3.txt", rvMonth(2011, 2, 15))...)
					if _, err := settleAt(d, system(), rows, (&keeper{}).opts()); err != nil {
						t.Fatalf("the settle: %v", err)
					}
					present := rvNamesAt(t, d, "/dir", time.Time{})
					if now := rvNamesAt(t, d, "/dir", time.Now()); now != present {
						t.Errorf("as of now %q, the present %q", now, present)
					}
					if !strings.Contains(present, "x1.txt") {
						t.Errorf("present %q without x1", present)
					}
					dumped := false
					for m := 0; m < 14; m++ {
						at := rvMonth(2011, 1+m, 1).Add(12 * time.Hour)
						got := rvNamesAt(t, d, "/dir", at)
						if tw := r5Twice(got); tw != "" {
							t.Errorf("as of %s: %q has %s twice", at.Format("2006-01-02"), got, tw)
						}
						var want []string
						for i := 0; i < 8 && i <= m; i++ {
							want = append(want, fmt.Sprintf("f%d.txt", i))
						}
						if !at.Before(rvMonth(2011, 2, 15)) {
							want = append(want, "x3.txt")
						}
						if !at.Before(rvMonth(2011, 11, 1)) {
							want = append(want, "x1.txt", "x2.txt")
						}
						sort.Strings(want)
						w := strings.Join(want, ",")
						alt := ""
						if !at.Before(x1At) && at.Before(rvMonth(2011, 11, 1)) {
							a := append(append([]string(nil), want...), "x1.txt")
							sort.Strings(a)
							alt = strings.Join(a, ",")
						}
						if got != w && got != alt {
							t.Errorf("as of %s: %q, want %q (or %q)", at.Format("2006-01-02"), got, w, alt)
							if !dumped {
								dumped = true
								r5Dump(t, d)
							}
						}
					}
				})
			}
		}
	}
}

// TestSettleKeepsTheFolderRecord (R4): /dir a (2011-01), b (2011-03), its
// own record changed on time at 2011-06 (mode 0700, an app key), c
// (2011-09); a late x at 2011-02-15, settled. The past of the folder's
// record is as it was: before 2011-06 mode 0755 and no key, from 2011-06
// mode 0700 and the key, before and after the settle; the record-only
// state at 2011-06 stays a state (a later late write's Until is 2011-09,
// the folder's newest, not earlier).
func TestSettleKeepsTheFolderRecord(t *testing.T) {
	for _, entries := range []uint32{0, 4} {
		t.Run(fmt.Sprintf("blocks of %d", entries), func(t *testing.T) {
			d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: fmt.Sprintf("rec%d", entries), Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries},
				Config{DeferLateFolders: true})
			r5Write(t, d, "/dir/a.txt", rvMonth(2011, 1, 1))
			r5Write(t, d, "/dir/b.txt", rvMonth(2011, 3, 1))
			mode := uint32(0o700)
			if _, err := d.SetInfo(drive.WithSourceTime(system(), rvMonth(2011, 6, 1)), drive.ByPath("/dir"), drive.ItemInfo{Mode: &mode, App: map[string][]byte{"rv.k": []byte("june")}}); err != nil {
				t.Fatal(err)
			}
			r5Write(t, d, "/dir/c.txt", rvMonth(2011, 9, 1))
			stat := func(at time.Time) string {
				it, err := d.Stat(system(), drive.ByPath("/dir"), drive.At{Time: at})
				if err != nil {
					return errShort(err)
				}
				return fmt.Sprintf("mode %o app %q", it.Mode&0o7777, it.App["rv.k"])
			}
			want := map[time.Time]string{
				rvMonth(2011, 2, 1):  "mode 755 app \"\"",
				rvMonth(2011, 4, 1):  "mode 755 app \"\"",
				rvMonth(2011, 7, 1):  "mode 700 app \"june\"",
				rvMonth(2011, 10, 1): "mode 700 app \"june\"",
			}
			check := func(when string) {
				for at, w := range want {
					if got := stat(at); got != w {
						t.Errorf("%s: /dir's record as of %s: %s, want %s", when, at.Format("2006-01-02"), got, w)
					}
				}
			}
			check("before the settle")
			rows := r5Write(t, d, "/dir/x.txt", rvMonth(2011, 2, 15))
			if _, err := settleAt(d, system(), rows, (&keeper{}).opts()); err != nil {
				t.Fatal(err)
			}
			check("after the settle")
			if got := rvNamesAt(t, d, "/dir", rvMonth(2011, 4, 1)); got != "a.txt,b.txt,x.txt" {
				t.Errorf("/dir as of 2011-04-01: %s", got)
			}
			// A second late write: its Until is the folder's newest state.
			rows = r5Write(t, d, "/dir/y.txt", rvMonth(2011, 5, 1))
			if len(rows) != 1 || !rows[0].Until.Equal(rvMonth(2011, 9, 1)) {
				t.Errorf("the second late write's rows: %+v, want until 2011-09-01", rows)
			}
			if _, err := settleAt(d, system(), rows, (&keeper{}).opts()); err != nil {
				t.Fatal(err)
			}
			check("after a second settle")
		})
	}
}

// TestUndatedThenLate (R5, the reviewer's TestRV4UndatedThenLate as
// assertions): an undated write into /dir (f0..f7 dated monthly in 2011),
// then a late write dated 2011-03-15, in one block and in blocks of 4, the
// undated write into a leaf with room or a full one, the late one into
// another leaf, the same leaf, or splitting: on MinIO, whose listing gives
// milliseconds where every write's answer gives seconds. No view holds a
// name twice; as of now both files show; the settle is not refused; after
// it the late file is in the past from its date, the undated one from its
// second on.
func TestUndatedThenLate(t *testing.T) {
	bucket := newBuckets(t)
	for ci, tc := range []struct {
		name          string
		entries       uint32
		undated, late string
		room          bool
	}{
		{"one block", 0, "/dir/f3u.txt", "/dir/a-late.txt", false},
		{"blocks of 4, other leaf", 4, "/dir/f6u.txt", "/dir/a-late.txt", false},
		{"blocks of 4, same leaf", 4, "/dir/f6u.txt", "/dir/f6v.txt", false},
		{"blocks of 4, late splits", 4, "/dir/f6u.txt", "/dir/f0a.txt", false},
		{"room: blocks of 4, other leaf", 4, "/dir/f6u.txt", "/dir/a-late.txt", true},
		{"room: blocks of 4, same leaf", 4, "/dir/f6u.txt", "/dir/f6v.txt", true},
		{"room: blocks of 4, late splits", 4, "/dir/f6u.txt", "/dir/f0a.txt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDriveWith(t, bucket(t), icbfs.Settings{Name: fmt.Sprintf("und%d", ci), Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: tc.entries},
				Config{DeferLateFolders: true})
			for i := 0; i < 8; i++ {
				r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", i), rvMonth(2011, 1+i, 1))
			}
			if tc.room {
				for i, p := range []string{"/dir/f5.txt", "/dir/f7.txt", "/dir/f1.txt", "/dir/f3.txt"} {
					if err := d.Delete(drive.WithSourceTime(system(), rvMonth(2011, 9+i, 1)), drive.ByPath(p), false, drive.Condition{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			stored := time.Now()
			r5Write(t, d, tc.undated, time.Time{})
			rows := r5Write(t, d, tc.late, rvMonth(2011, 3, 15))
			u, l := strings.TrimPrefix(tc.undated, "/dir/"), strings.TrimPrefix(tc.late, "/dir/")
			for _, at := range []time.Time{stored.Add(-2 * time.Second), stored, stored.Add(500 * time.Millisecond), stored.Add(time.Second), stored.Add(2 * time.Second), time.Now().Add(time.Second)} {
				got := rvNamesAt(t, d, "/dir", at)
				if tw := r5Twice(got); tw != "" {
					t.Errorf("before the settle, as of %s: %q has %s twice", at.Format(time.RFC3339Nano), got, tw)
				}
				if strings.Contains(got, l) && !strings.Contains(got, u) {
					t.Errorf("before the settle, as of %s: the late file without the undated one it was written after: %q", at.Format(time.RFC3339Nano), got)
				}
			}
			now := rvNamesAt(t, d, "/dir", time.Now().Add(time.Second))
			if !strings.Contains(now, u) || !strings.Contains(now, l) {
				t.Errorf("as of now: %q, want both %s and %s; rows %+v", now, u, l, rows)
				r5Dump(t, d)
			}
			if _, err := settleAt(d, system(), rows, (&keeper{}).opts()); err != nil {
				t.Fatalf("the settle: %v", err)
			}
			for _, at := range []time.Time{rvMonth(2011, 4, 1), rvMonth(2012, 1, 1), time.Now().Add(time.Second)} {
				got := rvNamesAt(t, d, "/dir", at)
				if tw := r5Twice(got); tw != "" {
					t.Errorf("after the settle, as of %s: %q has %s twice", at.Format(time.RFC3339Nano), got, tw)
				}
				if !strings.Contains(got, l) {
					t.Errorf("after the settle, as of %s: %q without the late file", at.Format(time.RFC3339Nano), got)
				}
				if at.Before(stored.Add(-2*time.Second)) == strings.Contains(got, u) {
					t.Errorf("after the settle, as of %s: %q, the undated file (stored %s) wrong", at.Format(time.RFC3339Nano), got, stored.Format(time.RFC3339))
				}
			}
			if got := rvNamesAt(t, d, "/dir", time.Time{}); !strings.Contains(got, u) || !strings.Contains(got, l) {
				t.Errorf("the present: %q", got)
			}
		})
	}
}

// TestRerunStopsOnAWriteSinceTheCut (D3): a settle cut in its window (its
// third window put, before its commit; and after its commit, before its
// erases), then a late write into the folder before the rerun (a caller
// that breaks the order): the rerun with the record stops
// (ErrSettleStopped), erases nothing (the late write stays in the
// present), and holds the record.
func TestRerunStopsOnAWriteSinceTheCut(t *testing.T) {
	for _, c := range []struct {
		name string
		cut  func(folder string) func(op, key string) bool
	}{
		{"in the window", func(folder string) func(op, key string) bool {
			puts := 0
			return func(op, key string) bool {
				if op == "PUT" && (key == folder || key == folder+".metadata") {
					puts++
				}
				return puts >= 3
			}
		}},
		{"after the commit", func(folder string) func(op, key string) bool {
			committed := false
			return func(op, key string) bool {
				if committed && op == "ERASE" {
					return true
				}
				if op == "PUT" && key == folder+".version" {
					committed = true
				}
				return false
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &failStore{Store: miniotest.Store(t)}
			d, changes := cutCase(t, store, "since"+strings.ReplaceAll(c.name, " ", ""), Config{DeferLateFolders: true})
			folder := changes[0].Folder
			cutAt := c.cut(folder)
			listed, failing := false, false
			store.mu.Lock()
			store.watch = func(op, key string) error {
				if op == "HISTORY" && key == folder {
					listed = true
				}
				if listed && (failing || cutAt(op, key)) {
					failing = true
					return errFail
				}
				return nil
			}
			store.mu.Unlock()
			k := &keeper{}
			if _, err := settleAt(d, system(), changes, k.opts()); err == nil {
				t.Fatalf("the settle was not cut")
			}
			store.mu.Lock()
			store.watch = nil
			store.mu.Unlock()
			if k.held() == nil {
				t.Fatalf("no record held")
			}
			d2 := newDriveWith(t, store, cutCaseSettings("since"+strings.ReplaceAll(c.name, " ", "")), Config{DeferLateFolders: true})
			ys := r5Write(t, d2, "/dir/y.txt", year(2023))
			_, err := settleAt(d2, system(), append(changes, ys...), k.opts())
			if !errors.Is(err, drive.ErrSettleStopped) {
				t.Fatalf("the rerun after a write since the cut: %v, want ErrSettleStopped", err)
			}
			if k.held() == nil {
				t.Errorf("the stopped rerun released the record")
			}
			if got := liveNames(t, d2, folder); !strings.Contains(got, "y.txt") {
				t.Errorf("the present lost the write since the cut: %s", got)
			}
		})
	}
}

// TestSettleHeapBounded (R2): one SettleFolder of 2,000 late changes into
// a folder of 300 entries, and of 300 into one of 2,100 (production's
// largest), each peaks under 64 MiB of heap over the heap before it.
// Long: run when T83_HEAP is set (the lab).
func TestSettleHeapBounded(t *testing.T) {
	if !envSet("T83_HEAP") {
		t.Skip("T83_HEAP unset: the heap gate runs on the lab")
	}
	bucket := newBuckets(t)
	for _, c := range []struct{ entries, late int }{{300, 2000}, {2100, 300}} {
		t.Run(fmt.Sprintf("%d entries, %d late", c.entries, c.late), func(t *testing.T) {
			st := newRVStore(bucket(t))
			d := newDriveWith(t, st, icbfs.Settings{Name: fmt.Sprintf("heap%d", c.entries), Size: 1 << 32, RootMode: 0o777, Created: year(2010)}, Config{DeferLateFolders: true})
			at := year(2011)
			for i := 0; i < c.entries; i++ {
				at = at.Add(time.Hour)
				r5Write(t, d, fmt.Sprintf("/dir/f%05d.txt", i), at)
			}
			behind := at.Add(-3 * time.Hour)
			var rows []drive.LateChange
			for i := 0; i < c.late; i++ {
				rows = append(rows, r5Write(t, d, fmt.Sprintf("/dir/L%05d.txt", i), behind.Add(time.Duration(i)*time.Second))...)
			}
			st.take()
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			base := ms.HeapInuse
			var mu sync.Mutex
			peak := base
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					select {
					case <-stop:
						return
					case <-time.After(20 * time.Millisecond):
					}
					var ms runtime.MemStats
					runtime.ReadMemStats(&ms)
					mu.Lock()
					peak = max(peak, ms.HeapInuse)
					mu.Unlock()
				}
			}()
			start := time.Now()
			s, err := settleAt(d, system(), rows, (&keeper{}).opts())
			close(stop)
			<-done
			reqs, _, _, _ := st.take()
			mu.Lock()
			over := float64(peak-base) / (1 << 20)
			mu.Unlock()
			t.Logf("HEAP %d entries, %d late: %+v, %d requests, %.1f s, peak heap over before %.1f MiB", c.entries, c.late, s, reqs, time.Since(start).Seconds(), over)
			if err != nil {
				t.Fatal(err)
			}
			if over >= 64 {
				t.Errorf("peak heap over before %.1f MiB, want under 64", over)
			}
			if got := len(strings.Split(rvNamesAt(t, d, "/dir", time.Time{}), ",")); got != c.entries+c.late {
				t.Errorf("the present: %d names, want %d", got, c.entries+c.late)
			}
		})
	}
}

// r5Dump logs every version of the store's objects with its date: for a
// failure's message.
func r5Dump(t *testing.T, d *Drive) {
	ctx := context.Background()
	all, err := d.cfg.Store.HistoryUnder(ctx, "")
	if err != nil {
		t.Logf("dump: %v", err)
		return
	}
	dates := map[string]objstore.Dates{}
	for _, o := range all {
		if strings.HasSuffix(o.Key, objstore.VersionSuffix) || strings.HasSuffix(o.Key, "-past") {
			continue
		}
		base := objstore.BaseKey(o.Key)
		if dates[base] == nil {
			dates[base], _ = objstore.ReadDates(ctx, d.cfg.Store, base)
		}
		dt, listed := dates[base][o.VersionID]
		var at time.Time
		if !o.Deleted {
			if h, err := d.cfg.Store.HeadVersion(ctx, o.Key, o.VersionID); err == nil {
				at, _ = objstore.PutAt(h)
			}
		}
		t.Logf("dump %s %s stored %s listed %v %s putat %s del %v", o.Key, o.VersionID[:8], o.LastModified.Format(time.RFC3339Nano), listed, dt.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), o.Deleted)
	}
}
