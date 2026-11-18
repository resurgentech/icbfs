package icbfsdrive

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// PS-31 (interloc docs/STATES.md 4.1): one settle of one folder can
// outlast the limit after which a daemon takes its pass for hung, so the
// settle tells its caller each time it moves (SettleOptions.Progress).

// holdStore holds one Erase, when armed, until released: a store call
// that does not answer.
type holdStore struct {
	objstore.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *holdStore) Erase(ctx context.Context, key, v string) error {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.Erase(ctx, key, v)
}

// count is how many requests were recorded so far.
func (s *seqStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seq)
}

// ownCount is how many of what was recorded are the operation's own.
func ownCount(seq []string) int {
	n := 0
	for _, r := range seq {
		if ownRequest(r) {
			n++
		}
	}
	return n
}

// progressFolder is a folder /dir of 40 entries in blocks of 10 with
// states states (one entry named anew in each), and lateN late creates
// spread over its history, deferred: their changes, to settle. The
// requests are recorded from the settle on (store.on).
func progressFolder(t *testing.T, raw objstore.Store, states, lateN int) (*Drive, *seqStore, *holdStore, []drive.LateChange) {
	t.Helper()
	ctx := system()
	hold := &holdStore{Store: raw, entered: make(chan struct{}), release: make(chan struct{})}
	store := &seqStore{Store: hold}
	const entries = 40
	d := newDriveWith(t, store, icbfs.Settings{Name: fmt.Sprintf("ps31-%d-%d", states, lateN), Size: 1 << 30, RootMode: 0o777,
		Created: year(2015), DirBlockEntries: 10}, Config{DeferLateFolders: true})
	items := make([]string, entries)
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
		for j := 0; j < entries-1; j++ {
			s.Entries = append(s.Entries, drive.FolderEntry{Name: fmt.Sprintf("f%03d", j), ID: items[j], Type: drive.TypeFile})
		}
		s.Entries = append(s.Entries, drive.FolderEntry{Name: fmt.Sprintf("v%03d", i), ID: items[entries-1], Type: drive.TypeFile})
		hist[i] = s
	}
	if err := d.ReplaceFolderHistory(ctx, drive.ByPath("/dir"), hist, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	late := &drive.LateChanges{}
	lctx := drive.WithLateChanges(ctx, late)
	for k := 0; k < lateN; k++ {
		when := stateAt(2 + k*(states-4)/lateN).Add(time.Minute)
		if _, err := d.Write(drive.WithSourceTime(lctx, when), fmt.Sprintf("/dir/late%02d", k), strings.NewReader("late"), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	changes := late.Take()
	if len(changes) != lateN {
		t.Fatalf("%d late changes, want %d", len(changes), lateN)
	}
	time.Sleep(pastMarkEvery + 100*time.Millisecond)
	d.past.flush()
	store.take()
	store.on.Store(true)
	return d, store, hold, changes
}

// report is one report as the test saw it: what the settle said, and
// how many requests the store had been asked by then.
type report struct {
	p        drive.SettleProgress
	requests int
}

var stepOrder = map[string]int{
	drive.SettleBuild: 0, drive.SettlePlay: 1, drive.SettleProve: 2, drive.SettlePlan: 3, drive.SettleRecordIt: 4,
	drive.SettleNewBlocks: 5, drive.SettleWindow: 6, drive.SettleCommit: 7, drive.SettleErase: 8, drive.SettleDone: 9,
}

// maxCallsBetweenReports is the most store calls a settle makes between
// two reports on this folder: one state's view in the build, whose calls
// grow with the state's blocks (five here: the first view fills the
// history cache with each block's history, its .version's history and
// both read; measured 19 at four blocks, base 2026-11-18). A whole
// block's erase is one call here (seqStore records EraseAll once): its
// own requests are one a version, inside the call, with no report
// (PS-31's bound).
const maxCallsBetweenReports = 30

// TestPS31_ASettleReportsEachTimeItMoves: a settle reports each state
// read as it builds, each state written, each version erased, in order,
// with how far it is (states written of the states to write); never more
// than a few store calls pass between two reports; every report is made
// before SettleFolder returns; and the reports make no request: the same
// settle with no function makes the same requests.
func TestPS31_ASettleReportsEachTimeItMoves(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds two folders on MinIO")
	}
	bucket := newBuckets(t)

	// With no function: the requests to compare with.
	d0, store0, _, changes0 := progressFolder(t, bucket(t), 16, 6)
	want, err := d0.SettleFolder(system(), changes0, drive.SettleOptions{Path: "/dir"})
	store0.on.Store(false)
	if err != nil {
		t.Fatal(err)
	}
	wantRequests := ownCount(store0.take())

	d, store, _, changes := progressFolder(t, bucket(t), 16, 6)
	var reports []report
	var returned atomic.Bool
	lateReports := 0
	got, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir", Progress: func(p drive.SettleProgress) {
		if returned.Load() {
			lateReports++
		}
		reports = append(reports, report{p, store.count()})
	}})
	returned.Store(true)
	store.on.Store(false)
	if err != nil {
		t.Fatal(err)
	}
	seq := store.take()
	if lateReports > 0 {
		t.Errorf("%d reports after SettleFolder returned", lateReports)
	}
	if got != want {
		t.Errorf("settled with a function %+v, without %+v", got, want)
	}
	if n := ownCount(seq); n != wantRequests {
		t.Errorf("the settle's requests: %d with a function, %d without", n, wantRequests)
	}
	if got.Written < 8 || got.Read < 8 || got.Erased < 8 {
		t.Fatalf("the folder's settle is too small to tell: %+v", got)
	}
	if len(reports) == 0 {
		t.Fatal("no report")
	}

	// In order, one part, and what each step counts.
	var buildReads, eraseLast, lastStep int
	var window []int
	for i, r := range reports {
		p := r.p
		if p.Part != 1 || p.Parts != 1 {
			t.Fatalf("report %d: part %d of %d, want 1 of 1", i, p.Part, p.Parts)
		}
		o, ok := stepOrder[p.Step]
		if !ok {
			t.Fatalf("report %d: step %q", i, p.Step)
		}
		if o < lastStep {
			t.Errorf("report %d: step %q after a later step", i, p.Step)
		}
		lastStep = o
		if p.Of > 0 && p.Done > p.Of {
			t.Errorf("report %d: %s %d of %d", i, p.Step, p.Done, p.Of)
		}
		switch p.Step {
		case drive.SettleBuild:
			if p.Done > buildReads {
				buildReads = p.Done
			}
		case drive.SettleWindow:
			window = append(window, p.Done)
			if p.Written != p.Done || p.ToWrite != got.Written {
				t.Errorf("report %d: window %d of %d, states written %d of %d; want %d of %d", i, p.Done, p.Of, p.Written, p.ToWrite, p.Done, got.Written)
			}
		case drive.SettleErase:
			if p.Of != got.Erased {
				t.Errorf("report %d: erase of %d, the settle erased %d", i, p.Of, got.Erased)
			}
			eraseLast = p.Done
		}
	}
	// Each state read: the present's read too, so one more than Settled.Read.
	if buildReads < got.Read {
		t.Errorf("the build reported %d states read, the settle read %d", buildReads, got.Read)
	}
	if len(window) != got.Written {
		t.Errorf("%d window reports, %d states written", len(window), got.Written)
	}
	for i, n := range window {
		if n != i+1 {
			t.Errorf("window report %d says %d states written", i, n)
			break
		}
	}
	if eraseLast != got.Erased {
		t.Errorf("the erase reported %d versions erased, the settle erased %d", eraseLast, got.Erased)
	}
	if last := reports[len(reports)-1].p; last.Step != drive.SettleDone || last.Done != 1 {
		t.Errorf("the last report is %+v, want the settle done", last)
	}
	if s := reports[len(reports)/2].p.String(); !strings.HasPrefix(s, "part 1 of 1: ") {
		t.Errorf("a report as a status: %q", s)
	}

	// Never more than a few store calls between two reports.
	worst, at := reports[0].requests, 0
	for i := 1; i < len(reports); i++ {
		if g := reports[i].requests - reports[i-1].requests; g > worst {
			worst, at = g, i
		}
	}
	if g := len(seq) - reports[len(reports)-1].requests; g > worst {
		worst, at = g, len(reports)
	}
	t.Logf("%d reports, %d store calls; at most %d between two reports (before report %d); settled %+v", len(reports), len(seq), worst, at, got)
	if worst > maxCallsBetweenReports {
		from := 0
		if at > 0 {
			from = reports[at-1].requests
		}
		to := len(seq)
		if at < len(reports) {
			to = reports[at].requests
		}
		t.Errorf("%d store calls with no report between them (want at most %d): %s", worst, maxCallsBetweenReports, summarize(seq[from:to]))
	}
}

