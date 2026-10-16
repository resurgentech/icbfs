package icbfs

import (
	"context"
	"testing"
	"time"
)

// TestReadDoesNotChangeCtime: recording an access is not a status change,
// so a new file's ctime stays as it was (xfstests' generic/003).
func TestReadDoesNotChangeCtime(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fsys.Stat(ctx, key, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // past the store's one-second timestamps
	if err := fsys.TouchAtime(ctx, key, TypeFile); err != nil {
		t.Fatal(err)
	}
	after, err := fsys.Stat(ctx, key, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Ctime.Equal(before.Ctime) {
		t.Fatalf("ctime moved on a read: %v -> %v", before.Ctime, after.Ctime)
	}
	if !after.Atime.After(before.Atime) {
		t.Fatalf("atime didn't move: %v -> %v", before.Atime, after.Atime)
	}
}

func TestAtimeModes(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "d", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	atime := func(k string, typ EntryType) time.Time {
		t.Helper()
		a, err := fsys.Stat(ctx, k, typ)
		if err != nil {
			t.Fatal(err)
		}
		return a.Atime
	}
	touch := func(k string, typ EntryType) {
		t.Helper()
		if err := fsys.TouchAtime(ctx, k, typ); err != nil {
			t.Fatal(err)
		}
	}

	// noatime: nothing is recorded.
	fsys.SetAtime(AtimeNever, false)
	touch(key, TypeFile)
	if !atime(key, TypeFile).IsZero() {
		t.Fatal("noatime recorded an access")
	}

	// relatime: the first read is recorded (never read before), a second
	// one isn't (the recorded access is newer than the last change)...
	fsys.SetAtime(AtimeRelative, true)
	touch(key, TypeFile)
	first := atime(key, TypeFile)
	if first.IsZero() {
		t.Fatal("relatime didn't record a first access")
	}
	touch(key, TypeFile)
	if got := atime(key, TypeFile); !got.Equal(first) {
		t.Fatalf("relatime recorded a second access: %v -> %v", first, got)
	}
	// ... until the file changes again.
	time.Sleep(1100 * time.Millisecond)
	if _, _, err := fsys.WriteFile(ctx, key, []byte("new"), ""); err != nil {
		t.Fatal(err)
	}
	touch(key, TypeFile)
	if got := atime(key, TypeFile); !got.After(first) {
		t.Fatalf("relatime didn't record an access after a change: %v -> %v", first, got)
	}

	// strict: every read.
	fsys.SetAtime(AtimeStrict, true)
	before := atime(key, TypeFile)
	time.Sleep(10 * time.Millisecond)
	touch(key, TypeFile)
	if got := atime(key, TypeFile); !got.After(before) {
		t.Fatalf("strict didn't record: %v -> %v", before, got)
	}

	// nodiratime: directories are left alone, files aren't.
	fsys.SetAtime(AtimeStrict, false)
	touch(dir, TypeDir)
	if !atime(dir, TypeDir).IsZero() {
		t.Fatal("nodiratime recorded a directory access")
	}
}
