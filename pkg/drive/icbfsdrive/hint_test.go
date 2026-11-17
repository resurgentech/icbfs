package icbfsdrive

import (
	"fmt"
	"sync"
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

// TestIDPathCacheBound (T82-13): the drive's memory of where items are
// holds at most its bound, drops the least recently used first (a get
// counts as a use), keeps a moved folder's remembered items under it,
// and is safe for use from many goroutines.
func TestIDPathCacheBound(t *testing.T) {
	c := idPathCache{max: 8}
	for i := range 8 {
		c.put(fmt.Sprint("id", i), fmt.Sprintf("/d/f%d", i))
	}
	if _, ok := c.get("id0"); !ok { // id0 used: id1 is now the oldest
		t.Fatal("id0 is not remembered")
	}
	c.put("id8", "/d/f8")
	c.put("id9", "/d/f9")
	if n := c.len(); n != 8 {
		t.Errorf("%d entries, want the bound, 8", n)
	}
	for id, want := range map[string]bool{"id0": true, "id1": false, "id2": false, "id3": true, "id8": true, "id9": true} {
		if _, ok := c.get(id); ok != want {
			t.Errorf("%s remembered: %v, want %v", id, ok, want)
		}
	}
	c.put("id3", "/d/f3b") // a put over an entry replaces it, no growth
	if p, _ := c.get("id3"); p != "/d/f3b" || c.len() != 8 {
		t.Errorf("id3 put again: %q, %d entries", p, c.len())
	}
	c.moved("dir", "/d", "/e", true)
	if p, _ := c.get("id9"); p != "/e/f9" {
		t.Errorf("id9 after its folder moved: %q, want /e/f9", p)
	}
	if p, _ := c.get("dir"); p != "/e" || c.len() != 8 {
		t.Errorf("the moved folder: %q, %d entries", p, c.len())
	}

	// The default bound, and many goroutines at once (go test -race).
	var d idPathCache
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2 * idPathEntries {
				id := fmt.Sprintf("g%d-%d", g, i)
				d.put(id, "/x/"+id)
				d.get(fmt.Sprintf("g%d-%d", g, i/2))
				if i%500 == 0 {
					d.moved(fmt.Sprint("dir", g), "/x", "/x", true)
				}
			}
		}()
	}
	wg.Wait()
	if n := d.len(); n != idPathEntries {
		t.Errorf("after %d puts: %d entries, want the bound %d", 16*idPathEntries, n, idPathEntries)
	}
}

// forget empties the memory, as a start of the process does.
func (c *idPathCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = nil
	c.lru.Init()
}