// TestPS31_AStoreCallThatNeverAnswersReportsNothing: while one store
// call of the settle does not answer, the settle reports nothing (a daemon
// then calls it hung, as it should); once it answers the settle reports
// again and finishes.
func TestPS31_AStoreCallThatNeverAnswersReportsNothing(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds a folder on MinIO")
	}
	d, store, hold, changes := progressFolder(t, newBuckets(t)(t), 12, 4)
	var mu sync.Mutex
	var last time.Time
	var count int
	progress := func(p drive.SettleProgress) {
		if p.Step == drive.SettleCommit && p.Done == 1 {
			hold.armed.Store(true) // the erase's first request will not answer
		}
		mu.Lock()
		last, count = time.Now(), count+1
		mu.Unlock()
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir", Progress: progress})
		done <- err
	}()
	select {
	case <-hold.entered:
	case err := <-done:
		t.Fatalf("the settle ended (%v) before its erase", err)
	case <-time.After(2 * time.Minute):
		t.Fatal("the settle did not reach its erase in two minutes")
	}
	mu.Lock()
	before := count
	mu.Unlock()
	time.Sleep(time.Second)
	mu.Lock()
	during, quiet := count-before, time.Since(last)
	mu.Unlock()
	if during != 0 || quiet < time.Second {
		t.Errorf("while a store call did not answer: %d reports, the last %s ago; want none for the whole second", during, quiet)
	}
	close(hold.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the settle did not finish once the call answered")
	}
	store.on.Store(false)
	mu.Lock()
	defer mu.Unlock()
	if count <= before {
		t.Errorf("no report once the call answered")
	}
}

