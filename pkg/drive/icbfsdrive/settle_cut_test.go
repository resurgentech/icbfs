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
	lost  bool // the cut-th request is made, and its answer lost
	cut   int
	n     int
	seq   []string
	// watch, if set, sees every request (armed or not), under mu, and
	// may fail it.
	watch func(op, key string) error
}

var errFail = errors.New("the store is away")

// errLost is a request's error when the request was made and its answer
// lost (failStore.lost).
var errLost = fmt.Errorf("the answer was lost: %w", errFail)

func (s *failStore) arm(cut int, once bool) {
	s.mu.Lock()
	s.armed, s.cut, s.once, s.lost, s.n, s.seq = true, cut, once, false, 0, nil
	s.mu.Unlock()
}

// armLost is arm (the store away from the cut-th request on), with the
// cut-th request made and its answer lost.
func (s *failStore) armLost(cut int) {
	s.arm(cut, false)
	s.mu.Lock()
	s.lost = true
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
	lost := s.lost && s.n == s.cut
	n := s.n
	s.mu.Unlock()
	if !fail {
		return nil
	}
	if lost {
		return fmt.Errorf("request %d (%s %s): %w", n, op, key, errLost)
	}
	return fmt.Errorf("request %d (%s %s): %w", n, op, key, errFail)
}

