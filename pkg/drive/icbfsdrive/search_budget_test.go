package icbfsdrive

import (
	"errors"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestSearchByIDStopsAtItsBudget (T82-15): a drive with a search budget
// reads at most that many folders in one lookup by id, answers
// ErrNotSearched (not ErrNotFound) when it stops, and goes on from there
// on the next call, so the calls together read each folder once. An id
// that isn't there is "not found" once the search has read every folder:
// in one call when the tree is within the budget, else after the calls
// that read it all.
// STATES: LK-4
// STATES: LK-8
// STATES: LK-9
func TestSearchByIDStopsAtItsBudget(t *testing.T) {
	base := miniotest.Store(t)
	s := icbfs.Settings{Name: "budget", Size: 1 << 32, RootMode: 0o777}
	ctx := system()
	w := newDrive(t, base, s, "")
	const folders = 60
	paths, ids := searchTree(t, w, folders, 9)
	deep := paths[8] + "/doc.txt"
	const budget = 8

	tap := &searchTap{Store: base}
	d := newDriveWith(t, tap, s, Config{SearchBudget: budget})
	calls, reads := 0, int64(0)
	for {
		calls++
		r0 := tap.n.Load()
		it, err := d.Stat(ctx, drive.ByID(ids[deep]), drive.At{})
		n := tap.n.Load() - r0
		reads += n
		if err == nil {
			if it.Path != deep {
				t.Fatalf("found at %s; want %s", it.Path, deep)
			}
			break
		}
		if !errors.Is(err, drive.ErrNotSearched) || errors.Is(err, drive.ErrNotFound) {
			t.Fatalf("call %d: %v; want ErrNotSearched", calls, err)
		}
		if n > budget+4 {
			t.Errorf("call %d: %d reads; want at most the budget of %d folders and the drive's root", calls, n, budget)
		}
		if calls > folders {
			t.Fatal("never found")
		}
	}
	t.Logf("found by call %d, %d reads in all, budget %d, %d folders", calls, reads, budget, folders+1)
	if calls < (folders+1)/budget-1 {
		t.Errorf("found by call %d; with a budget of %d it can't have read the tree sooner", calls, budget)
	}
	if reads > int64(folders+1)+int64(calls)*4+8 {
		t.Errorf("%d reads over %d calls: folders read again, not gone on from", reads, calls)
	}
	// Found: remembered.
	w0 := d.Walks()
	if _, err := d.Stat(ctx, drive.ByID(ids[deep]), drive.At{}); err != nil || d.Walks() != w0 {
		t.Errorf("again after it was found: %v, %d searches", err, d.Walks()-w0)
	}

	// An id that isn't there: not found once every folder is read.
	const missing = "0000000000000000000000000000dead"
	small := newDriveWith(t, base, s, Config{SearchBudget: 4 * folders})
	if _, err := small.Stat(ctx, drive.ByID(missing), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Errorf("a missing id, the tree within the budget: %v; want not found", err)
	}
	tight := newDriveWith(t, base, s, Config{SearchBudget: budget})
	var err error
	calls = 0
	for calls = 1; calls <= folders; calls++ {
		_, err = tight.Stat(ctx, drive.ByID(missing), drive.At{})
		if !errors.Is(err, drive.ErrNotSearched) {
			break
		}
	}
	if !errors.Is(err, drive.ErrNotFound) {
		t.Errorf("a missing id over calls with a budget of %d: %v; want not found in the end", budget, err)
	}
	if want := (folders + 1 + budget - 1) / budget; calls < want || calls > want+1 {
		t.Errorf("a missing id: not found by call %d; want %d (every folder read once)", calls, want)
	}
}
