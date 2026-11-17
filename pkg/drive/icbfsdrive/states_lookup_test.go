package icbfsdrive

// docs/STATES.md section 5, "Finding an item by its id": one test a row
// (T82-15 round 2). Rows proven by an older test are named in the T82-15
// report instead: LK-4 and LK-8, LK-9 (TestSearchByIDStopsAtItsBudget),
// LK-5 and KS-1 (TestSearchByIDCutSaysSoAndGoesOn), ME-2
// (TestSearchByIDPastViewKeepsItsOwnMemory), ME-3 (TestIDPathCacheBound),
// LC-5 (TestFindByOriginMatchesOldSearch).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// lookupWorld is a writer drive over a tree in the Clio large test's
// shape (searchTree) and a fresh reader over a counting store.
type lookupWorld struct {
	ctx   context.Context
	base  objstore.Store
	s     icbfs.Settings
	w     *Drive
	paths []string
	ids   map[string]string
}

func newLookupWorld(t *testing.T, name string, folders, depth int) *lookupWorld {
	t.Helper()
	base := miniotest.Store(t)
	s := icbfs.Settings{Name: name, Size: 1 << 32, RootMode: 0o777}
	w := newDrive(t, base, s, "")
	paths, ids := searchTree(t, w, folders, depth)
	return &lookupWorld{ctx: system(), base: base, s: s, w: w, paths: paths, ids: ids}
}

// reader is a fresh drive (a restart: nothing remembered) over a tap.
func (lw *lookupWorld) reader(t *testing.T, budget int) (*Drive, *searchTap) {
	t.Helper()
	tap := &searchTap{Store: lw.base}
	return newDriveWith(t, tap, lw.s, Config{SearchBudget: budget}), tap
}

// stat is d.Stat by ref, with the searches and store reads it made.
func stat(d *Drive, tap *searchTap, ctx context.Context, ref drive.ItemRef, at drive.At) (drive.Item, error, int64, int64) {
	w0, r0 := d.Walks(), tap.n.Load()
	it, err := d.Stat(ctx, ref, at)
	return it, err, d.Walks() - w0, tap.n.Load() - r0
}

// TestLK1_APathThatHoldsIsNotSearched: an id given with the path it is
// at costs what the path alone costs, and no search.
func TestLK1_APathThatHoldsIsNotSearched(t *testing.T) {
	lw := newLookupWorld(t, "lk1", 30, 6)
	p := lw.paths[5] + "/doc.txt"
	d, tap := lw.reader(t, 0)
	_, err, _, byPath := stat(d, tap, lw.ctx, drive.ByPath(p), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	d2, tap2 := lw.reader(t, 0)
	it, err, walks, reads := stat(d2, tap2, lw.ctx, drive.ItemRef{ID: lw.ids[p], Path: p}, drive.At{})
	if err != nil || it.Path != p || walks != 0 || reads > byPath {
		t.Errorf("by id with its path: %q, %v, %d searches, %d reads (by path alone: %d); want no search, no more reads", it.Path, err, walks, reads, byPath)
	}
}

// TestLK2_AStalePathTriesTheMemoryThenSearches: a path given that leads
// elsewhere is passed over; the memory answers if it holds, else a search.
func TestLK2_AStalePathTriesTheMemoryThenSearches(t *testing.T) {
	lw := newLookupWorld(t, "lk2", 30, 6)
	p := lw.paths[5] + "/doc.txt"
	other := lw.paths[6] + "/doc.txt"
	id := lw.ids[p]
	d, tap := lw.reader(t, 0)
	it, err, walks, _ := stat(d, tap, lw.ctx, drive.ItemRef{ID: id, Path: other}, drive.At{})
	if err != nil || it.Path != p || walks != 1 {
		t.Fatalf("a stale path, nothing remembered: %q, %v, %d searches; want %s by one search", it.Path, err, walks, p)
	}
	it, err, walks, _ = stat(d, tap, lw.ctx, drive.ItemRef{ID: id, Path: other}, drive.At{})
	if err != nil || it.Path != p || walks != 0 {
		t.Errorf("a stale path, the memory holding: %q, %v, %d searches; want %s from the memory", it.Path, err, walks, p)
	}
}

// TestLK3_TheMemoryHoldsNoSearch: an id found once is answered from the
// memory, with no search.
func TestLK3_TheMemoryHoldsNoSearch(t *testing.T) {
	lw := newLookupWorld(t, "lk3", 30, 6)
	p := lw.paths[7] + "/doc.txt"
	d, tap := lw.reader(t, 0)
	if _, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(lw.ids[p]), drive.At{}); err != nil || walks != 1 {
		t.Fatalf("first: %v, %d searches", err, walks)
	}
	if it, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(lw.ids[p]), drive.At{}); err != nil || it.Path != p || walks != 0 {
		t.Errorf("again: %q, %v, %d searches; want no search", it.Path, err, walks)
	}
}

