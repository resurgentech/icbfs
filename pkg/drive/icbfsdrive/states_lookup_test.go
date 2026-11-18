package icbfsdrive

// docs/STATES.md section 5, "Finding an item by its id": one test a row
// (T82-15 rounds 2 and 3), named by the row. Rows proven by an older test
// carry a "STATES:" comment above that test.

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
// remembered as not there for absentFor (2 minutes); a restore through
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
	if absentFor != 2*time.Minute {
		t.Errorf("not there is remembered for %v; want 2 minutes, as a kept search", absentFor)
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
		s.keep(stoppedKey{id: fmt.Sprint(i)}, &stoppedSearch{queue: []searchDir{{"k", "/p"}}}, 0)
		time.Sleep(time.Millisecond)
	}
	if len(s.m) != maxStopped {
		t.Errorf("%d kept; want %d", len(s.m), maxStopped)
	}
	if _, ok := s.m[stoppedKey{id: "0"}]; ok {
		t.Error("the oldest was kept")
	}
	big := &stoppedSearch{queue: make([]searchDir, maxStoppedDirs+1)}
	s.keep(stoppedKey{id: "big"}, big, 0)
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

// --- round 3 (the review of 2026-11-17) ---

const missingID = "0000000000000000000000000000beef"

// TestLK15_AQueuedFolderGoneIsNotSearched (the review's RVRow_LK15): a kept
// search whose next folder is gone answers "could not be searched", never
// "not there", naming no path; the kept search is dropped, so the next
// call starts from the root.
func TestLK15_AQueuedFolderGoneIsNotSearched(t *testing.T) {
	lw := newLookupWorld(t, "lk15", 30, 9)
	d, tap := lw.reader(t, 3)
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(missingID), drive.At{}); !keptAll(err, 3) {
		t.Fatalf("first: %v", err)
	}
	key := stoppedKey{id: missingID}
	d.stopped.mu.Lock()
	s := d.stopped.m[key]
	if s == nil {
		d.stopped.mu.Unlock()
		t.Fatal("nothing kept")
	}
	// A folder the search queued that is gone now (purged since).
	s.queue = append([]searchDir{{"lk15-0000000000000000000000000000gone", "/ghost-name"}}, s.queue...)
	d.stopped.dirs++
	d.stopped.mu.Unlock()
	_, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(missingID), drive.At{})
	if errors.Is(err, drive.ErrNotFound) || !errors.Is(err, drive.ErrNotSearched) {
		t.Fatalf("a queued folder gone: %v; want ErrNotSearched, never not found", err)
	}
	if strings.Contains(err.Error(), "ghost-name") {
		t.Errorf("the answer names the path the search had queued: %v", err)
	}
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(missingID), drive.At{}); !keptAll(err, 3) {
		t.Errorf("after: %v; want the kept search dropped (started from the root)", err)
	}
}

// TestLK16_TheTrashUnreadableIsNotSearched (the review's RVRow_LK16): the
// store fails reading the trash's pointer for the id: "could not be
// searched" with the store's error, never "not there".
func TestLK16_TheTrashUnreadableIsNotSearched(t *testing.T) {
	lw := newLookupWorld(t, "lk16", 9, 9)
	tap := &faultTap{Store: lw.base}
	d := newDriveWith(t, tap, lw.s, Config{})
	tap.mu.Lock()
	tap.failKey = "-trashby-"
	tap.mu.Unlock()
	_, err := d.Stat(lw.ctx, drive.ByID(missingID), drive.At{})
	if tap.failed != 1 {
		t.Fatalf("the trash's read failed %d times; want 1", tap.failed)
	}
	if !errors.Is(err, drive.ErrNotSearched) || errors.Is(err, drive.ErrNotFound) || !errors.Is(err, errStoreDown) {
		t.Errorf("the trash unreadable: %v; want ErrNotSearched with the store's error", err)
	}
}

