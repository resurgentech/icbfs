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

// failStore, once armed, counts the requests that reach the store (the
// past-changed mark's and the clock's aside, which a drive makes on its
// own time) and fails the cut-th and every one after it: a settle or a
// write cut at that request, and the store away until disarmed. once
// fails only the cut-th. seq, while armed, is every request in order.
type failStore struct {
	objstore.Store
	mu    sync.Mutex
	armed bool
	once  bool
	cut   int
	n     int
	seq   []string
	// watch, if set, sees every request (armed or not), under mu, and
	// may fail it.
	watch func(op, key string) error
}

var errFail = errors.New("the store is away")

func (s *failStore) arm(cut int, once bool) {
	s.mu.Lock()
	s.armed, s.cut, s.once, s.n, s.seq = true, cut, once, 0, nil
	s.mu.Unlock()
}

// disarm stops counting, and returns how many requests were made armed
// and what they were.
func (s *failStore) disarm() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = false
	return s.n, s.seq
}

func (s *failStore) hit(op, key string) error {
	if strings.HasSuffix(key, "-past") {
		return nil
	}
	s.mu.Lock()
	if s.watch != nil {
		if err := s.watch(op, key); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if !s.armed {
		s.mu.Unlock()
		return nil
	}
	s.n++
	s.seq = append(s.seq, op+" "+key)
	fail := s.n == s.cut || !s.once && s.cut > 0 && s.n > s.cut
	n := s.n
	s.mu.Unlock()
	if !fail {
		return nil
	}
	return fmt.Errorf("request %d (%s %s): %w", n, op, key, errFail)
}

func (s *failStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if err := s.hit("PUT", key); err != nil {
		return nil, err
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func (s *failStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	if err := s.hit("PUT-absent", key); err != nil {
		return nil, err
	}
	return s.Store.PutIfAbsent(ctx, key, body, md)
}

func (s *failStore) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if err := s.hit("PUT-parts", key); err != nil {
		return nil, err
	}
	return s.Store.PutParts(ctx, key, parts, md, ifMatch)
}

func (s *failStore) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	if err := s.hit("GET", key); err != nil {
		return nil, nil, err
	}
	return s.Store.Get(ctx, key)
}

func (s *failStore) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	if err := s.hit("GET-if-changed", key); err != nil {
		return nil, nil, false, err
	}
	return s.Store.GetIfChanged(ctx, key, etag)
}

func (s *failStore) Head(ctx context.Context, key string) (*objstore.Object, error) {
	if err := s.hit("HEAD", key); err != nil {
		return nil, err
	}
	return s.Store.Head(ctx, key)
}

func (s *failStore) GetVersion(ctx context.Context, key, v string) (io.ReadCloser, *objstore.Object, error) {
	if err := s.hit("GET-version", key); err != nil {
		return nil, nil, err
	}
	return s.Store.GetVersion(ctx, key, v)
}

func (s *failStore) HeadVersion(ctx context.Context, key, v string) (*objstore.Object, error) {
	if err := s.hit("HEAD-version", key); err != nil {
		return nil, err
	}
	return s.Store.HeadVersion(ctx, key, v)
}

func (s *failStore) Delete(ctx context.Context, key string) error {
	if err := s.hit("DELETE", key); err != nil {
		return err
	}
	return s.Store.Delete(ctx, key)
}

func (s *failStore) Undelete(ctx context.Context, key string) error {
	if err := s.hit("UNDELETE", key); err != nil {
		return err
	}
	return s.Store.Undelete(ctx, key)
}

func (s *failStore) History(ctx context.Context, key string) ([]objstore.Object, error) {
	if err := s.hit("HISTORY", key); err != nil {
		return nil, err
	}
	return s.Store.History(ctx, key)
}

func (s *failStore) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	if err := s.hit("HISTORY-under", prefix); err != nil {
		return nil, err
	}
	return s.Store.HistoryUnder(ctx, prefix)
}

func (s *failStore) Erase(ctx context.Context, key, v string) error {
	if err := s.hit("ERASE", key); err != nil {
		return err
	}
	return s.Store.Erase(ctx, key, v)
}

