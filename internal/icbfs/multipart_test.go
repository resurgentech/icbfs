package icbfs

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// partsStore records each PutParts call's copied and uploaded parts.
type partsStore struct {
	objstore.Store
	mu    sync.Mutex
	calls [][2]int // {copied, uploaded} per PutParts call
}

func (s *partsStore) PutParts(ctx context.Context, key string, parts []objstore.Part, metadata map[string]string, ifMatch string) (*objstore.Object, error) {
	copied, uploaded := 0, 0
	for _, p := range parts {
		if p.Body == nil {
			copied++
		} else {
			uploaded++
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, [2]int{copied, uploaded})
	s.mu.Unlock()
	return s.Store.PutParts(ctx, key, parts, metadata, ifMatch)
}

func (s *partsStore) last(t *testing.T) (copied, uploaded int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("no multipart write happened")
	}
	c := s.calls[len(s.calls)-1]
	return c[0], c[1]
}

// limitsStore is a store that only answers PartLimits.
type limitsStore struct {
	objstore.Store
	limits objstore.PartLimits
}

func (s limitsStore) PartLimits() objstore.PartLimits { return s.limits }

func TestPartSize(t *testing.T) {
	s3 := objstore.PartLimits{Min: 5 << 20, Max: 5 << 30, Count: 10000}
	azure := objstore.PartLimits{Min: 1, Max: 4000 << 20, Count: 50000}
	for _, tc := range []struct {
		limits    objstore.PartLimits
		max, want int64
	}{
		{s3, 8 << 30, 8 << 20},
		{s3, DefaultMaxFileSize, 128 << 20},
		{s3, 1 << 20, 8 << 20},
		{azure, DefaultMaxFileSize, 32 << 20},
		{azure, 8 << 30, 8 << 20},
	} {
		f := &Filesystem{maxFileSize: tc.max, store: limitsStore{limits: tc.limits}}
		got := f.partSize()
		if got != tc.want {
			t.Errorf("partSize for max %d, %d parts = %d, want %d", tc.max, tc.limits.Count, got, tc.want)
		}
		if tc.max/got > tc.limits.Count || got < tc.limits.Min || got > tc.limits.Max {
			t.Errorf("max %d: %d parts of %d, outside %+v", tc.max, tc.max/got, got, tc.limits)
		}
	}
}

