package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// rebuildSample is the heap in use part way through a rebuild of a
// mirror's hash map: after files Versions calls had returned, with
// entries distinct SHA-256s among the versions they gave.
type rebuildSample struct {
	files, entries int
	heap           uint64
}

// versionsSampler is a drive (and a Walker: *Drive's Walk) whose
// Versions samples the heap every sampleEvery calls, and counts the
// distinct contents seen so far: the entries the map being rebuilt has
// by then. index numbers the tree's contents by SHA-256.
//
// A sample over limit, less entries of at most overEntry bytes each
// (far more than one costs), cancels the rebuild with errOver: a rebuild
// that holds the tree is seen to at its first samples, not hours later.
type versionsSampler struct {
	*Drive
	index  map[string]int
	limit  int64
	cancel context.CancelCauseFunc
	start  time.Time
	logf   func(string, ...any)
	over   *rebuildSample
	two    map[string]bool // the paths whose Versions the drive answers

	mu       sync.Mutex
	calls    int
	seen     []bool
	distinct int
	samples  []rebuildSample
}

const sampleEvery = 100

// versions is the drive's Versions for a file with two versions (two),
// and for the rest, which have one, that one version from a Stat. A
// history listing on MinIO costs tens of milliseconds of its CPU with
// 60,000 objects in the bucket, so 29,700 of them took over half an
// hour; a Stat is a read of one object. What this test measures is what
// the rebuild holds between calls, which is the same either way; the
// drive's own Versions feeds the rebuild in TestRebuildHashesAsListed
// and for the 891 files here that have a history.
func (s *versionsSampler) versions(ctx context.Context, ref drive.ItemRef) ([]drive.Version, error) {
	if s.two[ref.Path] {
		return s.Drive.Versions(ctx, ref)
	}
	it, err := s.Drive.Stat(ctx, ref, drive.At{})
	if err != nil {
		return nil, err
	}
	if it.Version == nil {
		return nil, nil
	}
	v := *it.Version
	v.Current = true
	if v.Checksums == nil {
		v.Checksums = it.Checksums
	}
	return []drive.Version{v}, nil
}

func (s *versionsSampler) Versions(ctx context.Context, ref drive.ItemRef) ([]drive.Version, error) {
	vs, err := s.versions(ctx, ref)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range vs {
		if v.Checksums == nil {
			continue
		}
		if i, ok := s.index[v.Checksums.SHA256]; ok && !s.seen[i] {
			s.seen[i] = true
			s.distinct++
		}
	}
	s.calls++
	if s.calls%sampleEvery == 0 {
		sm := rebuildSample{s.calls, s.distinct, heapInUse()}
		s.samples = append(s.samples, sm)
		if s.over == nil && int64(sm.heap)-overEntry*int64(sm.entries) > s.limit {
			s.over = &sm
			s.cancel(errOver)
		}
	}
	if s.calls%3000 == 0 {
		s.logf("%d files' versions read in %v", s.calls, time.Since(s.start).Round(time.Second))
	}
	return vs, err
}

// overEntry is more than a map entry could cost: hashMapCost measured
// about 250 bytes.
const overEntry = 1 << 10

var errOver = errors.New("the heap is over the bound")

// hashMapCost is the heap a HashMap of entries takes, built alone, its
// strings its own: what the map a rebuild makes costs, entries and all.
func hashMapCost(entries map[string]drive.HashEntry) uint64 {
	before := heapInUse()
	h := drive.NewHashMap()
	for sha, e := range entries {
		h.Add(strings.Clone(sha), drive.HashEntry{ID: strings.Clone(e.ID), Version: strings.Clone(e.Version), Size: e.Size})
	}
	after := heapInUse()
	if len(h.Entries()) != len(entries) {
		panic("a map of other entries")
	}
	if after < before {
		return 0
	}
	return after - before
}

