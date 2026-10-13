package icbfs

import (
	"context"
	"errors"
	"testing"
)

// TestOpenSessions checks the core of what openSessions promises: Stat
// sees an open session's unflushed writes, and an unlinked file's
// objects outlive its name until the last session closes, with writes
// after the unlink discarded.
func TestOpenSessions(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	key, _, etag, err := fsys.Create(ctx, root, "f", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a := fsys.NewOpenFile(key, etag, nil)
	if err := a.WriteAt([]byte("abc"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if attr, err := fsys.Stat(ctx, key, TypeFile); err != nil || attr.Size != 3 {
		t.Fatalf("stat with an unflushed write: size %d, %v; want 3", attr.Size, err)
	}
	if _, err := a.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// A second session, then unlink: the name is gone, the objects stay.
	b, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := fsys.Unlink(ctx, root, "f"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, _, err := fsys.Lookup(ctx, root, "f"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup after unlink: %v", err)
	}
	attr, err := fsys.Stat(ctx, key, TypeFile)
	if err != nil || attr.Nlink != 0 || attr.Size != 3 {
		t.Fatalf("stat of the unlinked open file: nlink %d size %d, %v; want 0, 3", attr.Nlink, attr.Size, err)
	}
	if got := string(b.ReadAt(0, 3)); got != "abc" {
		t.Fatalf("read after unlink = %q", got)
	}
	// Writes after the unlink work locally and are dropped at flush.
	if err := b.WriteAt([]byte("xyz"), 0); err != nil {
		t.Fatalf("write after unlink: %v", err)
	}
	if _, err := b.Flush(ctx); err != nil {
		t.Fatalf("flush after unlink: %v", err)
	}
	if data, _, _, err := fsys.ReadFile(ctx, key); err != nil || string(data) != "abc" {
		t.Fatalf("stored content after a dropped flush = %q, %v; want abc", data, err)
	}

	// One session still open: still there.
	if err := a.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	if _, err := store.Head(ctx, key); err != nil {
		t.Fatalf("content gone with a session still open: %v", err)
	}
	// The last close deletes everything.
	if err := b.Close(); err != nil {
		t.Fatalf("close b: %v", err)
	}
	if _, err := store.Head(ctx, key); err == nil {
		t.Fatal("content still there after the last close")
	}
	if _, err := store.Head(ctx, metadataObjectKey(key)); err == nil {
		t.Fatal(".metadata still there after the last close")
	}

	// Not open anywhere: unlink deletes at once, as before.
	key2, _, _, err := fsys.Create(ctx, root, "g", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create g: %v", err)
	}
	if err := fsys.Unlink(ctx, root, "g"); err != nil {
		t.Fatalf("unlink g: %v", err)
	}
	if _, err := store.Head(ctx, key2); err == nil {
		t.Fatal("unlink of a file nobody has open left its content behind")
	}
}