// TestLK17_NotThereComesBackAnotherWay: an id this drive found in the
// trash is answered "not there" for absentFor even after another process
// restores it, unless the caller gives a path that leads to it; once the
// time is over it is searched for and found.
func TestLK17_NotThereComesBackAnotherWay(t *testing.T) {
	lw := newLookupWorld(t, "lk17", 30, 6)
	p := lw.paths[20] + "/doc.txt"
	id := lw.ids[p]
	if err := lw.w.Delete(lw.ctx, drive.ByPath(p), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	d, tap := lw.reader(t, 0)
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("trashed: %v", err)
	}
	te, err := lw.w.trashedItem(lw.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.w.Restore(lw.ctx, te.ID, drive.RestoreOptions{}); err != nil { // another process
		t.Fatal(err)
	}
	if _, err, walks, reads := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); !errors.Is(err, drive.ErrNotFound) || walks != 0 || reads != 0 {
		t.Errorf("restored by another, within the time: %v, %d searches, %d reads; want not found, remembered", err, walks, reads)
	}
	if it, err, walks, _ := stat(d, tap, lw.ctx, drive.ItemRef{ID: id, Path: p}, drive.At{}); err != nil || it.Path != p || walks != 0 {
		t.Errorf("with the path that leads to it: %q, %v, %d searches; want found, no search", it.Path, err, walks)
	}
	was := absentFor
	absentFor = 0
	defer func() { absentFor = was }()
	if it, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); err != nil || it.Path != p {
		t.Errorf("after the time: %q, %v; want found by a search", it.Path, err)
	}
}

// TestLK18_ASecondLookupCarriesOnUnderItsOwnBudget: a lookup that stopped
// at its budget is carried on by the next lookup of the id, which reads
// its own budget's worth from where the first stopped.
func TestLK18_ASecondLookupCarriesOnUnderItsOwnBudget(t *testing.T) {
	lw := newLookupWorld(t, "lk18", 30, 9)
	id := lw.ids[lw.paths[8]+"/doc.txt"]
	d, tap := lw.reader(t, 3)
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{}); !keptAll(err, 3) {
		t.Fatalf("first: %v", err)
	}
	_, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(id), drive.At{})
	if err == nil || !strings.Contains(err.Error(), "searched 6 folders (3 in this call)") {
		t.Errorf("second: %v; want 3 more folders read from where the first stopped", err)
	}
}

