package icbfs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// countVersions is how many versions (delete markers included) keys
// starting with prefix have.
func countVersions(t *testing.T, ctx context.Context, f *Filesystem, prefix string) int {
	t.Helper()
	vs, err := f.store.ListAllVersions(ctx, prefix)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range vs {
		if strings.HasPrefix(v.Key, prefix) {
			n++
		}
	}
	return n
}

// TestPurgeTrash: permanently deleting a trashed file erases every
// version of it and its trash entry; the trash can't bring it back.
func TestPurgeTrash(t *testing.T) {
	f, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()

	key := writeNew(t, ctx, f, root, "a", "one")
	if _, _, err := f.WriteFile(ctx, key, []byte("two"), ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.CreateStream(ctx, key, TypeFile, "ads"); err != nil {
		t.Fatal(err)
	}
	keep := writeNew(t, ctx, f, root, "keep", "kept")
	if err := f.Unlink(ctx, root, "a"); err != nil {
		t.Fatal(err)
	}
	trash, _ := f.ListTrash(ctx, TrashOptions{})
	if err := f.PurgeTrash(ctx, trash[0].EntryID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := countVersions(t, ctx, f, key); n != 0 {
		t.Fatalf("%d versions of the purged file's objects left", n)
	}
	if n := countVersions(t, ctx, f, f.trashPrefix()); n != 0 {
		t.Fatalf("%d versions of trash entries left", n)
	}
	if _, err := f.Restore(ctx, trash[0].EntryID, RestoreOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore after purge: %v; want ErrNotFound", err)
	}
	if got := readName(t, ctx, f, root, "keep"); got != "kept" || countVersions(t, ctx, f, keep) == 0 {
		t.Fatal("purge touched another file")
	}
}

// TestPurgeLiveTree: purging a live directory erases it and everything
// under it, split blocks included; a hard-linked file inside stops it,
// unless allNames, which removes its other name too.
func TestPurgeLiveTree(t *testing.T) {
	f, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()

	d, _, err := f.Mkdir(ctx, root, "d", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	sub, _, err := f.Mkdir(ctx, d, "sub", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 0; i < 3*testDirBlockEntries; i++ { // enough to split d
		keys = append(keys, writeNew(t, ctx, f, d, fmt.Sprintf("f%02d", i), "x"))
	}
	inner := writeNew(t, ctx, f, sub, "inner", "y")
	if _, err := f.Link(ctx, root, "outside", inner, TypeFile); err != nil {
		t.Fatal(err)
	}
	if err := f.Purge(ctx, root, "d", false); !errors.Is(err, ErrHasLinks) {
		t.Fatalf("purge with a hard link inside: %v; want ErrHasLinks", err)
	}
	before := countVersions(t, ctx, f, f.prefix())
	if err := f.Purge(ctx, root, "d", true); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, name := range []string{"d", "outside"} {
		if _, _, err := f.Lookup(ctx, root, name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("lookup %s after purge: %v", name, err)
		}
	}
	for _, k := range append(keys, d, sub, inner) {
		if n := countVersions(t, ctx, f, k); n != 0 {
			t.Fatalf("%s: %d versions left", k, n)
		}
	}
	// Only the root's own block (the name stays in its history) and
	// other bookkeeping remain; most of what there was is gone.
	if after := countVersions(t, ctx, f, f.prefix()); after*2 > before {
		t.Fatalf("%d of %d versions left after purge", after, before)
	}
	if trash, _ := f.ListTrash(ctx, TrashOptions{}); len(trash) != 0 {
		t.Fatalf("a purge left trash entries: %+v", trash)
	}
}

// TestPruneErasesHistory: pruning a filesystem leaves none of its
// objects' versions behind, so a filesystem reusing its slot starts
// with no past.
func TestPruneErasesHistory(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	key := writeNew(t, ctx, f, f.RootKey(), "a", "one")
	if _, _, err := f.WriteFile(ctx, key, []byte("two"), ""); err != nil {
		t.Fatal(err)
	}
	prefix := f.prefix()
	if err := Archive(ctx, store, "test"); err != nil {
		t.Fatal(err)
	}
	if err := Prune(ctx, store, "test"); err != nil {
		t.Fatal(err)
	}
	if n := countVersions(t, ctx, f, prefix); n != 0 {
		t.Fatalf("%d versions left under %s after prune", n, prefix)
	}
}