// faultTap fails the first read of a key that contains failKey, once,
// with errStoreDown: a store that fails reading one folder.
type faultTap struct {
	objstore.Store
	mu      sync.Mutex
	failKey string
	failed  int
}

var errStoreDown = errors.New("the store is down for a moment")

func (f *faultTap) fail(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failKey != "" && strings.Contains(key, f.failKey) {
		f.failKey = ""
		f.failed++
		return errStoreDown
	}
	return nil
}

func (f *faultTap) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	if err := f.fail(key); err != nil {
		return nil, nil, err
	}
	return f.Store.Get(ctx, key)
}

func (f *faultTap) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	if err := f.fail(key); err != nil {
		return nil, nil, false, err
	}
	return f.Store.GetIfChanged(ctx, key, etag)
}

func (f *faultTap) Head(ctx context.Context, key string) (*objstore.Object, error) {
	if err := f.fail(key); err != nil {
		return nil, err
	}
	return f.Store.Head(ctx, key)
}

// TestLK6_AStoreErrorOnOneFolderIsNeverNotThere: the store fails reading
// one folder on the way to the item (a chain): the lookup answers that it
// could not search, with the store's error, never "not found"; asked
// again it reads that folder (kept in the search) and finds the item.
func TestLK6_AStoreErrorOnOneFolderIsNeverNotThere(t *testing.T) {
	lw := newLookupWorld(t, "lk6", 9, 9)
	deep := lw.paths[8] + "/doc.txt"
	mid, err := lw.w.Stat(lw.ctx, drive.ByPath(lw.paths[4]), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	tap := &faultTap{Store: lw.base}
	d := newDriveWith(t, tap, lw.s, Config{})
	tap.mu.Lock()
	tap.failKey = mid.ID
	tap.mu.Unlock()
	_, err = d.Stat(lw.ctx, drive.ByID(lw.ids[deep]), drive.At{})
	if tap.failed != 1 {
		t.Fatalf("the folder's read failed %d times; want 1", tap.failed)
	}
	if !errors.Is(err, drive.ErrNotSearched) || errors.Is(err, drive.ErrNotFound) || !errors.Is(err, errStoreDown) {
		t.Fatalf("a store error on one folder: %v; want ErrNotSearched with the store's error, not not-found", err)
	}
	if it, err := d.Stat(lw.ctx, drive.ByID(lw.ids[deep]), drive.At{}); err != nil || it.Path != deep {
		t.Errorf("asked again: %q, %v; want %s", it.Path, err, deep)
	}
}

// TestLK7_AFoundPathThatLeadsNowhereIsNotSearched: a search carried over
// calls reads a folder by the path it had when queued; a folder above is
// renamed meanwhile, so the found path leads nowhere. The answer is "could
// not be searched", never 404; the kept search is dropped and a later call
// finds the item where it is now.
func TestLK7_AFoundPathThatLeadsNowhereIsNotSearched(t *testing.T) {
	lw := newLookupWorld(t, "lk7", 9, 9)
	deep := lw.paths[8] + "/doc.txt"
	id := lw.ids[deep]
	d, tap := lw.reader(t, 2)
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); !errors.Is(err, drive.ErrNotSearched) {
		t.Fatalf("first call, budget 2: %v; want ErrNotSearched", err)
	}
	top := lw.paths[0]
	if _, err := lw.w.Move(lw.ctx, drive.ByPath(top), "/moved", false); err != nil {
		t.Fatal(err)
	}
	want := "/moved" + strings.TrimPrefix(deep, top)
	var leadsNowhere bool
	for call := 2; ; call++ {
		it, err := d.Stat(lw.ctx, drive.ByID(id), drive.At{})
		if err == nil {
			if it.Path != want {
				t.Fatalf("found at %s; want %s", it.Path, want)
			}
			break
		}
		if errors.Is(err, drive.ErrNotFound) {
			t.Fatalf("call %d: %v; an item that is there answered not found", call, err)
		}
		if !errors.Is(err, drive.ErrNotSearched) {
			t.Fatalf("call %d: %v", call, err)
		}
		if strings.Contains(err.Error(), "no longer leads") {
			leadsNowhere = true
		}
		if call > 30 {
			t.Fatal("never found")
		}
	}
	if !leadsNowhere {
		t.Error("the stale find was never reported as such")
	}
}

