package icbfs

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// putStub writes, by hand, a stub as key's new current content version:
// a RefRecord in layout naming targetKey's version targetVersion of size
// bytes, with the stub's metadata names.
func putStub(t *testing.T, ctx context.Context, store objstore.Store, key, targetKey, targetVersion string, size int64, layout uint32) *objstore.Object {
	t.Helper()
	body := mustMarshal(t, &pb.RefRecord{Layout: layout, Key: targetKey, VersionId: targetVersion, Size: size})
	obj, err := store.Put(ctx, key, bytes.NewReader(body), map[string]string{
		MetaRef: "1", MetaRefKey: targetKey, MetaRefVersion: targetVersion, MetaRefSize: strconv.FormatInt(size, 10),
	}, "")
	if err != nil {
		t.Fatalf("put stub on %s: %v", key, err)
	}
	return obj
}

// stubFixture is a filesystem with a target file and a file a whose
// content is a stub naming the target's current version.
type stubFixture struct {
	fsys       *Filesystem
	store      objstore.Store
	target     string
	targetData []byte
	targetVer  *objstore.Object
	a          string
	stub       *objstore.Object
}

func newStubFixture(t *testing.T) *stubFixture {
	t.Helper()
	ctx := context.Background()
	fsys, store := newTestFilesystem(t)
	root := fsys.RootKey()
	data := []byte(strings.Repeat("the target's bytes, much longer than a stub. ", 40))
	x := &stubFixture{fsys: fsys, store: store, targetData: data}
	x.target = writeNew(t, ctx, fsys, root, "target", string(data))
	var err error
	if x.targetVer, err = store.Head(ctx, x.target); err != nil {
		t.Fatal(err)
	}
	x.a = writeNew(t, ctx, fsys, root, "a", "before the stub")
	x.stub = putStub(t, ctx, store, x.a, x.target, x.targetVer.VersionID, int64(len(data)), refLayout)
	if x.stub.Size >= int64(len(data)) {
		t.Fatalf("stub is %d bytes, not smaller than its target's %d", x.stub.Size, len(data))
	}
	return x
}

