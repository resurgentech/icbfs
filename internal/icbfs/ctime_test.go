package icbfs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestStatusChangeTimes checks ctime (and a directory's mtime) the way
// pjdfstest does: every successful change moves it forward, and reads
// don't.
func TestStatusChangeTimes(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	stat := func(key string, typ EntryType) Attr {
		t.Helper()
		a, err := fsys.Stat(ctx, key, typ)
		if err != nil {
			t.Fatalf("stat %s: %v", key, err)
		}
		return a
	}
	// Stored times are exact, but a content write's comes from S3's
	// one-second Last-Modified; pause so "later" is visible either way.
	tick := func() { time.Sleep(10 * time.Millisecond) }
	moved := func(what string, before, after time.Time) {
		t.Helper()
		if !after.After(before) {
			t.Fatalf("%s: time didn't move forward (%v -> %v)", what, before, after)
		}
	}
	same := func(what string, before, after time.Time) {
		t.Helper()
		if !after.Equal(before) {
			t.Fatalf("%s: time changed (%v -> %v)", what, before, after)
		}
	}

	dirKey, _, err := fsys.Mkdir(ctx, root, "d", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if d := stat(dirKey, TypeDir); d.Ctime.IsZero() {
		t.Fatal("new directory has no ctime")
	}
	key, _, _, err := fsys.Create(ctx, dirKey, "f", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// chmod: ctime moves, mtime doesn't.
	a0 := stat(key, TypeFile)
	tick()
	mode := uint32(0600)
	if _, err := fsys.SetAttr(ctx, key, TypeFile, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	a1 := stat(key, TypeFile)
	moved("chmod ctime", a0.Ctime, a1.Ctime)
	same("chmod mtime", a0.Mtime, a1.Mtime)

	// A read's atime update isn't a status change.
	tick()
	if err := fsys.TouchAtime(ctx, key, TypeFile); err != nil {
		t.Fatalf("touch atime: %v", err)
	}
	a2 := stat(key, TypeFile)
	same("atime update ctime", a1.Ctime, a2.Ctime)
	moved("atime update atime", a1.Atime, a2.Atime)
	d0 := stat(dirKey, TypeDir)
	if err := fsys.TouchAtime(ctx, dirKey, TypeDir); err != nil {
		t.Fatalf("touch dir atime: %v", err)
	}
	same("readdir ctime", d0.Ctime, stat(dirKey, TypeDir).Ctime)

	// link: the file's ctime and the directory's mtime/ctime move.
	tick()
	if _, err := fsys.Link(ctx, dirKey, "g", key, TypeFile); err != nil {
		t.Fatalf("link: %v", err)
	}
	a3, d1 := stat(key, TypeFile), stat(dirKey, TypeDir)
	moved("link ctime", a2.Ctime, a3.Ctime)
	moved("link dir mtime", d0.Mtime, d1.Mtime)
	moved("link dir ctime", d0.Ctime, d1.Ctime)

	// unlink of one name: the surviving file's ctime moves too.
	tick()
	if err := fsys.Unlink(ctx, dirKey, "g"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	a4, d2 := stat(key, TypeFile), stat(dirKey, TypeDir)
	moved("unlink ctime", a3.Ctime, a4.Ctime)
	moved("unlink dir mtime", d1.Mtime, d2.Mtime)

	// A failed change leaves everything alone.
	tick()
	if err := fsys.Unlink(ctx, dirKey, "missing"); err == nil {
		t.Fatal("unlink of a missing name succeeded")
	}
	same("failed unlink dir mtime", d2.Mtime, stat(dirKey, TypeDir).Mtime)

	// rename: both directories' times and the renamed file's ctime.
	other, _, err := fsys.Mkdir(ctx, root, "e", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir e: %v", err)
	}
	e0 := stat(other, TypeDir)
	tick()
	if err := fsys.Rename(ctx, dirKey, "f", other, "f2", true); err != nil {
		t.Fatalf("rename: %v", err)
	}
	a5, d3, e1 := stat(key, TypeFile), stat(dirKey, TypeDir), stat(other, TypeDir)
	moved("rename ctime", a4.Ctime, a5.Ctime)
	moved("rename source dir mtime", d2.Mtime, d3.Mtime)
	moved("rename target dir mtime", e0.Mtime, e1.Mtime)

	// A content write: S3's Last-Modified is whole seconds, so wait one.
	time.Sleep(1100 * time.Millisecond)
	if _, _, err := fsys.WriteFile(ctx, key, []byte("hello"), ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	a6 := stat(key, TypeFile)
	moved("write ctime", a5.Ctime, a6.Ctime)
	moved("write mtime", a5.Mtime, a6.Mtime)

	// A directory deep enough that adding an entry rewrites only a leaf,
	// not its root block: its times must move all the same.
	big, _, err := fsys.Mkdir(ctx, root, "big", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir big: %v", err)
	}
	for i := 0; i < 4*testDirBlockEntries; i++ {
		if _, _, _, err := fsys.Create(ctx, big, fmt.Sprintf("n%03d", i), 0644, 0, 0); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	b0 := stat(big, TypeDir)
	tick()
	if _, _, _, err := fsys.Create(ctx, big, "n999", 0644, 0, 0); err != nil {
		t.Fatalf("create n999: %v", err)
	}
	b1 := stat(big, TypeDir)
	moved("create in a multi-level directory: mtime", b0.Mtime, b1.Mtime)
	moved("create in a multi-level directory: ctime", b0.Ctime, b1.Ctime)
}