// TestLK10_TwoLookupsOfOneIDSearchOnce: while one lookup searches for an
// id, a second for the same id waits for it and uses its find; a third
// whose deadline ends while it waits answers "could not be searched" and
// searches nothing. One search in all.
func TestLK10_TwoLookupsOfOneIDSearchOnce(t *testing.T) {
	lw := newLookupWorld(t, "lk10", 30, 6)
	p := lw.paths[5] + "/doc.txt"
	id := lw.ids[p]
	d, tap := lw.reader(t, 0)
	type answer struct {
		it  drive.Item
		err error
	}
	second, third := make(chan answer, 1), make(chan answer, 1)
	tap.mu.Lock()
	tap.cutAfter = tap.n.Load() + 3
	tap.cut = func() { // inside the first search's third read
		go func() {
			it, err := d.Stat(lw.ctx, drive.ByID(id), drive.At{})
			second <- answer{it, err}
		}()
		go func() {
			ctx, cancel := context.WithTimeout(lw.ctx, 100*time.Millisecond)
			defer cancel()
			it, err := d.Stat(ctx, drive.ByID(id), drive.At{})
			third <- answer{it, err}
		}()
		time.Sleep(400 * time.Millisecond)
	}
	tap.mu.Unlock()
	w0 := d.Walks()
	first, err := d.Stat(lw.ctx, drive.ByID(id), drive.At{})
	if err != nil || first.Path != p {
		t.Fatalf("first: %q, %v", first.Path, err)
	}
	a := <-second
	if a.err != nil || a.it.Path != p {
		t.Errorf("second, the same id at once: %q, %v; want %s", a.it.Path, a.err, p)
	}
	b := <-third
	if !errors.Is(b.err, drive.ErrNotSearched) || !errors.Is(b.err, context.DeadlineExceeded) {
		t.Errorf("third, its deadline ending while it waits: %v; want ErrNotSearched and its deadline", b.err)
	}
	if n := d.Walks() - w0; n != 1 {
		t.Errorf("%d searches for one id asked three times at once; want 1", n)
	}
}