// TestMultipartFlush: on a filesystem with an 8 GiB limit (8 MiB parts),
// a file bigger than one part is written in parts, and a later edit
// uploads only the parts it touched; the rest are copied server-side.
// Concurrent edits to different parts both survive a rebase.
func TestMultipartFlush(t *testing.T) {
	store := &partsStore{Store: newTestStore(t)}
	ctx := context.Background()
	fsys := New(store, "mp")
	fsys.SetCreationMaxFileSize(8 << 30)
	if err := fsys.Bootstrap(ctx, 1<<40, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	const mib = 1 << 20
	part := fsys.partSize()
	if part != 8*mib {
		t.Fatalf("part size %d, want 8 MiB", part)
	}
	key, _, etag, err := fsys.Create(ctx, fsys.RootKey(), "big", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	want := bytes.Repeat([]byte("0123456789abcdef"), 20*mib/16) // 20 MiB: parts of 8, 8 and 4 MiB
	o := fsys.NewOpenFile(key, etag, nil)
	if err := o.WriteAt(want, 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	o.Close()
	if c, u := store.last(t); c != 0 || u != 3 {
		t.Fatalf("first write: %d copied, %d uploaded; want 0, 3", c, u)
	}
	check := func(what string) {
		t.Helper()
		got, _, _, err := fsys.ReadFile(ctx, key)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: content differs (%d bytes, %v)", what, len(got), err)
		}
	}
	check("first write")

	// One byte in the middle part: one upload, two copies.
	o, err = fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := o.WriteAt([]byte("X"), 9*mib); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	o.Close()
	want[9*mib] = 'X'
	if c, u := store.last(t); c != 2 || u != 1 {
		t.Fatalf("one-byte edit: %d copied, %d uploaded; want 2, 1", c, u)
	}
	check("one-byte edit")

	// A byte in the first part and one in the last: two uploads, and the
	// middle part, between them, is still copied.
	o, err = fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := o.WriteAt([]byte("Y"), 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := o.WriteAt([]byte("Z"), 17*mib); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	o.Close()
	want[1], want[17*mib] = 'Y', 'Z'
	if c, u := store.last(t); c != 1 || u != 2 {
		t.Fatalf("edits in the first and last parts: %d copied, %d uploaded; want 1, 2", c, u)
	}
	check("edits in the first and last parts")

	// Two clients (two Filesystems, as two mounts: opens in one share
	// their content) edit different parts; the second loses the race,
	// rebases, and both edits land.
	a, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	b, err := otherClient(t, fsys).Open(ctx, key)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()
	if err := a.WriteAt([]byte("A"), 1*mib); err != nil {
		t.Fatalf("a write: %v", err)
	}
	if err := b.WriteAt([]byte("B"), 17*mib); err != nil {
		t.Fatalf("b write: %v", err)
	}
	if _, err := a.Flush(ctx); err != nil {
		t.Fatalf("a flush: %v", err)
	}
	if _, err := b.Flush(ctx); err != nil {
		t.Fatalf("b flush: %v", err)
	}
	want[1*mib], want[17*mib] = 'A', 'B'
	check("concurrent edits")
	if c, u := store.last(t); c != 2 || u != 1 {
		t.Fatalf("rebased flush: %d copied, %d uploaded; want 2, 1", c, u)
	}

	// Shrinking to 10 MiB copies what's left of the base.
	size := int64(10 * mib)
	if _, err := fsys.SetAttr(ctx, key, TypeFile, nil, nil, nil, &size, nil, nil, nil); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	want = want[:10*mib]
	check("truncate")
	if c, u := store.last(t); c != 2 || u != 0 {
		t.Fatalf("truncate: %d copied, %d uploaded; want 2, 0", c, u)
	}

	// Down to one part: a plain Put, no multipart.
	calls := len(store.calls)
	size = 4 * mib
	if _, err := fsys.SetAttr(ctx, key, TypeFile, nil, nil, nil, &size, nil, nil, nil); err != nil {
		t.Fatalf("truncate to one part: %v", err)
	}
	want = want[:4*mib]
	check("truncate to one part")
	if len(store.calls) != calls {
		t.Fatal("a one-part file was written with PutParts")
	}
}

// newPartsFilesystem is a filesystem with an 8 GiB limit (8 MiB parts) on
// a store that records each multipart write's copied and uploaded parts.
func newPartsFilesystem(t *testing.T) (*Filesystem, *partsStore) {
	t.Helper()
	store := &partsStore{Store: newTestStore(t)}
	fsys := New(store, "mp")
	fsys.SetCreationMaxFileSize(8 << 30)
	if err := fsys.Bootstrap(context.Background(), 1<<40, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if part := fsys.partSize(); part != 8<<20 {
		t.Fatalf("part size %d, want 8 MiB", part)
	}
	return fsys, store
}

// writeBig creates name in the root with data as its content, and
// returns its key and the version written.
func writeBig(t *testing.T, ctx context.Context, fsys *Filesystem, name string, data []byte) (string, *objstore.Object) {
	t.Helper()
	key, _, etag, err := fsys.Create(ctx, fsys.RootKey(), name, 0644, 0, 0)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	o := fsys.NewOpenFile(key, etag, nil)
	defer o.Close()
	if err := o.WriteAt(data, 0); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush %s: %v", name, err)
	}
	head, err := fsys.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, head
}

// TestMultipartStubBase: one byte edited in a three-part file whose
// content is a stub uploads exactly one part; the other two are copied
// from the stub's target at its version. The result is a full version
// under the file's own key, with the right bytes, and the target is
// untouched.
func TestMultipartStubBase(t *testing.T) {
	fsys, store := newPartsFilesystem(t)
	ctx := context.Background()
	const mib = 1 << 20
	data := bytes.Repeat([]byte("0123456789abcdef"), 20*mib/16) // parts of 8, 8 and 4 MiB
	target, tv := writeBig(t, ctx, fsys, "target", data)

	key, _, etag, err := fsys.Create(ctx, fsys.RootKey(), "a", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.PutRef(ctx, key, target, tv.VersionID, nil, etag); err != nil {
		t.Fatalf("put ref: %v", err)
	}

	o, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := o.WriteAt([]byte("X"), 9*mib); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush over a stub: %v", err)
	}
	o.Close()
	if c, u := store.last(t); c != 2 || u != 1 {
		t.Fatalf("one-byte edit over a stub: %d copied, %d uploaded; want 2, 1", c, u)
	}
	want := append([]byte(nil), data...)
	want[9*mib] = 'X'
	got, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("content after the edit differs (%d bytes, %v)", len(got), err)
	}
	head, err := fsys.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if IsRef(head) || head.Size != int64(len(want)) {
		t.Fatalf("the edit is a stub (%v) of %d bytes; want a full version of %d", IsRef(head), head.Size, len(want))
	}
	if got, _, _, err := fsys.ReadFile(ctx, target); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the target changed (%d bytes, %v)", len(got), err)
	}
}

// TestMultipartFlushBaseErased: the version an open, dirty file is based
// on is erased under it, after another client replaced it (here by a stub,
// as dedupe does). The flush's part copies from it fail not-found; the
// flush rebases onto the current content and lands, with the edit on it.
func TestMultipartFlushBaseErased(t *testing.T) {
	fsys, store := newPartsFilesystem(t)
	ctx := context.Background()
	const mib = 1 << 20
	old := bytes.Repeat([]byte("old content, 16."), 20*mib/16)
	data := bytes.Repeat([]byte("0123456789abcdef"), 20*mib/16)
	key, v1 := writeBig(t, ctx, fsys, "a", old)
	target, tv := writeBig(t, ctx, fsys, "target", data)

	o, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer o.Close()
	if err := o.WriteAt([]byte("X"), 9*mib); err != nil {
		t.Fatal(err)
	}

	other := otherClient(t, fsys)
	if _, err := other.PutRef(ctx, key, target, tv.VersionID, nil, v1.ETag); err != nil {
		t.Fatalf("put ref elsewhere: %v", err)
	}
	if err := fsys.store.Erase(ctx, key, v1.VersionID); err != nil {
		t.Fatalf("erase the base version: %v", err)
	}

	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush with its base erased: %v", err)
	}
	want := append([]byte(nil), data...)
	want[9*mib] = 'X'
	got, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("content after the flush differs (%d bytes, %v)", len(got), err)
	}
	if c, u := store.last(t); c != 2 || u != 1 {
		t.Fatalf("rebased flush: %d copied, %d uploaded; want 2, 1", c, u)
	}
}
