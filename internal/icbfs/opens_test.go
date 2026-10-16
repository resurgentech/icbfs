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

// TestOpensShareContent: the opens of a file in one Filesystem (one mount)
// share its content: each reads the others' writes at once, one flush
// stores them all, and the last close ends it. Opening a file already open
// here still sees another client's newer version, unless there are writes
// here not yet flushed.
func TestOpensShareContent(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, []byte("0123456789"), ""); err != nil {
		t.Fatal(err)
	}
	a, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WriteAt([]byte("AA"), 0); err != nil {
		t.Fatal(err)
	}
	if got := string(b.ReadAt(0, 10)); got != "AA23456789" {
		t.Fatalf("b reads %q before any flush, want a's write", got)
	}
	if err := b.WriteAt([]byte("BB"), 8); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _, _, _ := fsys.ReadFile(ctx, key); string(got) != "AA234567BB" {
		t.Fatalf("stored %q after a's flush, want both writes", got)
	}
	if b.Dirty() {
		t.Fatal("b still has writes to flush after a's flush stored them")
	}

	// Another client writes; a new open here, with nothing unflushed,
	// sees it, and so do the opens already there.
	if _, _, err := fsys.WriteFile(ctx, key, []byte("elsewhere!"), ""); err != nil {
		t.Fatal(err)
	}
	c, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(a.ReadAt(0, 10)); got != "elsewhere!" {
		t.Fatalf("after a new open, a reads %q, want the other client's write", got)
	}

	// With a write here not yet flushed, a new open keeps it.
	if err := c.WriteAt([]byte("C"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, []byte("third ver."), ""); err != nil {
		t.Fatal(err)
	}
	d, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(d.ReadAt(0, 1)); got != "C" {
		t.Fatalf("a new open reads %q, want the unflushed write", got)
	}
	if _, err := d.Flush(ctx); err != nil { // merges onto "third ver."
		t.Fatal(err)
	}
	if got, _, _, _ := fsys.ReadFile(ctx, key); string(got) != "Chird ver." {
		t.Fatalf("stored %q, want the write merged onto the newer version", got)
	}

	for _, o := range []*OpenFile{a, b, c, d} {
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if fsys.openContent(key) != nil {
		t.Fatal("the content outlived the last close")
	}
}
