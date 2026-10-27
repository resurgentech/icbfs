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
	if _, err := fsys.WriteFile(ctx, key, []byte("new"), ""); err != nil {
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

// TestAtimeHourly: NTFS's rule. A first access is recorded; another within
// the hour isn't, and costs this mount no request at all; one more than an
// hour after the recorded access is recorded again, also by another mount
// that never saw the first.
func TestAtimeHourly(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetAtime(AtimeHourly, true)
	atime := func() time.Time {
		t.Helper()
		a, err := fsys.Stat(ctx, key, TypeFile)
		if err != nil {
			t.Fatal(err)
		}
		return a.Atime
	}
	if err := fsys.TouchAtime(ctx, key, TypeFile); err != nil {
		t.Fatal(err)
	}
	first := atime()
	if first.IsZero() {
		t.Fatal("a first access wasn't recorded")
	}

	counter := &countingStore{Store: store}
	quiet := New(counter, fsys.fsName)
	if err := quiet.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	quiet.SetAtime(AtimeHourly, true)
	if err := quiet.TouchAtime(ctx, key, TypeFile); err != nil {
		t.Fatal(err)
	}
	if got := atime(); !got.Equal(first) {
		t.Fatalf("an access within the hour was recorded: %v, then %v", first, got)
	}
	before := counter.gets.Load()
	for range 5 {
		if err := quiet.TouchAtime(ctx, key, TypeFile); err != nil {
			t.Fatal(err)
		}
	}
	if n := counter.gets.Load() - before; n != 0 {
		t.Fatalf("5 more accesses within the hour cost %d requests, want 0", n)
	}

	// An hour and more later (set back by hand), another mount records it.
	old := time.Now().Add(-2 * time.Hour)
	if _, err := fsys.SetAttr(ctx, key, TypeFile, nil, nil, nil, nil, &old, nil, nil); err != nil {
		t.Fatal(err)
	}
	later := New(store, fsys.fsName)
	if err := later.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	later.SetAtime(AtimeHourly, true)
	if err := later.TouchAtime(ctx, key, TypeFile); err != nil {
		t.Fatal(err)
	}
	if got := atime(); time.Since(got) > time.Minute {
		t.Fatalf("an access over an hour after the last wasn't recorded: %v", got)
	}
}