// rebuildMemory: a mirror's rebuild of its hash map from the tree (every
// file the mirror's, 1.03 versions a file, 2 contents in 5 another's)
// holds one folder at a time besides the map it builds. The heap in
// use, sampled in Versions, less what the map's entries so far cost
// (hashMapCost of the finished map, by the entry), stays under the
// bound a walk has (walkMemory's) and rebuildInFlight for its eight
// calls at once, and is no higher in the last third of the rebuild than
// in the first.
func rebuildMemory(t *testing.T, d *Drive, tr tree) {
	s := &versionsSampler{Drive: d, two: tr.two, index: make(map[string]int, len(tr.shas)), seen: make([]bool, len(tr.shas)),
		samples: make([]rebuildSample, 0, tr.versions/sampleEvery+1)}
	for i, sha := range tr.shas {
		s.index[sha] = i
	}
	m := drive.NewMirror(s, srcDrive)
	ctx, cancel := context.WithCancelCause(system())
	defer cancel(nil)
	base := heapInUse()
	bound := int64(base + walkBase + rebuildInFlight + walkPerItem*treeBig)
	s.limit, s.cancel, s.logf = bound+4<<20, cancel, t.Logf
	start := time.Now()
	s.start = start
	err := m.RebuildHashes(ctx)
	if s.over != nil {
		t.Fatalf("after %d files' versions, in %v, with %d entries in the map, the heap in use was %s: over %s (the bound %s: %s + %s + %d KiB x %d items, and 4 MiB) with %d KiB an entry taken off; stopped (%v)",
			s.over.files, time.Since(start).Round(time.Second), s.over.entries, mib(s.over.heap), mib(s.limit), mib(bound), mib(base),
			mib(uint64(walkBase)), walkPerItem>>10, treeBig, overEntry>>10, err)
	}
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start).Round(time.Second)
	entries := m.Hashes().Entries()
	if len(entries) != len(tr.shas) || s.calls != treeFiles || s.distinct != len(tr.shas) {
		t.Fatalf("rebuilt %d entries from %d files (%d contents seen); want %d from %d", len(entries), s.calls, s.distinct, len(tr.shas), treeFiles)
	}
	cost := hashMapCost(entries)
	perEntry := float64(cost) / float64(len(entries))
	var peak, rawPeak, first, last int64
	peak, first, last = -1<<62, -1<<62, -1<<62
	for _, sm := range s.samples {
		held := int64(sm.heap) - int64(perEntry*float64(sm.entries))
		rawPeak = max(rawPeak, int64(sm.heap))
		peak = max(peak, held)
		switch {
		case sm.files < treeFiles/3:
			first = max(first, held)
		case sm.files >= 2*treeFiles/3:
			last = max(last, held)
		}
	}
	t.Logf("rebuilt %d entries from %d files in %v; heap in use before %s; the map %s (%.0f bytes an entry); "+
		"peak %s, less the map's entries so far %s (bound %s: %s + %s + %d KiB x %d items); first third %s, last third %s",
		len(entries), s.calls, took, mib(base), mib(cost), perEntry, mib(rawPeak), mib(peak), mib(bound), mib(base),
		mib(uint64(walkBase)), walkPerItem>>10, treeBig, mib(first), mib(last))
	if peak > bound {
		t.Errorf("heap in use, less the map's entries, reached %s in the rebuild, over the bound %s", mib(peak), mib(bound))
	}
	// 1 MiB for what a collection leaves at random (partly used spans),
	// and the map's own cost again: its entries are taken off at what
	// they cost in a map built alone, and in the rebuild's they cost
	// more (their strings are the drive's, in its size classes) and
	// arrive in the table's steps. Measured drift: 0.9 MiB, and 2.3 MiB
	// under the race detector, with a map of 3.2 MiB. A rebuild that
	// kept the tree drifts by tens of MiB, and is far over the bound
	// from its first call.
	if last > first+1<<20+int64(cost) {
		t.Errorf("heap in use, less the map's entries, in the last third %s, over the first third's %s", mib(last), mib(first))
	}
}