// TestLK11_ARenameAfterTheMemorysAnswer: the caller keeps the item it was
// given; the next lookup finds the remembered path stale and the item
// where it is now.
func TestLK11_ARenameAfterTheMemorysAnswer(t *testing.T) {
	lw := newLookupWorld(t, "lk11", 30, 6)
	p := lw.paths[5] + "/doc.txt"
	d, tap := lw.reader(t, 0)
	given, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(lw.ids[p]), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.w.Move(lw.ctx, drive.ByPath(p), p+".renamed", false); err != nil {
		t.Fatal(err)
	}
	if given.Path != p {
		t.Errorf("the item given: %s; want %s", given.Path, p)
	}
	if it, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(lw.ids[p]), drive.At{}); err != nil || it.Path != p+".renamed" || walks != 1 {
		t.Errorf("the next lookup: %q, %v, %d searches; want the new path by a search", it.Path, err, walks)
	}
}

// TestLK12_ARestartForgets: a new drive (a restart) remembers nothing and
// keeps no search: by id and path, no search; by id alone, one.
func TestLK12_ARestartForgets(t *testing.T) {
	lw := newLookupWorld(t, "lk12", 30, 6)
	p := lw.paths[5] + "/doc.txt"
	d, tap := lw.reader(t, 0)
	if _, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(lw.ids[p]), drive.At{}); err != nil || walks != 1 {
		t.Fatalf("before: %v, %d", err, walks)
	}
	d2, tap2 := lw.reader(t, 0)
	if _, err, walks, _ := stat(d2, tap2, lw.ctx, drive.ItemRef{ID: lw.ids[p], Path: p}, drive.At{}); err != nil || walks != 0 {
		t.Errorf("after a restart, with the path: %v, %d searches; want none", err, walks)
	}
	d3, tap3 := lw.reader(t, 0)
	if _, err, walks, _ := stat(d3, tap3, lw.ctx, drive.ByID(lw.ids[p]), drive.At{}); err != nil || walks != 1 {
		t.Errorf("after a restart, by id alone: %v, %d searches; want one", err, walks)
	}
}

