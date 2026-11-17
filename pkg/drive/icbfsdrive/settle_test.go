package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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

var _ drive.LateSettler = (*Drive)(nil)

// writesSince is how many writes counter saw since its last take, past
// marks aside (at most one a second, whatever the operations).
func writesSince(c *requestCounter) (int, string) {
	c.mu.Lock()
	n := 0
	for k, v := range c.n {
		op, class, _ := strings.Cut(k, " ")
		if class == "past" {
			continue
		}
		if strings.HasPrefix(op, "PUT") || strings.HasPrefix(op, "ERASE") || op == "DELETE" || op == "UNDELETE" {
			n += v
		}
	}
	c.mu.Unlock()
	_, all := c.take()
	return n, all
}

func midYear(y int) time.Time { return year(y).AddDate(0, 6, 0) }

// TestDeferredLateFolder: on a drive that defers late folders, late
// writes into a folder report their changes to the context's
// LateChanges (folder, time, order, kind, name, item, type) and leave
// the folder's past as it was; SettleFolder then puts each change into
// the past at its time and after, the present the same throughout; the
// same changes settled again write nothing. A late write with no
// LateChanges in its context settles at once.
func TestDeferredLateFolder(t *testing.T) {
	counter := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d := newDriveWith(t, counter, icbfs.Settings{Name: "deferred", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	ctx := system()
	ids := map[string]string{}
	for _, w := range []struct {
		y    int
		path string
	}{{2019, "/other/keep.txt"}, {2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}, {2024, "/dir/c.txt"}} {
		it, err := d.Write(at(ctx, w.y), w.path, strings.NewReader(w.path), drive.Condition{}, drive.WriteOptions{Parents: true})
		if err != nil {
			t.Fatal(err)
		}
		ids[w.path] = it.ID
	}
	dir, err := d.Stat(ctx, drive.ByPath("/dir"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}

	late := &drive.LateChanges{}
	lctx := drive.WithLateChanges(ctx, late)
	x, err := d.Write(at(lctx, 2021), "/dir/x.txt", strings.NewReader("late"), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(at(lctx, 2023), drive.ByPath("/dir/b.txt"), "/other/b.txt", false); err != nil {
		t.Fatal(err)
	}
	changes := late.Take()
	want := []drive.LateChange{
		{Folder: dir.ID, At: year(2021), Seq: 0, Op: drive.LateAdd, Name: "x.txt", Item: x.ID, Type: drive.TypeFile},
		{Folder: dir.ID, At: year(2023), Seq: 0, Op: drive.LateRemove, Name: "b.txt", Item: ids["/dir/b.txt"], Type: drive.TypeFile},
	}
	if len(changes) != len(want) {
		t.Fatalf("late changes %+v, want %+v", changes, want)
	}
	for i := range want {
		if c := changes[i]; c.Folder != want[i].Folder || !c.At.Equal(want[i].At) || c.Seq != want[i].Seq || c.Op != want[i].Op ||
			c.Name != want[i].Name || c.Item != want[i].Item || c.Type != want[i].Type {
			t.Errorf("late change %d: %+v, want %+v", i, c, want[i])
		}
	}
	if more := late.Take(); len(more) != 0 {
		t.Fatalf("Take didn't empty it: %+v", more)
	}

	present := "a.txt,c.txt,x.txt"
	listed := func(when time.Time) string {
		got := strings.Split(names(t, d, "/dir", when), ",")
		sort.Strings(got)
		return strings.Join(got, ",")
	}
	// The folder as it is now, read live.
	live := func() string {
		t.Helper()
		entries, err := d.fs.ReadDir(ctx, dir.ID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name)
		}
		sort.Strings(got)
		return strings.Join(got, ",")
	}
	// Deferred: the past is as it was (a view after the folder's later
	// states doesn't show the changes), the present has them.
	if got := listed(midYear(2024)); got != "a.txt,b.txt,c.txt" {
		t.Fatalf("/dir as of mid-2024 before the settle: %s, want the past as it was", got)
	}
	if got := live(); got != present {
		t.Fatalf("/dir now before the settle: %s, want %s", got, present)
	}

	settled, err := d.SettleFolder(ctx, changes, drive.SettleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if settled.Written == 0 {
		t.Fatalf("settled %+v: nothing written", settled)
	}
	for _, c := range []struct {
		when time.Time
		want string
	}{
		{midYear(2020), "a.txt"},
		{midYear(2021), "a.txt,x.txt"},
		{midYear(2022), "a.txt,b.txt,x.txt"},
		{midYear(2023), "a.txt,x.txt"},
		{midYear(2024), present},
		{time.Now(), present},
	} {
		if got := listed(c.when); got != c.want {
			t.Errorf("/dir as of %v after the settle: %s, want %s", c.when.Format("2006-01"), got, c.want)
		}
	}
	if got := live(); got != present {
		t.Errorf("/dir now after the settle: %s, want %s", got, present)
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/x.txt"), drive.At{Time: midYear(2021)}); got != "late" {
		t.Errorf("/dir/x.txt as of mid-2021: %q", got)
	}

	// The same changes again: nothing to write.
	writesSince(counter)
	again, err := d.SettleFolder(ctx, changes, drive.SettleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n, all := writesSince(counter); n != 0 || again.Written != 0 || again.Erased != 0 {
		t.Fatalf("settled again: %+v and %d writes, want none:%s", again, n, all)
	}
	if got := listed(midYear(2022)); got != "a.txt,b.txt,x.txt" {
		t.Errorf("/dir as of mid-2022 after settling again: %s", got)
	}

	// Changes of two folders in one call are refused.
	two := append([]drive.LateChange{{Folder: ids["/other/keep.txt"], At: year(2021), Op: drive.LateTouch}}, changes...)
	if _, err := d.SettleFolder(ctx, two, drive.SettleOptions{}); !errors.Is(err, drive.ErrInvalid) {
		t.Errorf("changes of two folders: %v, want ErrInvalid", err)
	}
	if s, err := d.SettleFolder(ctx, nil, drive.SettleOptions{}); err != nil || s != (drive.Settled{}) {
		t.Errorf("no changes: %+v %v", s, err)
	}

	// Deferring, but no LateChanges to report to: settled at once.
	if _, err := d.Write(at(ctx, 2021), "/dir/y.txt", strings.NewReader("y"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := listed(midYear(2022)); got != "a.txt,b.txt,x.txt,y.txt" {
		t.Errorf("/dir as of mid-2022 after a late write with nowhere to report it: %s, want y.txt in it at once", got)
	}
}

// TestSettlePlan: a settle tells its Plan what it will create and erase
// before it writes, and an error from the Plan stops it with nothing
// written.
func TestSettlePlan(t *testing.T) {
	counter := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d := newDriveWith(t, counter, icbfs.Settings{Name: "settleplan", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	ctx := system()
	for _, w := range []struct {
		y    int
		path string
	}{{2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}} {
		if _, err := d.Write(at(ctx, w.y), w.path, strings.NewReader(w.path), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	changes := late.Take()
	writesSince(counter)
	stop := errStopPlan
	var seen drive.SettlePlan
	_, err := d.SettleFolder(ctx, changes, drive.SettleOptions{Plan: func(p drive.SettlePlan) error { seen = p; return stop }})
	if err == nil || !strings.Contains(err.Error(), stop.Error()) {
		t.Fatalf("a plan refused: %v", err)
	}
	if n, all := writesSince(counter); n != 0 {
		t.Fatalf("a plan refused, and %d writes:%s", n, all)
	}
	if seen.Folder != changes[0].Folder || seen.States != 2 || len(seen.Erase) == 0 || seen.RootBefore == "" || seen.MetaBefore == "" || !seen.From.Equal(year(2020)) {
		t.Fatalf("plan %+v", seen)
	}
	var planned drive.SettlePlan
	s, err := d.SettleFolder(ctx, changes, drive.SettleOptions{Plan: func(p drive.SettlePlan) error { planned = p; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if s.Written != planned.States || s.Erased != len(planned.Erase) {
		t.Fatalf("settled %+v, planned %+v", s, planned)
	}
	if got := names(t, d, "/dir", midYear(2021)); got != "a.txt,x.txt" {
		t.Fatalf("/dir as of mid-2021: %s", got)
	}
}

var errStopPlan = errString("the plan says no")

type errString string

func (e errString) Error() string { return string(e) }

// TestLateWriteSettlesAtOnce: without DeferLateFolders, a late write
// settles its folder inside the write, through the settle (SettleDir),
// and the past is right as soon as it returns.
func TestLateWriteSettlesAtOnce(t *testing.T) {
	var mu sync.Mutex
	var settled []string
	onSettle = func(dir string, s drive.Settled) {
		mu.Lock()
		settled = append(settled, dir)
		mu.Unlock()
	}
	t.Cleanup(func() { onSettle = nil })
	d, _ := sourceDrive(t, "atonce")
	ctx := system()
	for _, w := range []struct {
		y    int
		path string
	}{{2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}} {
		if _, err := d.Write(at(ctx, w.y), w.path, strings.NewReader(w.path), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := d.Stat(ctx, drive.ByPath("/dir"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(drive.WithLateChanges(at(ctx, 2021), &drive.LateChanges{}), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(settled, ",")
	mu.Unlock()
	if got != dir.ID {
		t.Fatalf("settled %q inside the write, want the folder %s once", got, dir.ID)
	}
	if got := names(t, d, "/dir", midYear(2021)); got != "a.txt,x.txt" {
		t.Fatalf("/dir as of mid-2021 right after the write: %s", got)
	}
	if got := names(t, d, "/dir", midYear(2022)); got != "a.txt,b.txt,x.txt" {
		t.Fatalf("/dir as of mid-2022 right after the write: %s", got)
	}
}

// TestDatedWriteKeepsTheHistoryCache: a dated write forgets what the
// history cache kept of the keys it wrote, not the rest: a past already
// read is read again with no request, after the write and after the
// drive's own past-changed mark has gone out and views have looked at it.
func TestDatedWriteKeepsTheHistoryCache(t *testing.T) {
	ctx := context.Background()
	counter := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	cache, err := objstore.OpenHistoryCache(ctx, counter, filepath.Join(t.TempDir(), "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	d := newDriveWith(t, counter, icbfs.Settings{Name: "keepcache", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{History: cache})
	sys := system()
	for _, w := range []struct {
		y          int
		path, body string
	}{{2020, "/a/f.txt", "f1"}, {2021, "/a/f.txt", "f2"}, {2020, "/b/g.txt", "g1"}} {
		if _, err := d.Write(at(sys, w.y), w.path, strings.NewReader(w.body), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	// The past through views alone: the folder's listing and the
	// file's attributes as of mid-2020 (a file's bytes are never cached).
	look := func() {
		t.Helper()
		fsys, err := d.at(sys, drive.At{Time: midYear(2020)})
		if err != nil {
			t.Fatal(err)
		}
		n, err := d.resolve(sys, fsys, drive.ByPath("/a/f.txt"))
		if err != nil {
			t.Fatalf("/a/f.txt as of mid-2020: %v", err)
		}
		entries, err := fsys.ReadDir(sys, n.parentKey())
		if err != nil || len(entries) != 1 || entries[0].Name != "f.txt" {
			t.Fatalf("/a as of mid-2020: %v %v", entries, err)
		}
		if n.attr.Size != 2 {
			t.Fatalf("/a/f.txt as of mid-2020: size %d", n.attr.Size)
		}
	}
	look()
	counter.take()
	look()
	if n, all := counter.take(); n != 0 {
		t.Fatalf("the past read twice: %d requests the second time:%s", n, all)
	}
	// An ordinary dated write elsewhere: its folder's newest state.
	if _, err := d.Write(at(sys, 2022), "/b/g.txt", strings.NewReader("g2"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	counter.take()
	look()
	if n, all := counter.take(); n != 0 {
		t.Errorf("the past read again after a dated write elsewhere: %d requests, want none:%s", n, all)
	}
	// The drive's own mark sent, and views due to look for one.
	time.Sleep(pastMarkEvery + 100*time.Millisecond)
	d.past.flush()
	saved := pastCheck
	pastCheck = 0
	t.Cleanup(func() { pastCheck = saved })
	look()
	if n, all := counter.take(); n != 0 {
		t.Errorf("the past read again after the drive's own mark: %d requests, want none (the mark's HEAD isn't counted):%s", n, all)
	}
}

// cutStore fails the cut-th write to the folder's root block or its
// .metadata once armed, and every one after it: a write-out cut there.
type cutStore struct {
	objstore.Store
	mu     sync.Mutex
	folder string
	cut, n int
}

var errCut = errString("the write-out was cut here")

func (s *cutStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	s.mu.Lock()
	hit := s.folder != "" && (key == s.folder || key == s.folder+".metadata")
	if hit {
		s.n++
	}
	failed := hit && s.n >= s.cut
	s.mu.Unlock()
	if failed {
		return nil, errCut
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

// TestSettleCutThenAgain: a settle cut inside its window (its first,
// second or third request there fails, and every one after) leaves the
// folder's present a state of the write-out at worst, and the same
// changes settled again put everything right: the present as it was,
// each change in the past.
func TestSettleCutThenAgain(t *testing.T) {
	for cut := 1; cut <= 3; cut++ {
		t.Run(fmt.Sprintf("request %d fails", cut), func(t *testing.T) {
			store := &cutStore{Store: miniotest.Store(t)}
			d := newDriveWith(t, store, icbfs.Settings{Name: fmt.Sprintf("cut%d", cut), Size: 1 << 30, RootMode: 0o777, Created: year(2015)},
				Config{DeferLateFolders: true})
			ctx := system()
			for _, w := range []struct {
				y    int
				path string
			}{{2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}, {2024, "/dir/c.txt"}} {
				if _, err := d.Write(at(ctx, w.y), w.path, strings.NewReader(w.path), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
					t.Fatal(err)
				}
			}
			late := &drive.LateChanges{}
			for _, y := range []int{2021, 2023} {
				if _, err := d.Write(at(drive.WithLateChanges(ctx, late), y), fmt.Sprintf("/dir/x%d.txt", y), strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			changes := late.Take()
			store.mu.Lock()
			store.folder, store.cut = changes[0].Folder, cut
			store.mu.Unlock()
			if _, err := d.SettleFolder(ctx, changes, drive.SettleOptions{}); err == nil || !strings.Contains(err.Error(), errCut.Error()) {
				t.Fatalf("a cut settle: %v", err)
			}
			store.mu.Lock()
			store.folder = ""
			store.mu.Unlock()
			if _, err := d.SettleFolder(ctx, changes, drive.SettleOptions{}); err != nil {
				t.Fatalf("settled again: %v", err)
			}
			entries, err := d.fs.ReadDir(ctx, changes[0].Folder)
			if err != nil {
				t.Fatal(err)
			}
			var now []string
			for _, e := range entries {
				now = append(now, e.Name)
			}
			sort.Strings(now)
			if got := strings.Join(now, ","); got != "a.txt,b.txt,c.txt,x2021.txt,x2023.txt" {
				t.Errorf("/dir now: %s", got)
			}
			for _, c := range []struct {
				y    int
				want string
			}{{2020, "a.txt"}, {2021, "a.txt,x2021.txt"}, {2022, "a.txt,b.txt,x2021.txt"}, {2023, "a.txt,b.txt,x2021.txt,x2023.txt"},
				{2024, "a.txt,b.txt,c.txt,x2021.txt,x2023.txt"}} {
				got := strings.Split(names(t, d, "/dir", midYear(c.y)), ",")
				sort.Strings(got)
				if strings.Join(got, ",") != c.want {
					t.Errorf("/dir as of mid-%d: %v, want %s", c.y, got, c.want)
				}
			}
		})
	}
}