func (s *failStore) EraseAll(ctx context.Context, prefix string) error {
	if err := s.hit("ERASE-all", prefix); err != nil {
		return err
	}
	return s.Store.EraseAll(ctx, prefix)
}

func (s *failStore) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	if err := s.hit("LIST", prefix); err != nil {
		return nil, err
	}
	return s.Store.List(ctx, prefix)
}

// sortedAt is dir's entry names as of when (now if zero), sorted.
func sortedAt(t *testing.T, d *Drive, dir string, when time.Time) string {
	t.Helper()
	got := strings.Split(names(t, d, dir, when), ",")
	sort.Strings(got)
	return strings.Join(got, ",")
}

// liveNames is folder's entry names now, read live, sorted.
func liveNames(t *testing.T, d *Drive, folder string) string {
	t.Helper()
	entries, err := d.fs.ReadDir(system(), folder)
	if err != nil {
		t.Fatalf("the folder now: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// pastIs checks /dir's past, year by year (mid-year).
func pastIs(t *testing.T, d *Drive, want map[int]string) {
	t.Helper()
	years := make([]int, 0, len(want))
	for y := range want {
		years = append(years, y)
	}
	sort.Ints(years)
	for _, y := range years {
		if got := sortedAt(t, d, "/dir", midYear(y)); got != want[y] {
			t.Errorf("/dir as of mid-%d: %s, want %s", y, got, want[y])
		}
	}
}

// settleLeftovers checks what a settle left in the store: no version of the
// folder's root block or .metadata its .version doesn't list (a cut
// write-out's, or one superseded and not erased), and, by fs.Check,
// nothing current that no name leads to but the .version records of
// blocks only the past reads (as ReplaceDirHistory leaves them), at most
// records of them.
func settleLeftovers(t *testing.T, d *Drive, folder string, records int) {
	t.Helper()
	ctx := context.Background()
	store := d.cfg.Store
	dates, err := objstore.ReadDates(ctx, store, folder)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{folder, folder + ".metadata"} {
		h, err := store.History(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range h {
			if _, ok := dates[o.VersionID]; !ok && !o.Deleted {
				t.Errorf("%s: version %s (stored %s) isn't in the folder's .version: left behind", key, o.VersionID, o.LastModified.Format(time.RFC3339Nano))
			}
		}
	}
	rep, err := d.fs.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, k := range rep.UnreachableListed {
		if !strings.HasSuffix(k, objstore.VersionSuffix) {
			t.Errorf("unreachable and current: %s", k)
			continue
		}
		n++
		h, err := store.History(ctx, strings.TrimSuffix(k, objstore.VersionSuffix))
		if err != nil {
			t.Fatal(err)
		}
		if len(h) == 0 || !h[0].Deleted {
			t.Errorf("unreachable record %s: its block isn't one only the past reads (%d versions)", k, len(h))
		}
	}
	if rep.Unreachable > len(rep.UnreachableListed) {
		t.Errorf("%d unreachable, %d listed", rep.Unreachable, len(rep.UnreachableListed))
	}
	if n > records {
		t.Errorf("%d unreachable .version records, want at most %d: %v", n, records, rep.UnreachableListed)
	}
}

// cutCase is the review's folder: a@2020, b@2022, c@2024, and x written
// at 2021, deferred.
func cutCase(t *testing.T, store objstore.Store, name string, cfg Config) (*Drive, []drive.LateChange) {
	t.Helper()
	d := newDriveWith(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, cfg)
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
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	return d, late.Take()
}

// The folder after the settle, then an on-time write of d in 2025.
var cutCaseAfter = map[int]string{
	2020: "a.txt",
	2021: "a.txt,x.txt",
	2022: "a.txt,b.txt,x.txt",
	2023: "a.txt,b.txt,x.txt",
	2024: "a.txt,b.txt,c.txt,x.txt",
	2025: "a.txt,b.txt,c.txt,d.txt,x.txt",
}

// TestSettleCutAtEveryRequest (review finding 1, and 8's cuts before the
// window and in the erases): a settle given only a LateTouch at the late
// write's time (what a crash's recovery queues: it names no entry) is
// cut at each of its requests in turn (that request and every one after
// fail). The caller settles again with the same touch and the cut
// settle's plan, then writes into the folder on time. Whatever the cut,
// the folder ends with the present it had before (x kept), x in the past
// from 2021, the on-time write in place, no root or .metadata version
// left unlisted, nothing current unreachable, and what the cut settle
// created that nothing reaches gone.
func TestSettleCutAtEveryRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("one folder per request of a settle")
	}
	store := &failStore{Store: miniotest.Store(t)}
	cfg := Config{DeferLateFolders: true}

	// The settle uncut: how many requests it makes, and what it leaves.
	d, changes := cutCase(t, store, "cutall0", cfg)
	folder := changes[0].Folder
	touch := []drive.LateChange{{Folder: folder, At: changes[0].At, Op: drive.LateTouch}}
	store.arm(0, false)
	if _, err := d.SettleFolder(system(), touch, drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	total, seq := store.disarm()
	rep, err := d.fs.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	records := rep.Unreachable
	t.Logf("an uncut settle: %d requests, leaving %d .version records unreachable: %s", total, records, summarize(seq))

	for cut := 1; cut <= total+1; cut++ {
		t.Run(fmt.Sprintf("request %d of %d fails", cut, total), func(t *testing.T) {
			d, changes := cutCase(t, store, fmt.Sprintf("cutall%d", cut), cfg)
			folder := changes[0].Folder
			touch := []drive.LateChange{{Folder: folder, At: changes[0].At, Op: drive.LateTouch}}
			ctx := system()
			before := liveNames(t, d, folder)
			var plan *drive.SettlePlan
			store.arm(cut, false)
			_, err := d.SettleFolder(ctx, touch, drive.SettleOptions{Plan: func(p drive.SettlePlan) error { plan = &p; return nil }})
			_, seq := store.disarm()
			// (A cut while the folder is looked up reads as not found: the
			// drive's search goes on past a folder it can't read.)
			if cut <= total && err == nil {
				t.Fatalf("cut at request %d (%s), and no error", cut, seq[len(seq)-1])
			}
			if cut > total && err != nil {
				t.Fatalf("not cut: %v", err)
			}
			// The caller settles again, with the cut's plan if it had one.
			if _, err := d.SettleFolder(ctx, touch, drive.SettleOptions{Cut: plan}); err != nil {
				t.Fatalf("settled again: %v", err)
			}
			if got := liveNames(t, d, folder); got != before {
				t.Fatalf("the folder now after the cut and again: %s, want %s as before", got, before)
			}
			if _, err := d.Write(at(ctx, 2025), "/dir/d.txt", strings.NewReader("d"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := liveNames(t, d, folder); got != cutCaseAfter[2025] {
				t.Errorf("the folder now after an on-time write: %s, want %s", got, cutCaseAfter[2025])
			}
			pastIs(t, d, cutCaseAfter)
			settleLeftovers(t, d, folder, records)
			if plan != nil {
				for _, k := range plan.Create {
					h, err := store.Store.History(context.Background(), k)
					if err != nil {
						t.Fatal(err)
					}
					if len(h) > 0 && !h[0].Deleted {
						if rep, _ := d.fs.Check(context.Background()); slicesContain(rep.UnreachableListed, k) {
							t.Errorf("the cut settle's new block %s is left, unreachable", k)
						}
					}
				}
			}
		})
	}
}

func slicesContain(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestLateWriteCutThenOnTimeWrite (review finding 2): on a drive that
// doesn't defer, a late write's own settle is cut at each of its
// requests in turn; the write fails. The next write into the folder, on
// time, lands on the present the folder had before the cut (b and c not
// lost), the settle finished first; the past has x from 2021.
func TestLateWriteCutThenOnTimeWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("one folder per request of a settle")
	}
	store := &failStore{Store: miniotest.Store(t)}
	setup := func(t *testing.T, name string) (*Drive, string) {
		d := newDriveWith(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{})
		ctx := system()
		for _, w := range []struct {
			y    int
			path string
		}{{2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}, {2024, "/dir/c.txt"}} {
			if _, err := d.Write(at(ctx, w.y), w.path, strings.NewReader(w.path), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
				t.Fatal(err)
			}
		}
		dir, err := d.Stat(ctx, drive.ByPath("/dir"), drive.At{})
		if err != nil {
			t.Fatal(err)
		}
		return d, dir.ID
	}
	// The late write uncut: where its settle starts (its first listing of
	// the folder's root block), and how many requests it makes.
	d, folder := setup(t, "cutwrite0")
	store.arm(0, false)
	if _, err := d.Write(at(system(), 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	total, seq := store.disarm()
	start := -1
	for i, r := range seq {
		if r == "HISTORY "+folder {
			start = i + 1 // 1-based
			break
		}
	}
	if start < 0 {
		t.Fatalf("the late write's settle never listed the folder: %v", seq)
	}
	rep, err := d.fs.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	records := rep.Unreachable
	t.Logf("a late write: %d requests, its settle from request %d", total, start)

	for cut := start; cut <= total; cut++ {
		t.Run(fmt.Sprintf("request %d of %d fails", cut, total), func(t *testing.T) {
			d, folder := setup(t, fmt.Sprintf("cutwrite%d", cut))
			ctx := system()
			store.arm(cut, false)
			_, err := d.Write(at(ctx, 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{})
			store.disarm()
			if !errors.Is(err, errFail) {
				t.Fatalf("a late write cut in its settle: %v", err)
			}
			if _, err := d.Write(at(ctx, 2025), "/dir/d.txt", strings.NewReader("d"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatalf("the next write: %v", err)
			}
			if got := liveNames(t, d, folder); got != cutCaseAfter[2025] {
				t.Errorf("the folder now: %s, want %s", got, cutCaseAfter[2025])
			}
			pastIs(t, d, cutCaseAfter)
			settleLeftovers(t, d, folder, records)
		})
	}
}

// TestSettleOutlivesItsCaller (review finding 2): a late write whose
// caller goes away inside its settle's window (its context canceled at
// the window's first request), and one whose store fails the window's
// first request once, both end with the folder settled: the present as
// it was, the past with the change, the write done.
func TestSettleOutlivesItsCaller(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	for _, c := range []struct {
		name   string
		cancel bool
	}{{"outlive", true}, {"retried", false}} {
		t.Run(c.name, func(t *testing.T) {
			d := newDriveWith(t, store, icbfs.Settings{Name: c.name, Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{})
			sys := system()
			for _, w := range []struct {
				y    int
				path string
			}{{2020, "/dir/a.txt"}, {2022, "/dir/b.txt"}, {2024, "/dir/c.txt"}} {
				if _, err := d.Write(at(sys, w.y), w.path, strings.NewReader(w.path), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
					t.Fatal(err)
				}
			}
			dir, err := d.Stat(sys, drive.ByPath("/dir"), drive.At{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(sys)
			defer cancel()
			// The window's first request: the settle's first write of the
			// folder's .metadata, after its listing of the folder.
			listed, hit := false, false
			store.mu.Lock()
			store.watch = func(op, key string) error {
				switch {
				case op == "HISTORY" && key == dir.ID:
					listed = true
				case listed && !hit && op == "PUT" && key == dir.ID+".metadata":
					hit = true
					if c.cancel {
						cancel()
						return nil
					}
					return errFail
				}
				return nil
			}
			store.mu.Unlock()
			_, err = d.Write(at(ctx, 2021), "/dir/x.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{})
			store.mu.Lock()
			store.watch = nil
			store.mu.Unlock()
			if !hit {
				t.Fatalf("the settle's window never came")
			}
			if err != nil {
				t.Fatalf("the late write: %v", err)
			}
			if got := liveNames(t, d, dir.ID); got != "a.txt,b.txt,c.txt,x.txt" {
				t.Errorf("the folder now: %s", got)
			}
			pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
			settleLeftovers(t, d, dir.ID, 0)
		})
	}
}
