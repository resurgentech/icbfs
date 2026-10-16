package icbfs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestUnlinkWalksTheDirectoryOnce: unlink checks the entry's type in the
// walk that removes it, refusing a directory without removing it, and
// reads each of the directory's blocks once.
func TestUnlinkWalksTheDirectoryOnce(t *testing.T) {
	cs := &countingStore{Store: newTestStore(t)}
	fsys := New(cs, "test")
	ctx := context.Background()
	if err := fsys.Bootstrap(ctx, 1<<30, 0o755, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	root := fsys.RootKey()
	for i := 0; i < 10*testDirBlockEntries; i++ { // a tree two levels deep
		if _, _, _, err := fsys.Create(ctx, root, fmt.Sprintf("f%03d", i), 0o644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := fsys.Mkdir(ctx, root, "d", 0o755, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Unlink(ctx, root, "d"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("unlink a directory = %v, want ErrIsDir", err)
	}
	if _, _, err := fsys.Lookup(ctx, root, "d"); err != nil {
		t.Fatalf("the directory went: %v", err)
	}

	depth := 0
	for key := root; ; depth++ {
		blk := readBlock(t, fsys, key)
		if len(blk.Children) == 0 {
			depth++
			break
		}
		key = blk.Children[0].UUID
	}
	cs.gets.Store(0)
	if err := fsys.Unlink(ctx, root, "f050"); err != nil {
		t.Fatal(err)
	}
	// Each block on the way down once, the file's .metadata (nlink) and
	// the directory's (mtime). Looking the name up first, as Unlink did,
	// read the blocks twice: 8 here.
	if got := cs.gets.Load(); got > int64(depth)+2 {
		t.Fatalf("Unlink made %d reads, want at most %d (a %d-level tree once, and two records)", got, depth+2, depth)
	}
}
