package icbfsdrive

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// recContents is a ContentMap that isn't the in-memory one: it keeps
// its contents in a HashMap of its own, records the calls made of it,
// and fails a rebuild's adds with failAdd. versions is the drive's
// count of Versions calls (versionsCounter), read at each rebuild add.
type recContents struct {
	h        *drive.HashMap
	versions *versionsCounter
	failAdd  error

	mu                                  sync.Mutex
	lookups, adds, drops, rebuilds      int
	rebuildAdds, commits, abandons      int
	firstAddAt, lastVersionsBeforeEnd   int
	inRebuildAdd, concurrentRebuildAdds int
}

func (r *recContents) note(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *recContents) Lookup(ctx context.Context, sha string) (drive.HashEntry, bool, error) {
	r.note(func() { r.lookups++ })
	return r.h.ContentMap().Lookup(ctx, sha)
}

func (r *recContents) LookupSource(ctx context.Context, s drive.Hash) (string, drive.HashEntry, bool, error) {
	r.note(func() { r.lookups++ })
	return r.h.ContentMap().LookupSource(ctx, s)
}

func (r *recContents) Add(ctx context.Context, sha string, e drive.HashEntry) error {
	r.note(func() { r.adds++ })
	return r.h.ContentMap().Add(ctx, sha, e)
}

func (r *recContents) AddSource(ctx context.Context, s drive.Hash, sha string) error {
	r.note(func() { r.adds++ })
	return r.h.ContentMap().AddSource(ctx, s, sha)
}

func (r *recContents) Drop(ctx context.Context, sha string, e drive.HashEntry) error {
	r.note(func() { r.drops++ })
	return r.h.ContentMap().Drop(ctx, sha, e)
}

func (r *recContents) Count(ctx context.Context) (int, error) { return r.h.ContentMap().Count(ctx) }

func (r *recContents) Rebuild(ctx context.Context) (drive.ContentMapRebuild, error) {
	r.note(func() { r.rebuilds++ })
	inner, err := r.h.ContentMap().Rebuild(ctx)
	if err != nil {
		return nil, err
	}
	return &recRebuild{r: r, inner: inner}, nil
}

type recRebuild struct {
	r     *recContents
	inner drive.ContentMapRebuild
}

func (b *recRebuild) Add(ctx context.Context, sha string, e drive.HashEntry) error {
	r := b.r
	r.note(func() {
		if r.inRebuildAdd++; r.inRebuildAdd > 1 {
			r.concurrentRebuildAdds++
		}
		if r.rebuildAdds++; r.rebuildAdds == 1 {
			r.firstAddAt = r.versions.n()
		}
	})
	defer r.note(func() { r.inRebuildAdd-- })
	if r.failAdd != nil {
		return r.failAdd
	}
	return b.inner.Add(ctx, sha, e)
}

func (b *recRebuild) Commit(ctx context.Context) error {
	b.r.note(func() { b.r.commits++ })
	return b.inner.Commit(ctx)
}

func (b *recRebuild) Abandon() {
	b.r.note(func() { b.r.abandons++ })
	b.inner.Abandon()
}

// versionsCounter is a drive (and a Walker) that counts its Versions
// calls.
type versionsCounter struct {
	*Drive
	mu    sync.Mutex
	calls int
}

func (v *versionsCounter) Versions(ctx context.Context, ref drive.ItemRef) ([]drive.Version, error) {
	v.mu.Lock()
	v.calls++
	v.mu.Unlock()
	return v.Drive.Versions(ctx, ref)
}

func (v *versionsCounter) n() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

// TestMirrorContentMap: a mirror given a ContentMap that isn't the
// in-memory one (WithContentMap) uses it and nothing else: RebuildHashes
// streams the drive's versions into its rebuild as the walk goes (the
// first add before the last Versions call; never two adds at once) and
// commits it, building what a listing built; a write of known content
// looks it up and is a stub of it, then recorded. The mirror has no
// HashMap then (Hashes is nil; LoadHashes and SaveHashes are
// ErrInvalid). A rebuild whose add fails is abandoned, not committed,
// the map as it was.
func TestMirrorContentMap(t *testing.T) {
	_, d, files := rebuildTree(t, "contentmap")
	ctx := system()
	want, _, err := listRebuild(ctx, d, srcDrive, nil)
	if err != nil {
		t.Fatal(err)
	}
	vc := &versionsCounter{Drive: d}
	rc := &recContents{h: drive.NewHashMap(), versions: vc}
	m := drive.NewMirror(vc, srcDrive).WithContentMap(rc)
	if m.Hashes() != nil {
		t.Fatal("a mirror with another ContentMap has a HashMap")
	}
	if m.ContentMap() != drive.ContentMap(rc) {
		t.Fatal("ContentMap isn't the one given")
	}
	if err := m.WithHashStore(nil).LoadHashes(ctx); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("LoadHashes: %v, want ErrInvalid", err)
	}
	if err := m.RebuildHashes(ctx); err != nil {
		t.Fatal(err)
	}
	if rc.rebuilds != 1 || rc.commits != 1 || rc.adds != 0 {
		t.Fatalf("%d rebuilds, %d commits, %d adds outside the rebuild; want 1, 1, 0", rc.rebuilds, rc.commits, rc.adds)
	}
	if rc.firstAddAt >= files || vc.n() != files {
		t.Fatalf("the first add after %d of %d Versions calls (%d in all): the rebuild didn't stream", rc.firstAddAt, files, vc.n())
	}
	if rc.concurrentRebuildAdds != 0 {
		t.Fatalf("%d rebuild adds made while another was under way", rc.concurrentRebuildAdds)
	}
	if got := rc.h.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt %+v\nlisted %+v", got, want)
	}
	if n, err := m.ContentMap().Count(ctx); err != nil || n != len(want) {
		t.Fatalf("count %d %v, want %d", n, err, len(want))
	}

	// A new item with content the map knows: looked up, a stub.
	lookups := rc.lookups
	src := &source{}
	a := apply(t, ctx, m, "known content", src.item("n", "1", "/new.txt", mirrorData("e"), year(2024)), drive.Created)
	if rc.lookups == lookups || rc.adds == 0 {
		t.Fatalf("the write made %d lookups and %d adds", rc.lookups-lookups, rc.adds)
	}
	if a.Item.Checksums == nil || a.Item.Checksums.SHA256 != sha256Of(mirrorData("e")) {
		t.Fatalf("the new item %+v", a.Item)
	}

	// A rebuild whose add fails: abandoned, the map as it was.
	before := rc.h.Entries()
	boom := errors.New("boom")
	rc.failAdd = boom
	if err := m.RebuildHashes(ctx); !errors.Is(err, boom) {
		t.Fatalf("a failing add: %v, want boom", err)
	}
	if rc.commits != 1 || rc.abandons < 2 {
		t.Fatalf("%d commits, %d abandons; want the failed rebuild abandoned", rc.commits, rc.abandons)
	}
	if got := rc.h.Entries(); !reflect.DeepEqual(got, before) {
		t.Fatalf("after a failed rebuild %+v, want %+v", got, before)
	}
}