// TestLK13_TheTrashBeforeASearchAndNotThereRemembered: an id in the trash
// is answered not found from the trash, with no search and a few reads; an
// id in no folder and not in the trash is searched for once and then
// remembered as not there for absentFor (10 minutes); a restore through
// the drive forgets it at once.
func TestLK13_TheTrashBeforeASearchAndNotThereRemembered(t *testing.T) {
	lw := newLookupWorld(t, "lk13", 30, 6)
	trashed := lw.paths[20]
	it, err := lw.w.Stat(lw.ctx, drive.ByPath(trashed), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lw.w.Delete(lw.ctx, drive.ByPath(trashed), true, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	d, tap := lw.reader(t, 4)
	_, err, walks, reads := stat(d, tap, lw.ctx, drive.ByID(it.ID), drive.At{})
	t.Logf("a trashed folder by id: %v, %d searches, %d reads", err, walks, reads)
	if !errors.Is(err, drive.ErrNotFound) || walks != 0 || reads > 8 {
		t.Errorf("a trashed folder by id: %v, %d searches, %d reads; want not found from the trash, no search", err, walks, reads)
	}
	_, err, walks, reads = stat(d, tap, lw.ctx, drive.ByID(it.ID), drive.At{})
	if !errors.Is(err, drive.ErrNotFound) || walks != 0 || reads != 0 {
		t.Errorf("again: %v, %d searches, %d reads; want not found, remembered", err, walks, reads)
	}
	// Restored through the drive: found again at once.
	te, err := d.trashedItem(lw.ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Restore(lw.ctx, te.ID, drive.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	// No longer remembered as not there: searched for again (over calls,
	// at a budget of 4), never answered not found.
	for call := 1; ; call++ {
		got, err := d.Stat(lw.ctx, drive.ByID(it.ID), drive.At{})
		if err == nil {
			if got.Path != trashed {
				t.Errorf("restored: %q; want %s", got.Path, trashed)
			}
			break
		}
		if !errors.Is(err, drive.ErrNotSearched) || call > 20 {
			t.Fatalf("restored, call %d: %v; want it found, searched for again", call, err)
		}
	}

	// Purged, or never here: one whole search, then remembered.
	const missing = "0000000000000000000000000000dead"
	m, mtap := lw.reader(t, 0)
	if _, err, walks, _ := stat(m, mtap, lw.ctx, drive.ByID(missing), drive.At{}); !errors.Is(err, drive.ErrNotFound) || walks != 1 {
		t.Fatalf("a missing id: %v, %d searches; want not found by one search", err, walks)
	}
	if _, err, walks, reads := stat(m, mtap, lw.ctx, drive.ByID(missing), drive.At{}); !errors.Is(err, drive.ErrNotFound) || walks != 0 || reads != 0 {
		t.Errorf("a missing id again: %v, %d searches, %d reads; want not found, remembered", err, walks, reads)
	}
	was := absentFor
	absentFor = 0
	defer func() { absentFor = was }()
	if _, err, walks, _ := stat(m, mtap, lw.ctx, drive.ByID(missing), drive.At{}); !errors.Is(err, drive.ErrNotFound) || walks != 1 {
		t.Errorf("a missing id once remembered no longer: %v, %d searches; want one search", err, walks)
	}
}

// TestME1_ThePresentRecordsWritesFoldersAndMoves: a file written, a
// folder made and an item moved in the present are then found by id with
// no search.
func TestME1_ThePresentRecordsWritesFoldersAndMoves(t *testing.T) {
	lw := newLookupWorld(t, "me1", 30, 6)
	d, tap := lw.reader(t, 0)
	f := write(t, lw.ctx, d, lw.paths[9]+"/new.txt", "x", drive.Condition{})
	dir, err := d.Mkdir(lw.ctx, lw.paths[10]+"/newdir", false)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := d.Move(lw.ctx, drive.ByPath(lw.paths[11]+"/doc.txt"), lw.paths[12]+"/moved.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []drive.Item{f, dir, moved} {
		if it, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(c.ID), drive.At{}); err != nil || it.Path != c.Path || walks != 0 {
			t.Errorf("%s by id: %q, %v, %d searches; want no search", c.Path, it.Path, err, walks)
		}
	}
}

// TestME4_AStaleEntryIsCheckedAndReplaced: an entry that leads elsewhere
// is not trusted; the search's find replaces it, so the next lookup needs
// no search.
func TestME4_AStaleEntryIsCheckedAndReplaced(t *testing.T) {
	lw := newLookupWorld(t, "me4", 30, 6)
	p := lw.paths[6] + "/doc.txt"
	id := lw.ids[p]
	d, tap := lw.reader(t, 0)
	d.idPaths.put(id, lw.paths[7]+"/doc.txt") // another item's path
	if it, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); err != nil || it.Path != p || walks != 1 {
		t.Fatalf("a stale entry: %q, %v, %d searches; want %s by a search", it.Path, err, walks, p)
	}
	if got, _ := d.idPaths.get(id); got != p {
		t.Errorf("the entry is %s; want %s", got, p)
	}
	if _, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); err != nil || walks != 0 {
		t.Errorf("then: %v, %d searches; want none", err, walks)
	}
}

// pastWorld is a lookupWorld with a moment of the past taken after the
// tree was written.
func pastWorld(t *testing.T, name string, folders, depth int) (*lookupWorld, time.Time) {
	t.Helper()
	lw := newLookupWorld(t, name, folders, depth)
	time.Sleep(1500 * time.Millisecond)
	then, err := lw.w.StoreTime(lw.ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	return lw, then
}

// dropViews drops every view d keeps, as an eviction does, while a lookup
// may hold one.
func dropViews(d *Drive) {
	d.views.mu.Lock()
	clear(d.views.views)
	d.views.mu.Unlock()
}

// TestME5_AViewDroppedDuringALookupKeepsItsFinds: a lookup in a view of
// the past whose view is dropped while it runs stores its find, and its
// kept search, under the view's moment: the next lookup at that moment
// has them.
func TestME5_AViewDroppedDuringALookupKeepsItsFinds(t *testing.T) {
	lw, then := pastWorld(t, "me5", 30, 9)
	deep := lw.paths[8] + "/doc.txt"
	id := lw.ids[deep]
	at := drive.At{Time: then}

	// The find.
	d, tap := lw.reader(t, 0)
	fsys, err := d.at(lw.ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	dropViews(d)
	if n, err := d.resolve(lw.ctx, fsys, drive.ByID(id)); err != nil || n.path != deep {
		t.Fatalf("in a dropped view: %v", err)
	}
	if it, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(id), at); err != nil || it.Path != deep || walks != 0 {
		t.Errorf("at the same moment after: %q, %v, %d searches; want the find remembered", it.Path, err, walks)
	}

	// The kept search: a budget of 2, one call in a dropped view, then
	// calls at that moment until found; a fresh drive's count less one.
	callsToFind := func(d *Drive) int {
		for call := 1; ; call++ {
			_, err := d.Stat(lw.ctx, drive.ByID(id), at)
			if err == nil {
				return call
			}
			if !errors.Is(err, drive.ErrNotSearched) || call > 40 {
				t.Fatalf("call %d: %v", call, err)
			}
		}
	}
	fresh, _ := lw.reader(t, 2)
	whole := callsToFind(fresh)
	d2, _ := lw.reader(t, 2)
	fsys2, err := d2.at(lw.ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	dropViews(d2)
	if _, err := d2.resolve(lw.ctx, fsys2, drive.ByID(id)); !errors.Is(err, drive.ErrNotSearched) {
		t.Fatalf("one call in a dropped view, budget 2: %v; want ErrNotSearched", err)
	}
	if got := callsToFind(d2); got != whole-1 {
		t.Errorf("after one call in a dropped view: %d more calls to find it; a fresh drive takes %d, so want %d", got, whole, whole-1)
	}
}

// keptAll is "searched N folders (N in this call)": a search that started
// from the root in this call.
func keptAll(err error, budget int) bool {
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("searched %d folders (%d in this call)", budget, budget))
}

// TestKS2_AKeptSearchOlderThanTwoMinutesStartsAgain: a kept search past
// stoppedFor is not gone on from: the next call starts from the root.
func TestKS2_AKeptSearchOlderThanTwoMinutesStartsAgain(t *testing.T) {
	lw := newLookupWorld(t, "ks2", 30, 9)
	id := lw.ids[lw.paths[8]+"/doc.txt"]
	d, tap := lw.reader(t, 3)
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); !keptAll(err, 3) {
		t.Fatalf("first: %v", err)
	}
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); keptAll(err, 3) {
		t.Fatalf("within two minutes: %v; want it gone on from", err)
	}
	d.stopped.mu.Lock()
	for _, s := range d.stopped.m {
		s.at = time.Now().Add(-stoppedFor - time.Second)
	}
	d.stopped.mu.Unlock()
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); !keptAll(err, 3) {
		t.Errorf("after two minutes: %v; want it started again from the root", err)
	}
}