// TestPS31_APanickingFunctionDoesNotFailTheSettle: a function that
// panics is told nothing more, and the settle's result is as with no
// function.
func TestPS31_APanickingFunctionDoesNotFailTheSettle(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds two folders on MinIO")
	}
	bucket := newBuckets(t)
	d0, _, _, changes0 := progressFolder(t, bucket(t), 12, 4)
	want, err := d0.SettleFolder(system(), changes0, drive.SettleOptions{Path: "/dir", Progress: nil})
	if err != nil {
		t.Fatal(err)
	}

	d, _, _, changes := progressFolder(t, bucket(t), 12, 4)
	calls := 0
	got, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir", Progress: func(p drive.SettleProgress) {
		calls++
		if calls == 5 {
			panic("a caller's fault")
		}
	}})
	if err != nil {
		t.Fatalf("a panicking function failed the settle: %v", err)
	}
	if got != want {
		t.Errorf("settled %+v with a panicking function, %+v with none", got, want)
	}
	if calls != 5 {
		t.Errorf("the function was called %d times, want 5 (none after it panicked)", calls)
	}
}

// TestPS31_EachPartIsNamed: a settle in several parts names each, and
// each reports to its end.
func TestPS31_EachPartIsNamed(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a folder on MinIO")
	}
	defer func(n int) { MaxSettleChanges = n }(MaxSettleChanges)
	MaxSettleChanges = 2 // 4 changes: two parts
	d, _, _, changes := progressFolder(t, newBuckets(t)(t), 12, 4)
	parts := map[int]bool{}
	_, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir", Progress: func(p drive.SettleProgress) {
		if p.Parts != 2 {
			t.Errorf("part %d of %d, want of 2", p.Part, p.Parts)
		}
		if p.Step == drive.SettleDone && p.Done == 1 {
			parts[p.Part] = true
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !parts[1] || !parts[2] {
		t.Errorf("parts done: %v, want 1 and 2", parts)
	}
}

// TestPS31_ARerunOfACutSettleReports: a settle cut in its window (an
// immediate stop) is finished by a rerun with its record, which reports
// as any settle does, to its end.
func TestPS31_ARerunOfACutSettleReports(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds a folder on MinIO")
	}
	d, _, _, changes := progressFolder(t, newBuckets(t)(t), 12, 4)
	ctx, cancel := context.WithCancel(system())
	defer cancel()
	var rec *drive.SettleRecord
	_, err := d.SettleFolder(ctx, changes, drive.SettleOptions{Path: "/dir",
		Record: func(r drive.SettleRecord) error { rec = &r; return nil },
		Progress: func(p drive.SettleProgress) {
			if p.Step == drive.SettleWindow && p.Done == 2 {
				cancel()
			}
		}})
	if err == nil || rec == nil {
		t.Fatalf("the settle was not cut in its window: %v, record %v", err, rec != nil)
	}
	steps := map[string]int{}
	var last drive.SettleProgress
	s, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir", Cut: rec, Progress: func(p drive.SettleProgress) {
		steps[p.Step]++
		last = p
	}})
	if err != nil {
		t.Fatal(err)
	}
	if steps[drive.SettleBuild] < s.Read || steps[drive.SettleErase] < s.Erased || last.Step != drive.SettleDone {
		t.Errorf("the rerun's reports: %v, the last %+v; it settled %+v", steps, last, s)
	}
}
