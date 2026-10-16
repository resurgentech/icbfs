package icbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/resurgentech/icbfs/internal/block"
)

func TestCreateAttachListDescribe(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Nothing created yet: attach fails, the list is empty.
	if err := New(store, "docs").Attach(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attach before any create: %v, want ErrNotFound", err)
	}
	if all, err := ListFilesystems(ctx, store); err != nil || len(all) != 0 {
		t.Fatalf("list of an empty bucket: %v, %v", all, err)
	}

	const sddl = "O:BAG:BAD:PAI(A;OICI;FA;;;BA)"
	info, err := CreateFilesystem(ctx, store, Settings{
		Name: "docs", Size: 1 << 30, PrimaryWindows: true, MaxFileSize: 8 << 30,
		RootMode: 0755, RootUid: 1000, RootGid: 1000, RootSDDL: sddl,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.ID != "0000" || !info.PrimaryWindows || info.MaxFileSize != 8<<30 || info.Created.IsZero() {
		t.Fatalf("created info = %+v", info)
	}
	if _, err := CreateFilesystem(ctx, store, Settings{Name: "docs"}); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: %v, want ErrExists", err)
	}
	if _, err := CreateFilesystem(ctx, store, Settings{Name: "bad/name"}); !errors.Is(err, ErrInvalidFilesystemName) {
		t.Fatalf("create with a bad name: %v", err)
	}

	f := New(store, "docs")
	if err := f.Attach(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !f.PrimaryWindows() || f.MaxFileSize() != 8<<30 {
		t.Fatalf("attached settings: windows=%v max=%d", f.PrimaryWindows(), f.MaxFileSize())
	}
	root, err := f.Stat(ctx, f.RootKey(), TypeDir)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	got, _ := root.WindowsACL()
	if root.Uid != 1000 || root.Mode != 0755 || string(got) != sddl {
		t.Fatalf("root = uid %d mode %o acl %q", root.Uid, root.Mode, got)
	}
	if _, _, _, err := f.Create(ctx, f.RootKey(), "a", 0644, 0, 0); err != nil {
		t.Fatalf("create a file in the new filesystem: %v", err)
	}

	if _, err := CreateFilesystem(ctx, store, Settings{Name: "other"}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	all, err := ListFilesystems(ctx, store)
	if err != nil || len(all) != 2 || all[1].Name != "other" || all[1].ID != "0001" {
		t.Fatalf("list = %+v, %v", all, err)
	}
	if d, err := DescribeFilesystem(ctx, store, "other"); err != nil || d.PrimaryWindows {
		t.Fatalf("describe other = %+v, %v", d, err)
	}
	if _, err := DescribeFilesystem(ctx, store, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("describe a missing filesystem: %v", err)
	}
}

// TestDirBlockEntriesSetting: a filesystem's directory block size is set at
// creation, kept in the master block, and used by every client that
// attaches; out of range is refused, and unset is the default.
func TestDirBlockEntriesSetting(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := CreateFilesystem(ctx, store, Settings{Name: "tiny", DirBlockEntries: 2}); err == nil {
		t.Fatal("2 entries per block was accepted")
	}
	info, err := CreateFilesystem(ctx, store, Settings{Name: "wide", DirBlockEntries: 16})
	if err != nil {
		t.Fatal(err)
	}
	if info.DirBlockEntries != 16 {
		t.Fatalf("created with %d, want 16", info.DirBlockEntries)
	}
	if info, err := CreateFilesystem(ctx, store, Settings{Name: "plain"}); err != nil || info.DirBlockEntries != testDirBlockEntries {
		t.Fatalf("unset: %d (%v), want the default", info.DirBlockEntries, err)
	}

	f := New(store, "wide")
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	root := f.RootKey()
	for i := 0; i < 16; i++ {
		if _, _, _, err := f.Create(ctx, root, fmt.Sprintf("f%02d", i), 0o644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if blk := readBlock(t, f, root); blk.Kind != block.Leaf {
		t.Fatal("split at 16 entries, want a split only past 16")
	}
	if _, _, _, err := f.Create(ctx, root, "f16", 0o644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if blk := readBlock(t, f, root); blk.Kind != block.Internal {
		t.Fatal("no split at 17 entries")
	}
}

// readBlock reads and decodes the directory block at key.
func readBlock(t *testing.T, f *Filesystem, key string) *block.Block {
	t.Helper()
	body, _, err := f.store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := block.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return blk
}
