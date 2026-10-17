package icbfs

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestCheck: a filesystem made through the API checks clean, directories
// split and all, and each kind of damage done to it underneath is reported.
func TestCheck(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()
	dir, _, err := fsys.Mkdir(ctx, root, "d", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for i := 0; i < 5*testDirBlockEntries; i++ { // splits d
		key, _, _, err := fsys.Create(ctx, dir, fmt.Sprintf("f%02d", i), 0o644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, key)
	}
	if _, err := fsys.Link(ctx, root, "hard", files[0], TypeFile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fsys.Symlink(ctx, root, "sym", "d/f00", 0, 0); err != nil {
		t.Fatal(err)
	}
	sub, _, err := fsys.Mkdir(ctx, dir, "sub", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	r, err := fsys.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() || r.Directories != 3 || r.Files != 5*testDirBlockEntries+1 || r.Blocks < 3 {
		t.Fatalf("a clean filesystem: %+v", r)
	}

	kinds := func() map[string]int {
		t.Helper()
		r, err := fsys.Check(ctx)
		if err != nil {
			t.Fatal(err)
		}
		k := map[string]int{}
		for _, p := range r.Problems {
			k[p.Kind]++
		}
		return k
	}

	// A second name the link count doesn't know about.
	if err := fsys.insertEntry(ctx, root, "extra", files[1], TypeFile); err != nil {
		t.Fatal(err)
	}
	// A directory with a second name.
	if err := fsys.insertEntry(ctx, root, "sub-again", sub, TypeDir); err != nil {
		t.Fatal(err)
	}
	// A file whose .metadata is gone.
	if err := store.Delete(ctx, metadataObjectKey(files[2])); err != nil {
		t.Fatal(err)
	}
	got := kinds()
	for _, want := range []string{"link-count", "directory-linked", "missing-object"} {
		if got[want] != 1 {
			t.Errorf("%s reported %d times, want once: %v", want, got[want], got)
		}
	}

	// A block that doesn't decode.
	if _, err := store.Put(ctx, sub, bytes.NewReader([]byte("not a block")), nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := kinds(); got["unreadable-block"] != 1 {
		t.Errorf("unreadable-block: %v", got)
	}

	// An object nothing leads to is counted, not a problem.
	stray, err := fsys.newKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, stray, bytes.NewReader(nil), nil, ""); err != nil {
		t.Fatal(err)
	}
	r, err = fsys.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range r.UnreachableListed {
		found = found || k == stray
	}
	if !found {
		t.Errorf("the stray object isn't among the unreachable: %v", r.UnreachableListed)
	}
}

// checkClean fails t if fsys doesn't check clean: for tests that race
// clients, to catch damage they don't look for themselves.
func checkClean(t *testing.T, fsys *Filesystem) {
	t.Helper()
	r, err := fsys.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, p := range r.Problems {
		t.Errorf("check: %s %s %s: %s", p.Kind, p.Key, p.Path, p.Detail)
	}
}