// TestStubTargetPlaceAfterRestart (T82-13, option 2): a mirror that
// writes a document as a stub of content it already holds tells the drive
// where the earlier item is (drive.Ref.Path, from the content map's
// recorded place): a drive that remembers no paths (a start, or an entry
// its bounded memory dropped) finds the target without a walk. A place
// that is stale (the target moved), wrong, or names another item costs
// one resolve and falls through to today's lookup; a target trashed since
// is refused, and the mirror writes the bytes: never a dangling stub.
func TestStubTargetPlaceAfterRestart(t *testing.T) {
	store := miniotest.Store(t)
	s := icbfs.Settings{Name: "stubplace", Size: 1 << 30, RootMode: 0o777}
	ctx := system()
	d := newDrive(t, store, s, "")
	hashes := drive.NewHashMap()
	src := &source{}
	x, y := mirrorData("x"), mirrorData("y")
	m := drive.NewMirror(d, srcDrive).WithContentMap(hashes.ContentMap())
	a := apply(t, ctx, m, "first", src.item("a", "1", "/m/a.txt", x, time.Time{}), drive.Created)
	if e, ok := hashes.Lookup(sha256Of(x)); !ok || e.Path != "/m/a.txt" {
		t.Fatalf("the map's entry for a's bytes: %+v %v; want its place /m/a.txt", e, ok)
	}

	// A start: a new drive (no remembered paths) and a new mirror over the
	// same map. The twin is a stub of a, found at its place: no walk.
	d2 := newDrive(t, store, s, "")
	m2 := drive.NewMirror(d2, srcDrive).WithContentMap(hashes.ContentMap())
	b := apply(t, ctx, m2, "twin after a start", src.item("b", "1", "/n/b.txt", x, time.Time{}), drive.Created)
	stubOf(t, store, b.Item.ID, b.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := d2.Walks(); n != 0 {
		t.Errorf("the twin after a start walked the tree %d times; want none", n)
	}

	// The target moved since its place was recorded: the stale place
	// falls through to the walk, and the stub is still a's.
	if _, err := d2.Move(ctx, drive.ByPath("/m/a.txt"), "/a-moved.txt", false); err != nil {
		t.Fatal(err)
	}
	d3 := newDrive(t, store, s, "")
	m3 := drive.NewMirror(d3, srcDrive).WithContentMap(hashes.ContentMap())
	c := apply(t, ctx, m3, "twin of a moved target", src.item("c", "1", "/n/c.txt", x, time.Time{}), drive.Created)
	stubOf(t, store, c.Item.ID, c.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := d3.Walks(); n != 1 {
		t.Errorf("a stale place: %d walks; want the one today's lookup makes", n)
	}

	// A place naming another item (y's) never makes the stub y's.
	ya := apply(t, ctx, m3, "other bytes", src.item("ya", "1", "/n/ya.txt", y, time.Time{}), drive.Created)
	e, _ := hashes.Lookup(sha256Of(x))
	e.Path = ya.Item.Path
	hashes.Drop(sha256Of(x), e)
	hashes.Add(sha256Of(x), e)
	if got, _ := hashes.Lookup(sha256Of(x)); got.Path != ya.Item.Path {
		t.Fatalf("the map didn't take the planted place: %+v", got)
	}
	d4 := newDrive(t, store, s, "")
	m4 := drive.NewMirror(d4, srcDrive).WithContentMap(hashes.ContentMap())
	f := apply(t, ctx, m4, "twin with a place naming another item", src.item("f", "1", "/n/f.txt", x, time.Time{}), drive.Created)
	stubOf(t, store, f.Item.ID, f.Item.Version.ID, a.Item.ID, a.Item.Version.ID)

	// The target trashed: refused, the bytes written, never a dangling stub.
	if err := d4.Delete(ctx, drive.ByPath("/a-moved.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if err := d4.Delete(ctx, drive.ByPath("/n/b.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/n/c.txt", "/n/f.txt"} {
		if err := d4.Delete(ctx, drive.ByPath(p), false, drive.Condition{}); err != nil {
			t.Fatal(err)
		}
	}
	d5 := newDrive(t, store, s, "")
	m5 := drive.NewMirror(d5, srcDrive).WithContentMap(hashes.ContentMap())
	g := apply(t, ctx, m5, "twin of trashed targets", src.item("g", "1", "/n/g.txt", x, time.Time{}), drive.Created)
	notStub(t, store, g.Item.ID, g.Item.Version.ID)
	if got := read(t, ctx, d5, drive.ByPath("/n/g.txt"), drive.At{}); got != x {
		t.Fatalf("the twin of a trashed target reads %q", got[:min(len(got), 30)])
	}
}

// TestStubTargetPlaceLearned (T82-13, option 2): an entry with no place
// (a content map from before places were recorded) costs one walk at the
// first stub of it, and the place found is written back to the map, so
// the next stub of it, after another start, makes none.
func TestStubTargetPlaceLearned(t *testing.T) {
	store := miniotest.Store(t)
	s := icbfs.Settings{Name: "stubplace-learn", Size: 1 << 30, RootMode: 0o777}
	ctx := system()
	d := newDrive(t, store, s, "")
	hashes := drive.NewHashMap()
	src := &source{}
	x := mirrorData("x")
	m := drive.NewMirror(d, srcDrive).WithContentMap(hashes.ContentMap())
	a := apply(t, ctx, m, "first", src.item("a", "1", "/m/a.txt", x, time.Time{}), drive.Created)
	// As a map from before places: the entry has none.
	old, _ := hashes.Lookup(sha256Of(x))
	hashes.Drop(sha256Of(x), old)
	hashes.Add(sha256Of(x), drive.HashEntry{ID: old.ID, Version: old.Version, Size: old.Size})

	d2 := newDrive(t, store, s, "")
	m2 := drive.NewMirror(d2, srcDrive).WithContentMap(hashes.ContentMap())
	b := apply(t, ctx, m2, "twin, no place", src.item("b", "1", "/n/b.txt", x, time.Time{}), drive.Created)
	stubOf(t, store, b.Item.ID, b.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := d2.Walks(); n != 1 {
		t.Errorf("the first stub of an entry with no place: %d walks; want 1", n)
	}
	if e, _ := hashes.Lookup(sha256Of(x)); e.Path != "/m/a.txt" || e.ID != a.Item.ID {
		t.Fatalf("the place wasn't learned: %+v", e)
	}
	d3 := newDrive(t, store, s, "")
	m3 := drive.NewMirror(d3, srcDrive).WithContentMap(hashes.ContentMap())
	c := apply(t, ctx, m3, "twin, place learned", src.item("c", "1", "/n/c.txt", x, time.Time{}), drive.Created)
	stubOf(t, store, c.Item.ID, c.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := d3.Walks(); n != 0 {
		t.Errorf("after the place was learned: %d walks; want none", n)
	}
}