// listRebuild is RebuildHashes as it was before it walked: the whole
// tree in one listing, then each of the source's files' versions, one
// after another. kept is the map's source hashes before the rebuild.
func listRebuild(ctx context.Context, d drive.Drive, source string, kept map[drive.Hash]string) (map[string]drive.HashEntry, map[drive.Hash]string, error) {
	h := drive.NewHashMap()
	for cursor := ""; ; {
		page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{Depth: 1 << 30, Cursor: cursor})
		if err != nil {
			return nil, nil, err
		}
		for _, it := range page.Items {
			if it.Type != drive.TypeFile || it.Origin == nil || it.Origin.Drive != source {
				continue
			}
			vs, err := d.Versions(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})
			if errors.Is(err, drive.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("versions of %s: %w", it.Path, err)
			}
			for _, v := range vs {
				if v.Checksums != nil && v.Checksums.SHA256 != "" {
					h.Add(v.Checksums.SHA256, drive.HashEntry{ID: it.ID, Version: v.ID, Size: v.Size})
				}
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	sha := h.Entries()
	src := map[drive.Hash]string{}
	for s, v := range kept {
		if _, ok := sha[v]; ok {
			src[s] = v
		}
	}
	return sha, src, nil
}

// rebuildTree is a small mirror for the rebuild's tests: files of the
// source in folders three deep, some with several versions, twins
// within a folder and across folders, one hashed by the source, one with
// a version with no checksum recorded, a file another source's mirror
// wrote, one made on the drive, and a folder of 20 of the source's files.
// It's the mirror, its drive, and how many files of the source it has.
func rebuildTree(t *testing.T, name string) (*drive.Mirror, *Drive, int) {
	t.Helper()
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777}, "")
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
	x, y, z := mirrorData("x"), mirrorData("y"), mirrorData("z")
	files := 0
	add := func(id, version, p, body string, at time.Duration, want ...drive.Action) {
		t.Helper()
		apply(t, ctx, m, id+" "+version, src.item(id, version, p, body, jan.Add(at)), want...)
	}
	add("a", "1", "/a.txt", x, 0, drive.Created)
	add("a", "2", "/a.txt", y, time.Hour, drive.Updated)
	add("a", "3", "/a.txt", y, 2*time.Hour, drive.Updated)
	add("b", "1", "/one/b.txt", x, 0, drive.Created)
	add("c", "1", "/one/two/c.txt", z, 0, drive.Created)
	add("c", "2", "/one/two/c.txt", z+"2", time.Hour, drive.Updated)
	add("d", "1", "/one/two/three/d.txt", z, 0, drive.Created)
	add("e", "1", "/one/two/three/e.txt", mirrorData("e"), 0, drive.Created)
	add("f", "1", "/one/two/three/f.txt", mirrorData("e"), 0, drive.Created)
	apply(t, ctx, m, "g, hashed", src.hashed("g", "1", "/other/g.txt", mirrorData("g"), jan), drive.Created)
	files += 7
	for i := range 20 {
		add(fmt.Sprintf("w%02d", i), "1", fmt.Sprintf("/wide/w%02d.txt", i), mirrorData(fmt.Sprint("w", i%7)), 0, drive.Created)
	}
	files += 20
	// A version with no checksum recorded: skipped.
	cur, err := d.Stat(ctx, drive.ByPath("/other/g.txt"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), cur.ID, strings.NewReader("no checksum recorded"), nil, ""); err != nil {
		t.Fatal(err)
	}
	// Another source's file, with content of the source's, and one of
	// the drive's own: neither in the map.
	om := drive.NewMirror(d, "webdav:other")
	apply(t, ctx, om, "another source's", src.item("o", "1", "/one/o.txt", mirrorData("other"), jan), drive.Created)
	write(t, ctx, d, "/one/mine.txt", mirrorData("mine"), drive.Condition{})
	return m, d, files
}

// TestRebuildHashesAsListed: the map RebuildHashes builds by walking the
// drive is the one a listing of the whole drive built (listRebuild), its
// source hashes too, on a Walker and on a drive that isn't one.
func TestRebuildHashesAsListed(t *testing.T) {
	m, idrv, _ := rebuildTree(t, "rebuild-listed")
	ctx := system()
	// A source hash whose SHA-256 the drive doesn't have: dropped.
	m.Hashes().AddSource(drive.Hash{Kind: "md5", Value: "feed"}, sha256Of("nowhere"))
	kept := m.Hashes().SourceEntries()
	if len(kept) != 2 {
		t.Fatalf("source hashes %+v, want g's and the stale one", kept)
	}
	wantSHA, wantSrc, err := listRebuild(ctx, idrv, srcDrive, kept)
	if err != nil {
		t.Fatal(err)
	}
	if len(wantSHA) != 13 || len(wantSrc) != 1 {
		t.Fatalf("the listing's map has %d entries and %d source hashes, want 13 and 1: %+v %+v", len(wantSHA), len(wantSrc), wantSHA, wantSrc)
	}
	for _, d := range []struct {
		name string
		d    drive.Drive
	}{{"a Walker", idrv}, {"not a Walker", listOnly{idrv}}} {
		fresh := drive.NewMirror(d.d, srcDrive)
		for s, sha := range kept {
			fresh.Hashes().AddSource(s, sha)
		}
		if err := fresh.RebuildHashes(ctx); err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		if got := fresh.Hashes().Entries(); !reflect.DeepEqual(got, wantSHA) {
			t.Fatalf("%s: walked %+v\nlisted %+v", d.name, got, wantSHA)
		}
		if got := fresh.Hashes().SourceEntries(); !reflect.DeepEqual(got, wantSrc) {
			t.Fatalf("%s: source hashes %+v, listed %+v", d.name, got, wantSrc)
		}
	}
}