// TestStubReadsThrough: a file whose content is a stub opens, reads and
// stats as its target: the target's bytes and size, with the stub's ETag
// for conditional writes, and a write through it lands as real bytes. A
// part is copied from the target, never from the stub's own bytes, and a trash entry has
// the real size.
func TestStubReadsThrough(t *testing.T) {
	x := newStubFixture(t)
	ctx := context.Background()
	f, n := x.fsys, int64(len(x.targetData))

	attr, err := f.Stat(ctx, x.a, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if attr.Size != n || attr.ContentETag() != x.stub.ETag {
		t.Fatalf("stat: size %d etag %s, want %d %s", attr.Size, attr.ContentETag(), n, x.stub.ETag)
	}
	if cv, _ := attr.ContentVersion(); cv.Size != n || cv.VersionID != x.stub.VersionID {
		t.Fatalf("content version %+v, want the stub's with size %d", cv, n)
	}

	data, rattr, etag, err := f.ReadFile(ctx, x.a)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(data, x.targetData) || etag != x.stub.ETag || rattr.Size != n {
		t.Fatalf("read file: %d bytes %q..., etag %s size %d; want the target's %d, etag %s", len(data), data[:min(len(data), 20)], etag, rattr.Size, n, x.stub.ETag)
	}

	o, err := f.Open(ctx, x.a)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := o.ReadAt(0, n+10); !bytes.Equal(got, x.targetData) {
		t.Fatalf("open read %d bytes %q..., want the target's", len(got), got[:min(len(got), 20)])
	}
	o.c.mu.Lock()
	cEtag, cBaseVer, cBaseKey := o.c.etag, o.c.baseVersion, o.c.baseKey
	parts := o.c.partsLocked(64)
	o.c.mu.Unlock()
	if cEtag != x.stub.ETag || cBaseVer != x.targetVer.VersionID || cBaseKey != x.target {
		t.Fatalf("content etag %s base %q key %s; want the stub's etag, the target's version %s and key %s", cEtag, cBaseVer, cBaseKey, x.targetVer.VersionID, x.target)
	}
	for i, p := range parts {
		if p.Body == nil && (p.CopyKey != x.target || p.CopyVersion != x.targetVer.VersionID) {
			t.Fatalf("part %d copies %q version %s from a stub base; want the target's", i, p.CopyKey, p.CopyVersion)
		}
	}
	if err := o.WriteAt([]byte("EDIT"), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush over a stub (If-Match its ETag): %v", err)
	}
	o.Close()
	want := append([]byte(nil), x.targetData...)
	copy(want[3:], "EDIT")
	if data, _, _, err := f.ReadFile(ctx, x.a); err != nil || !bytes.Equal(data, want) {
		t.Fatalf("after the write: %d bytes, %v", len(data), err)
	}
	if data, _, _, err := f.ReadFile(ctx, x.target); err != nil || !bytes.Equal(data, x.targetData) {
		t.Fatalf("the target changed: %v", err)
	}

	// A trashed stub: the entry has the real size.
	b := writeNew(t, ctx, f, f.RootKey(), "b", "x")
	putStub(t, ctx, x.store, b, x.target, x.targetVer.VersionID, n, refLayout)
	if err := f.Unlink(ctx, f.RootKey(), "b"); err != nil {
		t.Fatal(err)
	}
	trash, err := f.ListTrash(ctx, TrashOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range trash {
		if e.ItemID == b {
			found = true
			if e.Size != n {
				t.Fatalf("trash entry size %d, want %d", e.Size, n)
			}
		}
	}
	if !found {
		t.Fatalf("no trash entry for b: %+v", trash)
	}
}

// TestStubRebase: content already open is brought up to a stub written
// elsewhere, from the stub's target, and a flush that loses the race to a
// stub rebases its edits onto the target's bytes.
func TestStubRebase(t *testing.T) {
	x := newStubFixture(t)
	ctx := context.Background()
	f, n := x.fsys, int64(len(x.targetData))
	c := writeNew(t, ctx, f, f.RootKey(), "c", strings.Repeat("-", int(n)))

	// Clean: the next open rebases.
	held, err := f.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	stub := putStub(t, ctx, x.store, c, x.target, x.targetVer.VersionID, n, refLayout)
	o, err := f.Open(ctx, c)
	if err != nil {
		t.Fatalf("open after a stub: %v", err)
	}
	if got := o.ReadAt(0, n); !bytes.Equal(got, x.targetData) {
		t.Fatalf("rebased onto a stub: %q...", got[:min(len(got), 20)])
	}
	o.c.mu.Lock()
	if o.c.etag != stub.ETag || o.c.baseVersion != x.targetVer.VersionID || o.c.baseKey != x.target || o.c.size != n {
		t.Errorf("rebased content etag %s base %q key %s size %d", o.c.etag, o.c.baseVersion, o.c.baseKey, o.c.size)
	}
	o.c.mu.Unlock()
	o.Close()
	held.Close()

	// Dirty: a lost race rebases the edit onto the target.
	d := writeNew(t, ctx, f, f.RootKey(), "d", strings.Repeat("-", int(n)))
	o, err = f.Open(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.WriteAt([]byte("Z"), 2); err != nil {
		t.Fatal(err)
	}
	putStub(t, ctx, x.store, d, x.target, x.targetVer.VersionID, n, refLayout)
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush after a stub: %v", err)
	}
	o.Close()
	want := append([]byte(nil), x.targetData...)
	want[2] = 'Z'
	if data, _, _, err := f.ReadFile(ctx, d); err != nil || !bytes.Equal(data, want) {
		t.Fatalf("after the rebase: %q..., %v", data[:min(len(data), 20)], err)
	}
}

// TestStubAsOf: a view as of a time the stub was current reads and stats
// through it.
func TestStubAsOf(t *testing.T) {
	x := newStubFixture(t)
	ctx := context.Background()
	n := int64(len(x.targetData))
	time.Sleep(1100 * time.Millisecond)
	at, err := x.store.ServerTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := x.fsys.WriteFile(ctx, x.a, []byte("after"), ""); err != nil {
		t.Fatal(err)
	}

	hist, err := objstore.OpenTempHistoryCache(ctx, x.store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	view, err := objstore.NewAsOf(ctx, hist, at)
	if err != nil {
		t.Fatal(err)
	}
	past := New(view, "test")
	if err := past.BootstrapReadOnly(ctx); err != nil {
		t.Fatal(err)
	}
	if attr, err := past.Stat(ctx, x.a, TypeFile); err != nil || attr.Size != n || attr.ContentETag() != x.stub.ETag {
		t.Fatalf("as-of stat: size %d etag %s, %v; want %d %s", attr.Size, attr.ContentETag(), err, n, x.stub.ETag)
	}
	if data, _, _, err := past.ReadFile(ctx, x.a); err != nil || !bytes.Equal(data, x.targetData) {
		t.Fatalf("as-of read file: %d bytes, %v", len(data), err)
	}
	o, err := past.Open(ctx, x.a)
	if err != nil {
		t.Fatalf("as-of open: %v", err)
	}
	if got := o.ReadAt(0, n); !bytes.Equal(got, x.targetData) {
		t.Fatalf("as-of open read: %q...", got[:min(len(got), 20)])
	}
	o.Close()
	want, _ := ChecksumsOf(bytes.NewReader(x.targetData))
	if c, err := past.VersionChecksums(ctx, x.a, ""); err != nil || !bytes.Equal(c.SHA256, want.SHA256) {
		t.Fatalf("as-of checksums %x, %v; want %x", c.SHA256, err, want.SHA256)
	}
}

// TestStubChecksums: a stub's checksums are its target's bytes', recorded
// under the stub's ETag.
func TestStubChecksums(t *testing.T) {
	x := newStubFixture(t)
	ctx := context.Background()
	want, _ := ChecksumsOf(bytes.NewReader(x.targetData))
	for _, v := range []string{"", x.stub.VersionID} {
		c, err := x.fsys.VersionChecksums(ctx, x.a, v)
		if err != nil || !bytes.Equal(c.MD5, want.MD5) || !bytes.Equal(c.SHA256, want.SHA256) {
			t.Fatalf("checksums of version %q: %x, %v; want %x", v, c.SHA256, err, want.SHA256)
		}
	}
	attr, err := x.fsys.Stat(ctx, x.a, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if c := attr.Checksums[x.stub.ETag]; !bytes.Equal(c.SHA256, want.SHA256) {
		t.Fatalf("recorded under the stub's ETag: %x, want %x (all: %v)", c.SHA256, want.SHA256, attr.Checksums)
	}
	if c, ok := attr.ContentChecksums(); !ok || !bytes.Equal(c.SHA256, want.SHA256) {
		t.Fatalf("content checksums %x, %v", c.SHA256, ok)
	}
}

// TestStubRefusals: a stub to a stub, and a stub in a layout this build
// doesn't read, are errors, never bytes.
func TestStubRefusals(t *testing.T) {
	x := newStubFixture(t)
	ctx := context.Background()
	f, n := x.fsys, int64(len(x.targetData))
	refused := func(name, key string, want error) {
		t.Helper()
		if data, _, _, err := f.ReadFile(ctx, key); !errors.Is(err, want) {
			t.Errorf("%s: read file = %d bytes, %v; want %v", name, len(data), err, want)
		}
		if o, err := f.Open(ctx, key); !errors.Is(err, want) {
			if err == nil {
				o.Close()
			}
			t.Errorf("%s: open = %v, want %v", name, err, want)
		}
		if _, err := f.VersionChecksums(ctx, key, ""); !errors.Is(err, want) {
			t.Errorf("%s: checksums = %v, want %v", name, err, want)
		}
	}

	chain := writeNew(t, ctx, f, f.RootKey(), "chain", "x")
	putStub(t, ctx, x.store, chain, x.a, x.stub.VersionID, n, refLayout)
	refused("a stub to a stub", chain, ErrRefToRef)

	newer := writeNew(t, ctx, f, f.RootKey(), "newer", "x")
	putStub(t, ctx, x.store, newer, x.target, x.targetVer.VersionID, n, refLayout+1)
	refused("a newer layout", newer, ErrNewerFormat)

	unknown := writeNew(t, ctx, f, f.RootKey(), "unknown", "x")
	putStub(t, ctx, x.store, unknown, x.target, x.targetVer.VersionID, n, 0)
	refused("layout 0", unknown, ErrUnknownFormat)
}

// TestStubTargetTrashed: a stub still reads by version when its target's
// item was deleted (trashed).
func TestStubTargetTrashed(t *testing.T) {
	x := newStubFixture(t)
	ctx := context.Background()
	if err := x.fsys.Unlink(ctx, x.fsys.RootKey(), "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.store.Head(ctx, x.target); !objstore.IsNotFound(err) {
		t.Fatalf("target still current: %v", err)
	}
	if data, _, _, err := x.fsys.ReadFile(ctx, x.a); err != nil || !bytes.Equal(data, x.targetData) {
		t.Fatalf("read through a trashed target: %d bytes, %v", len(data), err)
	}
	o, err := x.fsys.Open(ctx, x.a)
	if err != nil {
		t.Fatalf("open through a trashed target: %v", err)
	}
	defer o.Close()
	if got := o.ReadAt(0, int64(len(x.targetData))); !bytes.Equal(got, x.targetData) {
		t.Fatalf("open read through a trashed target: %q...", got[:min(len(got), 20)])
	}
}

// TestStubFileHistory: a file's history reports a stub's real size and
// target, from .metadata's Refs, and .metadata writes keep Refs.
func TestStubFileHistory(t *testing.T) {
	x := newStubFixture(t)
	ctx := context.Background()
	n := int64(len(x.targetData))
	ref := Ref{Key: x.target, VersionID: x.targetVer.VersionID, Size: n}
	if _, err := x.fsys.updateFileMetadata(ctx, x.a, func(a *Attr) { a.Refs = map[string]Ref{x.stub.ETag: ref} }); err != nil {
		t.Fatal(err)
	}
	// Another .metadata write keeps them.
	mode := uint32(0600)
	if _, err := x.fsys.SetAttr(ctx, x.a, TypeFile, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	attr, err := x.fsys.Stat(ctx, x.a, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if attr.Refs[x.stub.ETag] != ref {
		t.Fatalf("refs after a chmod: %+v", attr.Refs)
	}
	history, current, err := x.fsys.FileHistory(ctx, x.a)
	if err != nil {
		t.Fatal(err)
	}
	if current != x.stub.VersionID || len(history) < 2 {
		t.Fatalf("history %+v, current %s", history, current)
	}
	for _, v := range history {
		if v.VersionID == x.stub.VersionID {
			if v.Size != n || v.Ref == nil || *v.Ref != ref {
				t.Fatalf("stub version: size %d ref %+v; want %d %+v", v.Size, v.Ref, n, ref)
			}
		} else if v.Ref != nil || v.Size > int64(len("before the stub")) {
			t.Fatalf("plain version: %+v", v)
		}
	}
}

// TestFormatVersions: a filesystem of format 2 (one that may hold stubs)
// opens; 3 is newer than this build reads. New filesystems are still 1.
func TestFormatVersions(t *testing.T) {
	_, store := newTestFilesystem(t)
	ctx := context.Background()
	info, err := DescribeFilesystem(ctx, store, "test")
	if err != nil || info.FormatVersion != 1 {
		t.Fatalf("new filesystem format %d, %v; want 1", info.FormatVersion, err)
	}
	for _, v := range []uint32{1, 2} {
		if err := updateFilesystemEntry(ctx, store, "test", func(e *pb.FilesystemEntry) { e.FormatVersion = v }); err != nil {
			t.Fatal(err)
		}
		if err := New(store, "test").Attach(ctx); err != nil {
			t.Fatalf("attach format %d: %v", v, err)
		}
		if err := New(store, "test").BootstrapReadOnly(ctx); err != nil {
			t.Fatalf("read-only open of format %d: %v", v, err)
		}
	}
	if err := updateFilesystemEntry(ctx, store, "test", func(e *pb.FilesystemEntry) { e.FormatVersion = 3 }); err != nil {
		t.Fatal(err)
	}
	if err := New(store, "test").Attach(ctx); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("attach format 3: %v, want ErrNewerFormat", err)
	}
}