// TestKS3_KeptSearchesAreBounded: a 17th search to keep drops the oldest;
// one with more folders to read than maxStoppedDirs is not kept.
func TestKS3_KeptSearchesAreBounded(t *testing.T) {
	var s stoppedSearches
	for i := range maxStopped + 1 {
		s.keep(stoppedKey{id: fmt.Sprint(i)}, &stoppedSearch{queue: []searchDir{{"k", "/p"}}})
		time.Sleep(time.Millisecond)
	}
	if len(s.m) != maxStopped {
		t.Errorf("%d kept; want %d", len(s.m), maxStopped)
	}
	if _, ok := s.m[stoppedKey{id: "0"}]; ok {
		t.Error("the oldest was kept")
	}
	big := &stoppedSearch{queue: make([]searchDir, maxStoppedDirs+1)}
	s.keep(stoppedKey{id: "big"}, big)
	if _, ok := s.m[stoppedKey{id: "big"}]; ok || s.dirs > maxStoppedDirs {
		t.Errorf("a search too large to keep was kept (%d folders in all)", s.dirs)
	}
}

// TestKS4_ThePastChangingDropsThePastsKeptSearches: when the past changes,
// a search kept in a view of the past starts again; the present's goes on.
func TestKS4_ThePastChangingDropsThePastsKeptSearches(t *testing.T) {
	lw, then := pastWorld(t, "ks4", 30, 9)
	id := lw.ids[lw.paths[8]+"/doc.txt"]
	d, _ := lw.reader(t, 3)
	at := drive.At{Time: then}
	if _, err := d.Stat(lw.ctx, drive.ByID(id), at); !keptAll(err, 3) {
		t.Fatalf("past, first: %v", err)
	}
	if _, err := d.Stat(lw.ctx, drive.ByID(id), drive.At{}); !keptAll(err, 3) {
		t.Fatalf("present, first: %v", err)
	}
	d.views.lookAgain() // as a change to the past through this drive does
	if _, err := d.Stat(lw.ctx, drive.ByID(id), at); !keptAll(err, 3) {
		t.Errorf("past, after the past changed: %v; want it started again", err)
	}
	if _, err := d.Stat(lw.ctx, drive.ByID(id), drive.At{}); keptAll(err, 3) {
		t.Errorf("present, after the past changed: %v; want it gone on from", err)
	}
}

