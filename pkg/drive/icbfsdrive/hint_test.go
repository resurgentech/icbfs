package icbfsdrive

import (
	"fmt"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestFindByOriginHintAfterRestart (T82-13): a folder moved on the drive
// leaves the origin records of the items under it naming their old
// paths, and a drive opened since (a daemon's start) remembers no path,
// so a lookup with no hint walks the tree (findPaths) for each of them.
// Told where an item is now, the lookup makes no walk, finds the item
// the walk finds, and corrects its record, so the next lookup, hinted or
// not, from a drive opened after, makes none either. A hint that is
// wrong, stale, or names another item costs its one resolve and falls
// through to the same answer as no hint.
func TestFindByOriginHintAfterRestart(t *testing.T) {
	store := miniotest.Store(t)
	s := icbfs.Settings{Name: "hint", Size: 1 << 30, RootMode: 0o777}
	ctx := system()
	d := newDrive(t, store, s, "")
	m := drive.NewMirror(d, srcDrive)
	src := &source{}
	if _, err := m.Apply(ctx, drive.SourceItem{Origin: drive.Origin{ID: "folder", Version: "1"}, Path: "/A/matter", Type: drive.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		id := fmt.Sprint("doc", i)
		if _, err := m.Apply(ctx, src.item(id, "1", fmt.Sprintf("/A/matter/%s.txt", id), mirrorData(id), time.Time{})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Apply(ctx, src.item("other", "1", "/A/other.txt", mirrorData("other"), time.Time{})); err != nil {
		t.Fatal(err)
	}
	// The folder renamed at the source: the mirror moves it, and rewrites
	// the folder's own record only.
	if _, err := m.Apply(ctx, drive.SourceItem{Origin: drive.Origin{ID: "folder", Version: "1"}, Path: "/B/renamed", Type: drive.TypeDir}); err != nil {
		t.Fatal(err)
	}

	// restart is a drive opened again: no remembered paths.
	restart := func() *Drive { return newDrive(t, store, s, "") }
	lookup := func(d *Drive, id, hint string) (found, int64) {
		t.Helper()
		before := d.searches.Load()
		var f found
		if hint == "" {
			f = foundOf(d.FindByOrigin(ctx, srcDrive, id))
		} else {
			f = foundOf(d.FindByOriginHint(ctx, srcDrive, id, hint))
		}
		return f, d.searches.Load() - before
	}

	// With no hint, after a start: a walk.
	want, walks := lookup(restart(), "doc0", "")
	if want.path != "/B/renamed/doc0.txt" || walks != 1 {
		t.Fatalf("doc0 with no hint after a start: %+v, %d walks; want it at /B/renamed/doc0.txt after 1 walk", want, walks)
	}

	// A right hint: no walk, the same item, and its record corrected.
	got, walks := lookup(restart(), "doc1", "/B/renamed/doc1.txt")
	if got.path != "/B/renamed/doc1.txt" || got.origin != "doc1" || walks != 0 {
		t.Fatalf("doc1 told where it is: %+v, %d walks; want it, with no walk", got, walks)
	}
	if again, walks := lookup(restart(), "doc1", ""); again != got || walks != 0 {
		t.Errorf("doc1 after its hinted lookup, no hint, a new start: %+v, %d walks; want %+v and no walk (its record corrected)", again, walks, got)
	}

	// Hints that miss, after a start: each falls through to the walk, and
	// the answer is the one no hint gives.
	for _, c := range []struct{ id, hint, what string }{
		{"doc2", "/A/matter/doc2.txt", "stale (where it was)"},
		{"doc3", "/nowhere/doc3.txt", "wrong (nothing there)"},
		{"doc3", "/A/other.txt", "another item's path"},
		{"doc3", "/B/renamed", "its folder's path"},
	} {
		d := restart()
		noHint, _ := lookup(restart(), c.id, "")
		got, _ := lookup(d, c.id, c.hint)
		if got != noHint {
			t.Errorf("%s with a %s hint: %+v; with none: %+v", c.id, c.what, got, noHint)
		}
	}
	// The record a missed hint's walk corrected: no walk now.
	if _, walks := lookup(restart(), "doc2", ""); walks != 0 {
		t.Errorf("doc2 after a walk found it: %d walks, want 0", walks)
	}

	// A hint naming the item for another source id never answers for it.
	if got, _ := lookup(restart(), "never", "/A/other.txt"); got.err != "not found" {
		t.Errorf("an unknown id hinted at another item's path: %+v, want not found", got)
	}
	// Trashed: the hint at its old path misses, and the trash answers.
	if _, err := m.Remove(drive.WithPass(ctx, "p1"), "other"); err != nil {
		t.Fatal(err)
	}
	noHint, _ := lookup(restart(), "other", "")
	if got, walks := lookup(restart(), "other", "/A/other.txt"); got != noHint || got.trash == "" || walks != 0 {
		t.Errorf("a trashed item hinted at its old path: %+v (%d walks), with none %+v; want its trash entry, no walk", got, walks, noHint)
	}
}

// forget empties the memory, as a start of the process does.
func (c *idPathCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = nil
}
