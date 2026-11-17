package icbfsdrive

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// seqStore records, while on, every request that reaches the store, in
// order, as "OP key". A read of the store's clock and a past-changed mark
// are recorded but not requests of the operation's own (see
// requestCounter.take).
type seqStore struct {
	objstore.Store
	on  atomic.Bool
	mu  sync.Mutex
	seq []string
}

func (s *seqStore) add(op, key string) {
	if !s.on.Load() {
		return
	}
	s.mu.Lock()
	s.seq = append(s.seq, op+" "+key)
	s.mu.Unlock()
}

// take returns what was recorded and starts again.
func (s *seqStore) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.seq
	s.seq = nil
	return out
}

func ownRequest(r string) bool {
	return !strings.HasPrefix(r, "TIME ") && !strings.HasSuffix(r, "-past")
}

func (s *seqStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	s.add("PUT", key)
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func (s *seqStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	s.add("PUT-absent", key)
	return s.Store.PutIfAbsent(ctx, key, body, md)
}

func (s *seqStore) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	s.add("PUT-parts", key)
	return s.Store.PutParts(ctx, key, parts, md, ifMatch)
}

func (s *seqStore) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.add("GET", key)
	return s.Store.Get(ctx, key)
}

func (s *seqStore) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	s.add("GET-if-changed", key)
	return s.Store.GetIfChanged(ctx, key, etag)
}

func (s *seqStore) Head(ctx context.Context, key string) (*objstore.Object, error) {
	s.add("HEAD", key)
	return s.Store.Head(ctx, key)
}

func (s *seqStore) GetVersion(ctx context.Context, key, v string) (io.ReadCloser, *objstore.Object, error) {
	s.add("GET-version", key)
	return s.Store.GetVersion(ctx, key, v)
}

func (s *seqStore) HeadVersion(ctx context.Context, key, v string) (*objstore.Object, error) {
	s.add("HEAD-version", key)
	return s.Store.HeadVersion(ctx, key, v)
}

func (s *seqStore) Delete(ctx context.Context, key string) error {
	s.add("DELETE", key)
	return s.Store.Delete(ctx, key)
}

func (s *seqStore) Undelete(ctx context.Context, key string) error {
	s.add("UNDELETE", key)
	return s.Store.Undelete(ctx, key)
}

func (s *seqStore) History(ctx context.Context, key string) ([]objstore.Object, error) {
	s.add("HISTORY", key)
	return s.Store.History(ctx, key)
}

func (s *seqStore) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.add("HISTORY-under", prefix)
	return s.Store.HistoryUnder(ctx, prefix)
}

func (s *seqStore) Erase(ctx context.Context, key, v string) error {
	s.add("ERASE", key)
	return s.Store.Erase(ctx, key, v)
}

func (s *seqStore) EraseAll(ctx context.Context, prefix string) error {
	s.add("ERASE-all", prefix)
	return s.Store.EraseAll(ctx, prefix)
}

func (s *seqStore) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.add("LIST", prefix)
	return s.Store.List(ctx, prefix)
}

func (s *seqStore) ServerTime(ctx context.Context) (time.Time, error) {
	s.add("TIME", "-")
	return s.Store.ServerTime(ctx)
}

// settleCase is one measured settle: a folder of 300 entries (three
// blocks of 100) with states states, 30 late creates dated between its
// third-last and second-last states, deferred, then one SettleFolder.
type settleCase struct {
	requests int      // the settle's own requests
	seq      []string // all of them, in order
	written  int      // states written
	peak     uint64   // the heap's peak during the settle over the heap before it
	folder   string
}

const (
	settleEntries = 300
	settleLate    = 30
)

func measureSettle(t *testing.T, states int) settleCase {
	t.Helper()
	// Twice, the lower peak kept: a peak sampled every millisecond
	// includes what garbage the collector has not come to, which varies
	// by a few hundred KiB from run to run (round 5: 1,136 to 1,272 KiB at
	// 20 states, 1,415 to 1,698 at 200, the same tree); the requests are
	// the same both times.
	bucket := newBuckets(t)
	a := measureSettleOf(t, bucket(t), states, settleLate, false)
	b := measureSettleOf(t, bucket(t), states, settleLate, false)
	if b.peak < a.peak {
		a.peak = b.peak
	}
	return a
}