// TestKS5_AFoundItemDropsItsKeptSearch: once found, nothing is kept.
func TestKS5_AFoundItemDropsItsKeptSearch(t *testing.T) {
	lw := newLookupWorld(t, "ks5", 30, 9)
	id := lw.ids[lw.paths[8]+"/doc.txt"]
	d, _ := lw.reader(t, 8)
	for call := 1; ; call++ {
		_, err := d.Stat(lw.ctx, drive.ByID(id), drive.At{})
		if err == nil {
			break
		}
		if call > 20 {
			t.Fatal(err)
		}
	}
	d.stopped.mu.Lock()
	defer d.stopped.mu.Unlock()
	if len(d.stopped.m) != 0 || d.stopped.dirs != 0 {
		t.Errorf("found, yet %d searches and %d folders kept", len(d.stopped.m), d.stopped.dirs)
	}
}

// TestLC3_AStubTargetThatCannotBeSearchedIsUnavailable: a write by
// reference whose target is past the search budget (no path given) is
// refused as ErrRefUnavailable, which the caller answers by writing the
// bytes in full, not with a failed write; given the target's path, the
// stub is written with no search.
func TestLC3_AStubTargetThatCannotBeSearchedIsUnavailable(t *testing.T) {
	lw := newLookupWorld(t, "lc3", 30, 9)
	ctx := lw.ctx
	x := write(t, ctx, lw.w, lw.paths[8]+"/x.txt", sameAsData, drive.Condition{})
	d, _ := lw.reader(t, 2)
	ref := refTo(x)
	if _, err := sameAs(ctx, d, "/stub.txt", ref, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrRefUnavailable) || errors.Is(err, drive.ErrNotSearched) {
		t.Fatalf("a target past the budget: %v; want ErrRefUnavailable", err)
	}
	ref.Path = x.Path
	w0 := d.Walks()
	if _, err := sameAs(ctx, d, "/stub.txt", ref, drive.Condition{}, drive.WriteOptions{}); err != nil || d.Walks() != w0 {
		t.Errorf("with the target's path: %v, %d searches; want a stub, no search", err, d.Walks()-w0)
	}
	if got := read(t, ctx, d, drive.ByPath("/stub.txt"), drive.At{}); got != sameAsData {
		t.Errorf("the stub reads %d bytes; want the target's", len(got))
	}
}
