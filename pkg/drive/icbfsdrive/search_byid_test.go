package icbfsdrive

// T82-15: a lookup by id that must search the tree (interloc report
// 2026-11-17, lookups by id alone in Drives): a view of the past keeps
// its finds out of the present's memory, and a search cut short says so
// and is not started again from nothing.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// searchTap counts the reads that reach the store and can cancel a
// context after a number of them (a request cut mid-search).
type searchTap struct {
	objstore.Store
	n        atomic.Int64
	mu       sync.Mutex
	cutAfter int64
	cut      context.CancelFunc
}

func (s *searchTap) tick() {
	n := s.n.Add(1)
	s.mu.Lock()
	if s.cut != nil && n >= s.cutAfter {
		s.cut()
		s.cut = nil
	}
	s.mu.Unlock()
}

func (s *searchTap) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.tick()
	return s.Store.Get(ctx, key)
}

func (s *searchTap) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	s.tick()
	return s.Store.GetIfChanged(ctx, key, etag)
}

func (s *searchTap) Head(ctx context.Context, key string) (*objstore.Object, error) {
	s.tick()
	return s.Store.Head(ctx, key)
}

func (s *searchTap) GetVersion(ctx context.Context, key, v string) (io.ReadCloser, *objstore.Object, error) {
	s.tick()
	return s.Store.GetVersion(ctx, key, v)
}

func (s *searchTap) HeadVersion(ctx context.Context, key, v string) (*objstore.Object, error) {
	s.tick()
	return s.Store.HeadVersion(ctx, key, v)
}

func (s *searchTap) History(ctx context.Context, key string) ([]objstore.Object, error) {
	s.tick()
	return s.Store.History(ctx, key)
}

func (s *searchTap) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.tick()
	return s.Store.HistoryUnder(ctx, prefix)
}

func (s *searchTap) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.tick()
	return s.Store.List(ctx, prefix)
}

// searchTree is the Clio large test's shape (memory_test.go's genTree,
// as the report's probe): a chain of `depth` folders, the rest a ternary
// heap under three top-level ones, one file in each. It returns the
// folders' paths in the order made and the files' ids by path.
func searchTree(t *testing.T, d drive.Drive, folders, depth int) (paths []string, ids map[string]string) {
	t.Helper()
	ctx := system()
	type folder struct{ parent int }
	var fs []folder
	for i := 0; i < depth; i++ {
		fs = append(fs, folder{parent: i - 1})
	}
	var heap []int
	for len(fs) < folders {
		k := len(heap)
		f := folder{parent: -1}
		if k >= 3 {
			f.parent = heap[(k-3)/3]
		}
		heap = append(heap, len(fs))
		fs = append(fs, f)
	}
	ids = map[string]string{}
	for i, f := range fs {
		p := fmt.Sprintf("/f%05d-xxxxx", i)
		if f.parent >= 0 {
			p = paths[f.parent] + p
		}
		paths = append(paths, p)
		it := write(t, ctx, d, p+"/doc.txt", "x", drive.Condition{})
		ids[p+"/doc.txt"] = it.ID
	}
	return paths, ids
}

// TestSearchByIDPastViewKeepsItsOwnMemory: a Draft's folder renamed in
// the library after the moment taken as the Draft's base, looked up by
// id now (Follow) and as of the base (folderAt), three times each on one
// drive: one search in each view, then none. Before T82-15 the two
// lookups shared one memory and overwrote each other: 6 searches in 6
// calls.
// STATES: ME-2
func TestSearchByIDPastViewKeepsItsOwnMemory(t *testing.T) {
	base := miniotest.Store(t)
	s := icbfs.Settings{Name: "pastview", Size: 1 << 32, RootMode: 0o777}
	ctx := system()
	w := newDrive(t, base, s, "")
	paths, _ := searchTree(t, w, 40, 4)
	// The heap's fourth folder, at depth 2 under its first, with folders
	// below it: a Draft's folder.
	folder := paths[4+3]
	it, err := w.Stat(ctx, drive.ByPath(folder), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	id := it.ID
	time.Sleep(1500 * time.Millisecond)
	then, err := w.StoreTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	renamed := folder + "-renamed"
	if _, err := w.Move(ctx, drive.ByPath(folder), renamed, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)

	d := newDriveWith(t, base, s, Config{})
	var walks []int64
	for i := range 3 {
		w0 := d.Walks()
		now, err := d.Stat(ctx, drive.ByID(id), drive.At{})
		if err != nil || now.Path != renamed {
			t.Fatalf("round %d, now: %q, %v; want %s", i, now.Path, err, renamed)
		}
		w1 := d.Walks()
		past, err := d.Stat(ctx, drive.ByID(id), drive.At{Time: then})
		if err != nil || past.Path != folder {
			t.Fatalf("round %d, as of the base: %q, %v; want %s", i, past.Path, err, folder)
		}
		walks = append(walks, w1-w0, d.Walks()-w1)
	}
	t.Logf("searches per call (now, then; three rounds): %v", walks)
	want := []int64{1, 1, 0, 0, 0, 0}
	for i := range want {
		if walks[i] != want[i] {
			t.Fatalf("searches per call %v; want %v (one in each view, then none)", walks, want)
		}
	}
}

// TestSearchByIDCutSaysSoAndGoesOn: a lookup by id whose request is cut
// mid-search answers that it could not search (the context's error), not
// "not found"; asked again on the same drive, it goes on from where it
// stopped instead of reading every folder again.
// STATES: LK-5
// STATES: KS-1
func TestSearchByIDCutSaysSoAndGoesOn(t *testing.T) {
	base := miniotest.Store(t)
	s := icbfs.Settings{Name: "cutsearch", Size: 1 << 32, RootMode: 0o777}
	ctx := system()
	w := newDrive(t, base, s, "")
	paths, ids := searchTree(t, w, 60, 9)
	deep := paths[8] + "/doc.txt" // the chain's last folder, depth 9: read last but a few
	id := ids[deep]

	// The whole search, on a fresh drive.
	fullTap := &searchTap{Store: base}
	full := newDriveWith(t, fullTap, s, Config{})
	r0 := fullTap.n.Load()
	if it, err := full.Stat(ctx, drive.ByID(id), drive.At{}); err != nil || it.Path != deep {
		t.Fatalf("by id on a fresh drive: %q, %v", it.Path, err)
	}
	whole := fullTap.n.Load() - r0

	tap := &searchTap{Store: base}
	d := newDriveWith(t, tap, s, Config{})
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tap.mu.Lock()
	tap.cutAfter, tap.cut = tap.n.Load()+whole/2, cancel
	tap.mu.Unlock()
	_, err := d.Stat(cctx, drive.ByID(id), drive.At{})
	t.Logf("cut after %d of %d reads: %v", whole/2, whole, err)
	if err == nil || errors.Is(err, drive.ErrNotFound) || !errors.Is(err, context.Canceled) {
		t.Errorf("a search cut short: %v; want the context's error, not not-found", err)
	}
	r1 := tap.n.Load()
	it, err := d.Stat(ctx, drive.ByID(id), drive.At{})
	if err != nil || it.Path != deep {
		t.Fatalf("asked again: %q, %v", it.Path, err)
	}
	again := tap.n.Load() - r1
	t.Logf("the whole search: %d reads; asked again after the cut: %d reads", whole, again)
	if again > whole-whole/4 {
		t.Errorf("asked again after a cut at half way: %d reads of the whole search's %d; want it to go on, not start again", again, whole)
	}
}