func (s *failStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if err := s.hit("PUT", key); err != nil {
		if errors.Is(err, errLost) {
			s.Store.Put(ctx, key, body, md, ifMatch)
		}
		return nil, err
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func (s *failStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	if err := s.hit("PUT-absent", key); err != nil {
		if errors.Is(err, errLost) {
			s.Store.PutIfAbsent(ctx, key, body, md)
		}
		return nil, err
	}
	return s.Store.PutIfAbsent(ctx, key, body, md)
}

func (s *failStore) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if err := s.hit("PUT-parts", key); err != nil {
		if errors.Is(err, errLost) {
			s.Store.PutParts(ctx, key, parts, md, ifMatch)
		}
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
		if errors.Is(err, errLost) {
			_ = s.Store.Delete(ctx, key)
		}
		return err
	}
	return s.Store.Delete(ctx, key)
}

func (s *failStore) Undelete(ctx context.Context, key string) error {
	if err := s.hit("UNDELETE", key); err != nil {
		if errors.Is(err, errLost) {
			_ = s.Store.Undelete(ctx, key)
		}
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
		if errors.Is(err, errLost) {
			_ = s.Store.Erase(ctx, key, v)
		}
		return err
	}
	return s.Store.Erase(ctx, key, v)
}

func (s *failStore) EraseAll(ctx context.Context, prefix string) error {
	if err := s.hit("ERASE-all", prefix); err != nil {
		if errors.Is(err, errLost) {
			_ = s.Store.EraseAll(ctx, prefix)
		}
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

// settleLeftovers checks what a settle left in the store: of the folder's
// root block and .metadata, no version its .version doesn't list (a cut
// write-out's, or one a lost answer left), none shadowed by a newer one
// dated alike (one superseded and not erased) and none late: nothing
// newer than the present before but the final states. And, by fs.Check,
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
		shadowed, late := objstore.NotStates(h, dates)
		if len(shadowed)+len(late) > 0 {
			t.Errorf("%s: versions shadowed %v and late %v left behind", key, shadowed, late)
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

// keeper keeps a settle's record as its caller does: from Record to Done.
type keeper struct {
	mu   sync.Mutex
	rec  *drive.SettleRecord
	done int
	// fail, if set, is Record's error.
	fail error
}

// opts is SettleOptions handing the record to k, with Cut the record k
// holds (none if it holds none).
func (k *keeper) opts() drive.SettleOptions {
	k.mu.Lock()
	cut := k.rec
	k.mu.Unlock()
	return drive.SettleOptions{
		Cut: cut,
		Record: func(r drive.SettleRecord) error {
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.fail != nil {
				return k.fail
			}
			k.rec = &r
			return nil
		},
		Done: func(drive.SettleRecord) error {
			k.mu.Lock()
			k.rec = nil
			k.done++
			k.mu.Unlock()
			return nil
		},
	}
}

func (k *keeper) held() *drive.SettleRecord {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.rec
}

// cutCaseSettings is cutCase's filesystem.
func cutCaseSettings(name string) icbfs.Settings {
	return icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015)}
}

// cutCase is the review's folder: a@2020, b@2022, c@2024, and x written
// at 2021, deferred.
func cutCase(t *testing.T, store objstore.Store, name string, cfg Config) (*Drive, []drive.LateChange) {
	t.Helper()
	d := newDriveWith(t, store, cutCaseSettings(name), cfg)
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

// TestSettleCutAtEveryRequest: a settle of the late write's changes is
// cut at each of its requests in turn: in "cut", that request and every
// one after fail; in "lost", that request is made and its answer lost
// (the store took it; the settle saw an error), and every one after
// fails. The caller settles again with the same changes and the record
// the cut settle handed it (none if it was cut before it handed one
// out): in "lost", and at every odd request in "cut", from a new Drive
// over the same store with an empty history cache (the process
// restarted). That rerun is itself cut, at a few of its requests (all of
// them with T83_CUTTWICE set), from a new Drive, and the third run
// finishes. Then the caller writes into the folder on time. Whatever the
// cuts, the folder ends with the present it had before (x kept), x in
// the past from 2021, the on-time write in place, nothing newer than the
// present before but the final states (no root or .metadata version
// unlisted, shadowed or late), nothing current unreachable, the cut
// settles' new blocks that nothing reaches gone, and the record released
// (Done). Before the reruns and between them, every view is a state the
// folder really had.
func TestSettleCutAtEveryRequest(t *testing.T) {
	if raceOn {
		t.Skip("measured without the race detector: `just test-settle`")
	}
	if testing.Short() {
		t.Skip("one folder per request of a settle")
	}
	store := &failStore{Store: miniotest.Store(t)}
	cfg := Config{DeferLateFolders: true}

	// The settle uncut: how many requests it makes, and what it leaves.
	d, changes := cutCase(t, store, "cutall0", cfg)
	_ = changes[0].Folder
	store.arm(0, false)
	k := &keeper{}
	if _, err := settleAt(d, system(), changes, k.opts()); err != nil {
		t.Fatal(err)
	}
	total, seq := store.disarm()
	if k.done != 1 || k.held() != nil {
		t.Fatalf("an uncut settle: done %d times, record held %+v", k.done, k.held())
	}
	rep, err := d.fs.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	records := rep.Unreachable
	t.Logf("an uncut settle: %d requests, leaving %d .version records unreachable: %s", total, records, summarize(seq))

	// realView: a view of /dir is the folder as it was (x from 2024 on) or
	// as it will be (x from 2021 on): states it really had.
	realView := func(t *testing.T, d *Drive, label string) {
		t.Helper()
		for _, y := range []int{2020, 2021, 2022, 2023, 2024} {
			got := sortedAt(t, d, "/dir", midYear(y))
			before := map[int]string{2020: "a.txt", 2021: "a.txt", 2022: "a.txt,b.txt", 2023: "a.txt,b.txt", 2024: "a.txt,b.txt,c.txt,x.txt"}[y]
			if got != before && got != cutCaseAfter[y] {
				t.Errorf("%s: /dir as of mid-%d is %s: no state it had (%s before, %s after)", label, y, got, before, cutCaseAfter[y])
			}
		}
	}
	twice := envSet("T83_CUTTWICE")
	for _, mode := range []string{"cut", "lost"} {
		for cut := 1; cut <= total+1; cut++ {
			t.Run(fmt.Sprintf("%s at request %d of %d", mode, cut, total), func(t *testing.T) {
				name := fmt.Sprintf("%s%d", mode, cut)
				d, changes := cutCase(t, store, name, cfg)
				folder := changes[0].Folder
				ctx := system()
				before := liveNames(t, d, folder)
				k := &keeper{}
				if mode == "lost" {
					store.armLost(cut)
				} else {
					store.arm(cut, false)
				}
				_, err := settleAt(d, ctx, changes, k.opts())
				_, seq := store.disarm()
				if cut <= total && err == nil {
					t.Fatalf("cut at request %d (%s), and no error", cut, seq[len(seq)-1])
				}
				if cut > total && (err != nil || k.held() != nil) {
					t.Fatalf("not cut: %v, record held %+v", err, k.held())
				}
				cutRecs := []*drive.SettleRecord{k.held()}
				// The rerun, from a new process, itself cut at a request.
				var seconds []int
				switch {
				case twice:
					for c := 1; c <= total; c++ {
						seconds = append(seconds, c)
					}
				case cut%7 == 3:
					seconds = []int{1, total / 2, total - 1}
				}
				for _, second := range seconds {
					realView(t, d, "after the first cut")
					d = newDriveWith(t, store, cutCaseSettings(name), cfg)
					if mode == "lost" {
						store.armLost(second)
					} else {
						store.arm(second, false)
					}
					_, err := settleAt(d, ctx, changes, k.opts())
					store.disarm()
					cutRecs = append(cutRecs, k.held())
					if err == nil {
						break // the rerun had less to do than that
					}
				}
				realView(t, d, "after the cuts")
				// The caller settles again, with the record it holds.
				if mode == "lost" || cut%2 == 1 || len(seconds) > 0 {
					d = newDriveWith(t, store, cutCaseSettings(name), cfg)
				}
				if _, err := settleAt(d, ctx, changes, k.opts()); err != nil {
					t.Fatalf("settled again (record %+v): %v", k.held(), err)
				}
				if k.held() != nil {
					t.Errorf("settled again, and the record is held: %+v", k.held())
				}
				if got := liveNames(t, d, folder); got != before {
					t.Fatalf("the folder now after the cut and again: %s, want %s as before", got, before)
				}
				pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2023: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
				settleLeftovers(t, d, folder, records)
				if _, err := d.Write(at(ctx, 2025), "/dir/d.txt", strings.NewReader("d"), drive.Condition{}, drive.WriteOptions{}); err != nil {
					t.Fatal(err)
				}
				if got := liveNames(t, d, folder); got != cutCaseAfter[2025] {
					t.Errorf("the folder now after an on-time write: %s, want %s", got, cutCaseAfter[2025])
				}
				pastIs(t, d, cutCaseAfter)
				settleLeftovers(t, d, folder, records)
				rep, err := d.fs.Check(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, rec := range cutRecs {
					if rec == nil {
						continue
					}
					for _, k := range rec.Create {
						if slicesContain(rep.UnreachableListed, k) {
							t.Errorf("a cut settle's new block %s is left, unreachable", k)
						}
					}
				}
			})
		}
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

// TestSettleWindowRetried: a settle whose store fails the window's first
// request once ends with the folder settled: the present as it was, the
// past with the change, the settle's record released. (Until round 6 a
// settle whose caller went away inside its window ran on to its end
// too; now that is an immediate stop: TestST9_LF17_ImmediateStopCutsTheSettle.)
func TestSettleWindowRetried(t *testing.T) {
	store := &failStore{Store: miniotest.Store(t)}
	for _, c := range []struct {
		name   string
		cancel bool
	}{{"retried", false}} {
		t.Run(c.name, func(t *testing.T) {
			d, changes := cutCase(t, store, c.name, Config{DeferLateFolders: true})
			folder := changes[0].Folder
			ctx, cancel := context.WithCancel(system())
			defer cancel()
			// The window's first request: the settle's first write of the
			// folder's .metadata, after its listing of the folder.
			listed, hit := false, false
			store.mu.Lock()
			store.watch = func(op, key string) error {
				switch {
				case op == "HISTORY" && key == folder:
					listed = true
				case listed && !hit && op == "PUT" && key == folder+".metadata":
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
			k := &keeper{}
			_, err := settleAt(d, ctx, changes, k.opts())
			store.mu.Lock()
			store.watch = nil
			store.mu.Unlock()
			if !hit {
				t.Fatalf("the settle's window never came")
			}
			if err != nil {
				t.Fatalf("the settle: %v", err)
			}
			if k.done != 1 || k.held() != nil {
				t.Errorf("done %d, record held %+v", k.done, k.held())
			}
			if got := liveNames(t, d, folder); got != "a.txt,b.txt,c.txt,x.txt" {
				t.Errorf("the folder now: %s", got)
			}
			pastIs(t, d, map[int]string{2020: "a.txt", 2021: "a.txt,x.txt", 2022: "a.txt,b.txt,x.txt", 2024: "a.txt,b.txt,c.txt,x.txt"})
			settleLeftovers(t, d, folder, 0)
		})
	}
}
