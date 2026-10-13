package icbfs

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestMaxFileSize checks the per-filesystem limit: fixed at creation,
// enforced on writes and truncates with ErrFileTooLarge, and 1 TiB when
// unset.
func TestMaxFileSize(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	def := New(store, "default")
	if err := def.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if def.MaxFileSize() != DefaultMaxFileSize {
		t.Fatalf("default max file size = %d, want %d", def.MaxFileSize(), int64(DefaultMaxFileSize))
	}

	const limit = 1 << 20
	fsys := New(store, "small")
	fsys.SetCreationMaxFileSize(limit)
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if fsys.MaxFileSize() != limit {
		t.Fatalf("max file size = %d, want %d", fsys.MaxFileSize(), limit)
	}
	// Fixed at creation: a later mount asking for something else gets
	// what's stored.
	again := New(store, "small")
	again.SetCreationMaxFileSize(1 << 30)
	if err := again.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap again: %v", err)
	}
	if again.MaxFileSize() != limit {
		t.Fatalf("remounted max file size = %d, want the stored %d", again.MaxFileSize(), limit)
	}

	root := fsys.RootKey()
	key, _, etag, err := fsys.Create(ctx, root, "f", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	o := fsys.NewOpenFile(key, etag, nil)
	defer o.Close()
	if err := o.WriteAt([]byte("x"), limit-1); err != nil {
		t.Fatalf("write ending exactly at the limit: %v", err)
	}
	if err := o.WriteAt([]byte("xy"), limit-1); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("write past the limit: %v, want ErrFileTooLarge", err)
	}
	if _, err := o.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	over := int64(limit + 1)
	if _, err := fsys.SetAttr(ctx, key, TypeFile, nil, nil, nil, &over, nil, nil, nil); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("truncate past the limit: %v, want ErrFileTooLarge", err)
	}

	// Truncate shrinks and grows (zero-filled) through the scratch file.
	if _, _, err := fsys.WriteFile(ctx, key, []byte("0123456789"), ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, tc := range []struct {
		size int64
		want []byte
	}{
		{4, []byte("0123")},
		{8, []byte("0123\x00\x00\x00\x00")},
		{0, nil},
	} {
		size := tc.size
		attr, err := fsys.SetAttr(ctx, key, TypeFile, nil, nil, nil, &size, nil, nil, nil)
		if err != nil || attr.Size != tc.size {
			t.Fatalf("truncate to %d: size %d, %v", tc.size, attr.Size, err)
		}
		data, _, _, err := fsys.ReadFile(ctx, key)
		if err != nil || !bytes.Equal(data, tc.want) {
			t.Fatalf("after truncate to %d: %q, %v; want %q", tc.size, data, err, tc.want)
		}
	}
}

// TestTruncateSurvivesARebase: a session that shrank the file and lost
// its flush's CAS race to another writer keeps both its size and the
// other writer's edit.
func TestTruncateSurvivesARebase(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, []byte("0123456789"), ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	a, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	b, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()

	if err := b.WriteAt([]byte("X"), 2); err != nil {
		t.Fatalf("b write: %v", err)
	}
	if _, err := b.Flush(ctx); err != nil {
		t.Fatalf("b flush: %v", err)
	}
	if err := a.Truncate(5); err != nil {
		t.Fatalf("a truncate: %v", err)
	}
	if _, err := a.Flush(ctx); err != nil {
		t.Fatalf("a flush: %v", err)
	}
	data, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil || string(data) != "01X34" {
		t.Fatalf("content = %q, %v; want 01X34", data, err)
	}
}
