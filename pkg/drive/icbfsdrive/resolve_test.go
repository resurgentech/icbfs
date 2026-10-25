package icbfsdrive

import (
	"context"
	"fmt"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestNewItemsResolveByIDWithoutASearch: an item just made, asked for by
// its id, is found where the drive made it, and one made elsewhere is
// found at the path the caller gave with its id: neither costs a search
// of the tree, a read of every directory, as an id alone does when the
// drive has never seen it.
func TestNewItemsResolveByIDWithoutASearch(t *testing.T) {
	store := &readCounter{Store: miniotest.Store(t)}
	s := icbfs.Settings{Name: "resolve", Size: 1 << 30, RootMode: 0o777}
	a := newDrive(t, store, s, "")
	ctx := system()
	const dirs = 40
	for i := range dirs {
		write(t, ctx, a, fmt.Sprintf("/d%02d/f.txt", i), "x", drive.Condition{})
	}
	file := write(t, ctx, a, fmt.Sprintf("/d%02d/new.txt", dirs-1), "new", drive.Condition{})
	dir, err := a.Mkdir(ctx, fmt.Sprintf("/d%02d/sub", dirs-1), false)
	if err != nil {
		t.Fatal(err)
	}

	cost := func(d *Drive, ref drive.ItemRef) int {
		t.Helper()
		before := store.n.Load()
		it, err := d.Stat(ctx, ref, drive.At{})
		if err != nil || it.ID != ref.ID {
			t.Fatalf("stat %+v: %+v, %v", ref, it, err)
		}
		return int(store.n.Load() - before)
	}
	for _, it := range []drive.Item{file, dir} {
		if n := cost(a, drive.ByID(it.ID)); n >= dirs/2 {
			t.Errorf("%s by id, on the drive that made it: %d reads, a search", it.Path, n)
		}
	}

	// Another drive, which has never seen them.
	b, err := New(context.Background(), Config{Store: store, Filesystem: s.Name, ID: "drive-b", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	if n := cost(b, drive.ItemRef{ID: file.ID, Path: file.Path}); n >= dirs/2 {
		t.Errorf("by id with its path: %d reads, a search", n)
	}
	if n := cost(b, drive.ItemRef{ID: dir.ID, Path: "/d00/f.txt"}); n < dirs {
		t.Errorf("by id with a path to something else: %d reads; want a search, which finds it", n)
	}
}
