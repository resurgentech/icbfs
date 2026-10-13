package icbfs

import (
	"context"
	"errors"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// TestNamedStreams covers named data streams end to end: independent
// content per stream and the main stream, create/rename/delete, and
// stream objects being deleted with their owner.
func TestNamedStreams(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	file, _, _, err := fsys.Create(ctx, root, "f", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := fsys.WriteFile(ctx, file, []byte("main"), ""); err != nil {
		t.Fatalf("write main: %v", err)
	}
	fooKey, _, err := fsys.CreateStream(ctx, file, TypeFile, "foo")
	if err != nil {
		t.Fatalf("create foo: %v", err)
	}
	barKey, _, err := fsys.CreateStream(ctx, file, TypeFile, "bar")
	if err != nil {
		t.Fatalf("create bar: %v", err)
	}
	if _, _, err := fsys.CreateStream(ctx, file, TypeFile, "foo"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate stream: err = %v, want ErrExists", err)
	}

	// Write through OpenFile, the same path the access layers use.
	open, err := fsys.Open(ctx, fooKey)
	if err != nil {
		t.Fatalf("open foo: %v", err)
	}
	if err := open.WriteAt([]byte("foo-data"), 0); err != nil {
		t.Fatalf("write foo: %v", err)
	}
	if _, err := open.Flush(ctx); err != nil {
		t.Fatalf("flush foo: %v", err)
	}
	_ = open.Close()

	for key, want := range map[string]string{file: "main", fooKey: "foo-data", barKey: ""} {
		got, _, _, err := fsys.ReadFile(ctx, key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if string(got) != want {
			t.Fatalf("read %s = %q, want %q", key, got, want)
		}
	}
	if a, err := fsys.StatStream(ctx, fooKey); err != nil || a.Size != int64(len("foo-data")) {
		t.Fatalf("StatStream foo = %+v, %v", a, err)
	}

	if err := fsys.RenameStream(ctx, file, TypeFile, "foo", "bar", false); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto existing without replace: err = %v, want ErrExists", err)
	}
	if err := fsys.RenameStream(ctx, file, TypeFile, "foo", "baz", false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := fsys.DeleteStream(ctx, file, TypeFile, "bar"); err != nil {
		t.Fatalf("delete bar: %v", err)
	}
	if _, err := store.Head(ctx, barKey); !objstore.IsNotFound(err) {
		t.Fatalf("bar object after DeleteStream: err = %v, want not found", err)
	}
	attr, err := fsys.Stat(ctx, file, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if len(attr.Streams) != 1 || attr.Streams["baz"] == 0 || attr.Size != int64(len("main")) {
		t.Fatalf("after rename/delete: streams = %v, size = %d", attr.Streams, attr.Size)
	}

	if err := fsys.Unlink(ctx, root, "f"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := store.Head(ctx, fooKey); !objstore.IsNotFound(err) {
		t.Fatalf("stream object after last unlink: err = %v, want not found", err)
	}

	// Directories can carry streams too; Rmdir removes them.
	dir, _, err := fsys.Mkdir(ctx, root, "d", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dsKey, _, err := fsys.CreateStream(ctx, dir, TypeDir, "s")
	if err != nil {
		t.Fatalf("dir stream: %v", err)
	}
	if err := fsys.Rmdir(ctx, root, "d"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if _, err := store.Head(ctx, dsKey); !objstore.IsNotFound(err) {
		t.Fatalf("dir stream object after Rmdir: err = %v, want not found", err)
	}
}