// TestLK10_AWaiterWhoseFirstIsCutGoesOn (the review's RVRow_LK10): the
// first lookup of an id is cut while a second waits for it: the second
// neither hangs nor takes the first's error; it goes on and finds the
// item, and nothing is left marked running.
func TestLK10_AWaiterWhoseFirstIsCutGoesOn(t *testing.T) {
	lw := newLookupWorld(t, "lk10b", 30, 9)
	p := lw.paths[8] + "/doc.txt"
	id := lw.ids[p]
	d, tap := lw.reader(t, 0)
	cctx, cancel := context.WithCancel(lw.ctx)
	defer cancel()
	type answer struct {
		it  drive.Item
		err error
	}
	second := make(chan answer, 1)
	tap.mu.Lock()
	tap.cutAfter = tap.n.Load() + 4
	tap.cut = func() {
		go func() {
			it, err := d.Stat(lw.ctx, drive.ByID(id), drive.At{})
			second <- answer{it, err}
		}()
		time.Sleep(300 * time.Millisecond)
		cancel()
	}
	tap.mu.Unlock()
	_, err := d.Stat(cctx, drive.ByID(id), drive.At{})
	if !errors.Is(err, drive.ErrNotSearched) || !errors.Is(err, context.Canceled) {
		t.Fatalf("first, cut: %v", err)
	}
	select {
	case a := <-second:
		if a.err != nil || a.it.Path != p {
			t.Errorf("second, waiting on a cut first: %q, %v; want %s", a.it.Path, a.err, p)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the second lookup hangs")
	}
	d.stopped.mu.Lock()
	defer d.stopped.mu.Unlock()
	if len(d.stopped.running) != 0 {
		t.Errorf("%d searches still marked running", len(d.stopped.running))
	}
}

// TestLK19_AForbiddenFindIsTheRefusalAndNamesNoFoundPath (the review's
// RVGap, B1): a caller who may not traverse /private asks by id for a file
// in it. The search (which reads as the drive) finds it; the answer is the
// refusal a lookup by that path gets, naming only the folder refused,
// never the found file's name.
func TestLK19_AForbiddenFindIsTheRefusalAndNamesNoFoundPath(t *testing.T) {
	base := miniotest.Store(t)
	s := icbfs.Settings{Name: "lk19", Size: 1 << 30, RootMode: 0o777}
	alice, bob := as(1000, 1000), as(1001, 1001)
	w := newDrive(t, base, s, "")
	if _, err := w.Mkdir(alice, "/private", false); err != nil {
		t.Fatal(err)
	}
	sec := write(t, alice, w, "/private/secret-name.txt", "s", drive.Condition{})
	fs := w.Filesystem()
	e, _, _ := fs.Lookup(context.Background(), fs.RootKey(), "private")
	mode := uint32(0o700)
	if _, err := fs.SetAttr(context.Background(), e.UUID, icbfs.TypeDir, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := newDriveWith(t, base, s, Config{SearchBudget: DefaultSearchBudget})
	_, byPath := d.Stat(bob, drive.ByPath("/private/secret-name.txt"), drive.At{})
	_, err := d.Stat(bob, drive.ByID(sec.ID), drive.At{})
	t.Logf("bob, by path: %v; by id: %v", byPath, err)
	if err == nil {
		t.Fatal("bob reached a file in a 0700 folder of alice's")
	}
	if errors.Is(err, drive.ErrNotSearched) || !errors.Is(err, drive.ErrForbidden) {
		t.Errorf("a find the caller may not reach: %v; want ErrForbidden, not ErrNotSearched", err)
	}
	if strings.Contains(err.Error(), "secret-name") {
		t.Errorf("the answer names the file under a folder the caller may not traverse: %v", err)
	}
}

// trashWorld is a drive whose trash holds junk entries and one file, x.
func trashWorld(t *testing.T, name string, junk int) (objstore.Store, icbfs.Settings, drive.Item) {
	t.Helper()
	base := miniotest.Store(t)
	s := icbfs.Settings{Name: name, Size: 1 << 32, RootMode: 0o777}
	ctx := system()
	w := newDrive(t, base, s, "")
	for i := range junk {
		p := fmt.Sprintf("/junk/f%03d.txt", i)
		write(t, ctx, w, p, "x", drive.Condition{})
		if err := w.Delete(ctx, drive.ByPath(p), false, drive.Condition{}); err != nil {
			t.Fatal(err)
		}
	}
	x := write(t, ctx, w, "/t/target.txt", "target\n", drive.Condition{})
	if err := w.Delete(ctx, drive.ByPath("/t/target.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	return base, s, x
}

// TestLK20_ATrashedItemsEntryIsReadByItsID (the review's RVGap, B2): the
// callers that need a trashed item's entry (Checksums for an approval,
// Versions, Stat with a version) read it by the item's id: the same
// number of reads with 10 entries in the trash as with 150.
func TestLK20_ATrashedItemsEntryIsReadByItsID(t *testing.T) {
	ctx := system()
	type cost struct{ sums, versions, stat int64 }
	measure := func(junk int) cost {
		base, s, x := trashWorld(t, fmt.Sprintf("lk20-%d", junk), junk)
		var c cost
		for i, f := range []func(d *Drive) error{
			func(d *Drive) error {
				_, err := d.Checksums(ctx, drive.ItemRef{ID: x.ID, Path: "/t/target.txt"}, x.Version.ID)
				return err
			},
			func(d *Drive) error { _, err := d.Versions(ctx, drive.ByID(x.ID)); return err },
			func(d *Drive) error {
				_, err := d.Stat(ctx, drive.ByID(x.ID), drive.At{Version: x.Version.ID})
				return err
			},
		} {
			tap := &searchTap{Store: base}
			d := newDriveWith(t, tap, s, Config{SearchBudget: DefaultSearchBudget})
			r0 := tap.n.Load()
			// Checksums of a small trashed file answers that its
			// checksums were never kept (no .metadata to read), after
			// it found the entry: only its reads count here.
			if err := f(d); err != nil && i > 0 {
				t.Fatalf("%d entries, call %d: %v", junk+1, i, err)
			}
			n := tap.n.Load() - r0
			switch i {
			case 0:
				c.sums = n
			case 1:
				c.versions = n
			case 2:
				c.stat = n
			}
		}
		return c
	}
	small, large := measure(10), measure(150)
	t.Logf("reads with 11 trash entries %+v; with 151 %+v", small, large)
	grew := large.sums > small.sums+2 || large.versions > small.versions+2 || large.stat > small.stat+2
	if grew || large.sums > 40 {
		t.Errorf("reads for a trashed item's entry grow with the trash: 11 entries %+v, 151 entries %+v (G5)", small, large)
	}
}

// TestLK21_ARestoredFolderBringsBackItsChildren (the review's RVGap, U2):
// a document whose folder was trashed is answered "not there" and
// remembered so; the folder restored through the same drive brings the
// document back, and it is found by id at once.
func TestLK21_ARestoredFolderBringsBackItsChildren(t *testing.T) {
	lw := newLookupWorld(t, "lk21", 30, 6)
	folder := lw.paths[20]
	doc := folder + "/doc.txt"
	docID := lw.ids[doc]
	fit, err := lw.w.Stat(lw.ctx, drive.ByPath(folder), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lw.w.Delete(lw.ctx, drive.ByPath(folder), true, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	d, tap := lw.reader(t, 0)
	if _, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(docID), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("the document, its folder trashed: %v; want not found", err)
	}
	te, err := d.trashedItem(lw.ctx, fit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Restore(lw.ctx, te.ID, drive.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if it, err, _, _ := stat(d, tap, lw.ctx, drive.ByID(docID), drive.At{}); err != nil || it.Path != doc {
		t.Errorf("the document by id after its folder was restored through this drive: %q, %v; want %s", it.Path, err, doc)
	}
}

// TestLC7_ACutStubLookupIsTheStopsError (the review's RVRow_LC7): refTarget
// whose lookup is cut by its context answers the stop's error, not
// ErrRefUnavailable: a stop never becomes a write of the bytes in full.
func TestLC7_ACutStubLookupIsTheStopsError(t *testing.T) {
	lw := newLookupWorld(t, "lc7", 30, 9)
	x := write(t, lw.ctx, lw.w, lw.paths[8]+"/x.txt", sameAsData, drive.Condition{})
	d, tap := lw.reader(t, 0)
	cctx, cancel := context.WithCancel(lw.ctx)
	defer cancel()
	tap.mu.Lock()
	tap.cutAfter, tap.cut = tap.n.Load()+4, cancel
	tap.mu.Unlock()
	_, err := d.refTarget(cctx, *refTo(x), nil)
	if errors.Is(err, drive.ErrRefUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("a stub's target lookup cut by a stop: %v; want the stop's error, not ErrRefUnavailable", err)
	}
}

// TestME7_ThePastsNotThereGoesWhenThePastChanges (the review's RVRow_ME7):
// an id found in no folder as of a time is searched for again once the
// past changed.
func TestME7_ThePastsNotThereGoesWhenThePastChanges(t *testing.T) {
	lw, then := pastWorld(t, "me7", 20, 4)
	d, tap := lw.reader(t, 0)
	at := drive.At{Time: then}
	if _, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(missingID), at); !errors.Is(err, drive.ErrNotFound) || walks != 1 {
		t.Fatalf("first: %v, %d searches", err, walks)
	}
	if _, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(missingID), at); !errors.Is(err, drive.ErrNotFound) || walks != 0 {
		t.Fatalf("again: %v, %d searches; want remembered", err, walks)
	}
	d.views.lookAgain()
	if _, err, walks, _ := stat(d, tap, lw.ctx, drive.ByID(missingID), at); !errors.Is(err, drive.ErrNotFound) || walks != 1 {
		t.Errorf("after the past changed: %v, %d searches; want searched again", err, walks)
	}
}

// TestME8_APastChangeDuringALookupStoresNothing (the review's RVGap, U1):
// lookups in a view of the past that the past's change dropped while they
// held it store nothing under the moment: not "not there", not a kept
// search, not a find.
func TestME8_APastChangeDuringALookupStoresNothing(t *testing.T) {
	lw, then := pastWorld(t, "me8", 30, 9)
	deep := lw.paths[8] + "/doc.txt"
	near := lw.paths[0] + "/doc.txt"
	// A drive with no budget (not there, a find) and one with a budget of
	// 3 (a kept search), each holding a view of then whose past changes.
	held := func(budget int) (*Drive, *icbfs.Filesystem, *idPathCache, time.Time) {
		d, _ := lw.reader(t, budget)
		fsys, err := d.at(lw.ctx, drive.At{Time: then})
		if err != nil {
			t.Fatal(err)
		}
		mem, mt, _, _ := d.memory(fsys)
		d.views.mu.Lock()
		d.views.forgetLocked()
		d.views.mu.Unlock()
		return d, fsys, mem, mt
	}
	d0, fsys0, mem, mt := held(0)
	if _, err := d0.resolve(lw.ctx, fsys0, drive.ByID(missingID)); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a missing id in the old past: %v", err)
	}
	if d0.stopped.isAbsent(stoppedKey{at: mt, id: missingID}) {
		t.Error("a not-there learned in a past already changed is kept for the new past")
	}
	if n, err := d0.resolve(lw.ctx, fsys0, drive.ByID(lw.ids[near])); err != nil || n.path != near {
		t.Fatalf("a near id in the old past: %v", err)
	}
	if _, ok := mem.get(lw.ids[near]); ok {
		t.Error("a find in a past already changed is stored for the new past")
	}
	d, fsys, _, mt := held(3)
	if _, err := d.resolve(lw.ctx, fsys, drive.ByID(lw.ids[deep])); !errors.Is(err, drive.ErrNotSearched) {
		t.Fatalf("a deep id in the old past, budget 3: %v", err)
	}
	d.stopped.mu.Lock()
	_, kept := d.stopped.m[stoppedKey{at: mt, id: lw.ids[deep]}]
	d.stopped.mu.Unlock()
	if kept {
		t.Error("a search stopped in a past already changed is kept for the new past")
	}
}

// TestME9_AFilesystemTheDriveDidNotMakeSharesNothing (the review's U3):
// lookups in two filesystems this drive did not make keep no search and
// remember nothing, so neither can answer for the other.
func TestME9_AFilesystemTheDriveDidNotMakeSharesNothing(t *testing.T) {
	lw := newLookupWorld(t, "me9", 30, 9)
	d, _ := lw.reader(t, 3)
	other := func() *icbfs.Filesystem {
		f := icbfs.New(lw.base, lw.s.Name)
		if err := f.Attach(lw.ctx); err != nil {
			t.Fatal(err)
		}
		return f
	}
	a, b := other(), other()
	if _, err := d.resolve(lw.ctx, a, drive.ByID(missingID)); !errors.Is(err, drive.ErrNotSearched) {
		t.Fatalf("a missing id in a, budget 3: %v", err)
	}
	if _, err := d.resolve(lw.ctx, b, drive.ByID(missingID)); !keptAll(err, 3) {
		t.Errorf("the same id in b: %v; want b's own search from the root, not a's kept one", err)
	}
	big, _ := lw.reader(t, 0)
	if _, err := big.resolve(lw.ctx, a, drive.ByID(missingID)); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a missing id in a, no budget: %v", err)
	}
	big.stopped.mu.Lock()
	n, k := len(big.stopped.absent), len(big.stopped.m)
	big.stopped.mu.Unlock()
	d.stopped.mu.Lock()
	k += len(d.stopped.m)
	d.stopped.mu.Unlock()
	if n != 0 || k != 0 {
		t.Errorf("lookups in filesystems the drive did not make: %d remembered not there, %d searches kept; want none", n, k)
	}
}