// measureSettleOf is measureSettle with late changes, on store: dated
// between the folder's third-last and second-last states, or, spread,
// one in every state's hour from the third on.
func measureSettleOf(t *testing.T, raw objstore.Store, states, lateN int, spread bool) settleCase {
	t.Helper()
	ctx := system()
	store := &seqStore{Store: raw}
	name := fmt.Sprintf("settle%d-%d-%v", states, lateN, spread)
	d := newDriveWith(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 100},
		Config{DeferLateFolders: true})

	// The items: written now; the folder's history is then written over
	// with states states, each of the 300 entries, one of them named
	// anew in each (so no two states are the same).
	items := make([]string, settleEntries)
	for i := range items {
		it, err := d.Write(ctx, fmt.Sprintf("/dir/f%03d", i), strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{Parents: true})
		if err != nil {
			t.Fatal(err)
		}
		items[i] = it.ID
	}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	stateAt := func(i int) time.Time { return base.Add(time.Duration(i) * time.Hour) }
	hist := make([]drive.FolderState, states)
	for i := range hist {
		s := drive.FolderState{At: stateAt(i)}
		for j := 0; j < settleEntries-1; j++ {
			s.Entries = append(s.Entries, drive.FolderEntry{Name: fmt.Sprintf("f%03d", j), ID: items[j], Type: drive.TypeFile})
		}
		s.Entries = append(s.Entries, drive.FolderEntry{Name: fmt.Sprintf("v%03d", i), ID: items[settleEntries-1], Type: drive.TypeFile})
		hist[i] = s
	}
	if err := d.ReplaceFolderHistory(ctx, drive.ByPath("/dir"), hist, drive.Condition{}); err != nil {
		t.Fatal(err)
	}

	late := &drive.LateChanges{}
	lctx := drive.WithLateChanges(ctx, late)
	for k := 0; k < lateN; k++ {
		when := stateAt(states - 3).Add(time.Duration(k+1) * time.Second)
		if spread {
			when = stateAt(2 + k*(states-4)/lateN).Add(time.Minute)
		}
		if _, err := d.Write(drive.WithSourceTime(lctx, when), fmt.Sprintf("/dir/late%02d", k), strings.NewReader("late"), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	changes := late.Take()
	if len(changes) != lateN {
		t.Fatalf("%d late changes, want %d", len(changes), lateN)
	}
	// Quiet: no past-changed mark still put off.
	time.Sleep(pastMarkEvery + 100*time.Millisecond)
	d.past.flush()

	// The collector runs at 5% growth while the settle does: the peak is
	// then what the settle holds, not garbage the collector hasn't come
	// to yet (which depends on when it last ran).
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	before := m.HeapAlloc
	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	store.take()
	store.on.Store(true)
	settled, err := d.SettleFolder(ctx, changes, drive.SettleOptions{})
	store.on.Store(false)
	close(stop)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	c := settleCase{seq: store.take(), written: settled.Written, folder: changes[0].Folder}
	for _, r := range c.seq {
		if ownRequest(r) {
			c.requests++
		}
	}
	if p := peak.Load(); p > before {
		c.peak = p - before
	}

	// The folder is right: each late file from its time, the present as
	// it was (the last state's entries and the late files).
	if !spread {
		if got := strings.Count(names(t, d, "/dir", stateAt(states-3).Add(15*time.Second+500*time.Millisecond)), "late"); got != min(15, lateN) {
			t.Errorf("%d states: late files fifteen and a half seconds into the late creates: %d, want %d", states, got, min(15, lateN))
		}
	}
	if got := strings.Split(names(t, d, "/dir", time.Now()), ","); len(got) != settleEntries+lateN {
		t.Errorf("%d states: the folder now has %d entries, want %d", states, len(got), settleEntries+lateN)
	}
	return c
}

// TestSettleRequestsBoundedByLaterStates is T83-1's gate: a settle's
// requests are bounded by the folder's states from its earliest change
// on, the same for a folder of 20 states as for one of 200, and so is
// the heap it holds; and the window in which the folder's present is
// between two states holds nothing but the root block's and .metadata's
// writes, two a state (TestSettleWindow's check, on both).
func TestSettleRequestsBoundedByLaterStates(t *testing.T) {
	if raceOn {
		t.Skip("measured without the race detector: `just test-settle`")
	}
	if testing.Short() {
		t.Skip("builds two folders of 300 entries")
	}
	small := measureSettle(t, 20)
	large := measureSettle(t, 200)
	t.Logf("settle requests: %d at 20 states, %d at 200; states written %d and %d; peak heap over the heap before: %s at 20, %s at 200",
		small.requests, large.requests, small.written, large.written, kib(small.peak), kib(large.peak))
	if small.requests != large.requests {
		t.Errorf("a settle's requests: %d at 20 states, %d at 200; want the same:\n20: %s\n200: %s",
			small.requests, large.requests, summarize(small.seq), summarize(large.seq))
	}
	if small.written != large.written {
		t.Errorf("states written: %d at 20, %d at 200", small.written, large.written)
	}
	// The heap: what a settle holds is a state or two of 330 entries
	// and, in one list each, the folder's root and .metadata versions and
	// its .version table: those grow with the states, at under 2 KiB a
	// state (measured: 1.1 MiB at 20 states, 1.3 MiB at 200, the
	// collector at 5%). So 200 states may hold 512 KiB more than 20. A
	// settle that held every state (200 of 330 entries, about 60 bytes
	// each: 4 MiB), or a copy of the history per state, would not fit.
	const allowance = 512 << 10
	if large.peak > small.peak+allowance {
		t.Errorf("peak heap during the settle: %s at 20 states, %s at 200; want no more than %s more",
			kib(small.peak), kib(large.peak), kib(allowance))
	}
	for _, c := range []settleCase{small, large} {
		checkWindow(t, c)
	}
}

// TestSettleGateTable measures a settle's requests and heap against the
// folder's states and the late changes queued (the round-3 review's
// table: 20, 200 and 2,000 states; 30 and 500 changes; 500 spread over
// the history). Long: run when T83_GATE is set (the lab).
func TestSettleGateTable(t *testing.T) {
	if !envSet("T83_GATE") {
		t.Skip("T83_GATE unset: the gate's table runs on the lab")
	}
	bucket := newBuckets(t)
	for _, c := range []struct {
		states, late int
		spread       bool
	}{{20, 30, false}, {200, 30, false}, {2000, 30, false}, {20, 500, false}, {200, 500, false}, {200, 500, true}} {
		m := measureSettleOf(t, bucket(t), c.states, c.late, c.spread)
		t.Logf("GATE states %d, late changes %d (spread %v): %d requests, %d states written, peak heap over before %s; %s",
			c.states, c.late, c.spread, m.requests, m.written, kib(m.peak), summarize(m.seq))
		checkWindow(t, m)
	}
}

func kib(n uint64) string { return fmt.Sprintf("%.0f KiB", float64(n)/1024) }

// summarize counts seq by request, for a failure message.
func summarize(seq []string) string {
	n := map[string]int{}
	var order []string
	for _, r := range seq {
		op, key, _ := strings.Cut(r, " ")
		class := "block"
		switch {
		case strings.HasSuffix(key, ".metadata"):
			class = ".metadata"
		case strings.HasSuffix(key, ".version"):
			class = ".version"
		case strings.HasSuffix(key, "-past"):
			class = "past"
		}
		k := op + " " + class
		if n[k] == 0 {
			order = append(order, k)
		}
		n[k]++
	}
	var b strings.Builder
	for _, k := range order {
		fmt.Fprintf(&b, "%s=%d ", k, n[k])
	}
	return b.String()
}

// checkWindow: between the settle's first write to the folder's root
// block or .metadata and its last, nothing but those writes, two per
// state written (.metadata, then the root block).
func checkWindow(t *testing.T, c settleCase) {
	t.Helper()
	root, meta := "PUT "+c.folder, "PUT "+c.folder+".metadata"
	first, last := -1, -1
	for i, r := range c.seq {
		if r == root || r == meta {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		t.Fatalf("the settle wrote neither the root block nor .metadata")
	}
	window := c.seq[first : last+1]
	t.Logf("the window: %d requests for %d states written", len(window), c.written)
	for i, r := range window {
		want := meta
		if i%2 == 1 {
			want = root
		}
		if r != want {
			t.Errorf("request %d of the window: %s, want %s", i, r, want)
			break
		}
	}
	if len(window) != 2*c.written {
		t.Errorf("the window holds %d requests, want %d (2 a state)", len(window), 2*c.written)
	}
}
