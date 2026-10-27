package icbfs

import (
	"context"
	"errors"
	"testing"
)

func writeNew(t *testing.T, ctx context.Context, f *Filesystem, dir, name, data string) string {
	t.Helper()
	key, _, etag, err := f.Create(ctx, dir, name, 0644, 0, 0)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := f.WriteFile(ctx, key, []byte(data), etag); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return key
}

func readName(t *testing.T, ctx context.Context, f *Filesystem, dir, name string) string {
	t.Helper()
	e, _, err := f.Lookup(ctx, dir, name)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	data, _, _, err := f.ReadFile(ctx, e.UUID)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestTrashRestoreKeepsHistory: a deleted file is listed in the trash with
// who and where; restoring it brings back its content under its old name
// and the same id, and its version history carries on from before.
func TestTrashRestoreKeepsHistory(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()

	key := writeNew(t, ctx, f, root, "a.txt", "one")
	e, _, _ := f.Lookup(ctx, root, "a.txt")
	if _, err := f.WriteFile(ctx, key, []byte("two"), ""); err != nil {
		t.Fatal(err)
	}
	before, _ := store.History(ctx, key)

	dctx := WithDeleteInfo(ctx, DeleteInfo{By: "uid:7", Path: "/a.txt"})
	if err := f.Unlink(dctx, root, "a.txt"); err != nil {
		t.Fatal(err)
	}
	trash, err := f.ListTrash(ctx, TrashOptions{})
	if err != nil || len(trash) != 1 {
		t.Fatalf("trash = %v, %v; want one entry", trash, err)
	}
	te := trash[0]
	if te.ItemID != e.UUID || te.Name != "a.txt" || te.DeletedBy != "uid:7" || te.Path != "/a.txt" || te.Size != 3 || te.Version == "" {
		t.Fatalf("trash entry = %+v", te)
	}

	if _, err := f.Restore(ctx, te.EntryID, RestoreOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readName(t, ctx, f, root, "a.txt"); got != "two" {
		t.Fatalf("restored content = %q", got)
	}
	e2, attr, _ := f.Lookup(ctx, root, "a.txt")
	if e2.UUID != e.UUID || attr.Nlink != 1 {
		t.Fatalf("restored id %s nlink %d; want %s, 1", e2.UUID, attr.Nlink, e.UUID)
	}
	after, _ := store.History(ctx, key)
	if len(after) != len(before) {
		t.Fatalf("history: %d versions after restore, %d before", len(after), len(before))
	}
	if trash, _ := f.ListTrash(ctx, TrashOptions{}); len(trash) != 0 {
		t.Fatalf("trash after restore = %v", trash)
	}
	if _, err := f.WriteFile(ctx, key, []byte("three"), ""); err != nil {
		t.Fatalf("write after restore: %v", err)
	}
}

// TestTrashRestoreElsewhereAndConflicts: restoring onto a taken name is
// ErrExists unless Replace (which trashes what was there); a restore can
// go to another directory and name.
func TestTrashRestoreElsewhereAndConflicts(t *testing.T) {
	f, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()

	writeNew(t, ctx, f, root, "a.txt", "old")
	if err := f.Unlink(ctx, root, "a.txt"); err != nil {
		t.Fatal(err)
	}
	writeNew(t, ctx, f, root, "a.txt", "new")
	trash, _ := f.ListTrash(ctx, TrashOptions{})
	id := trash[0].EntryID

	if _, err := f.Restore(ctx, id, RestoreOptions{}); !errors.Is(err, ErrExists) {
		t.Fatalf("restore onto a taken name: %v; want ErrExists", err)
	}
	sub, _, err := f.Mkdir(ctx, root, "sub", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Restore(ctx, id, RestoreOptions{ToDir: sub, ToName: "b.txt"}); err != nil {
		t.Fatalf("restore elsewhere: %v", err)
	}
	if got := readName(t, ctx, f, sub, "b.txt"); got != "old" {
		t.Fatalf("restored elsewhere = %q", got)
	}

	// Replace: the "new" a.txt goes to the trash, the old one comes back.
	if err := f.Unlink(ctx, sub, "b.txt"); err != nil {
		t.Fatal(err)
	}
	trash, _ = f.ListTrash(ctx, TrashOptions{})
	if _, err := f.Restore(ctx, trash[0].EntryID, RestoreOptions{ToDir: root, ToName: "a.txt", Replace: true}); err != nil {
		t.Fatalf("restore replacing: %v", err)
	}
	if got := readName(t, ctx, f, root, "a.txt"); got != "old" {
		t.Fatalf("after replace = %q", got)
	}
	trash, _ = f.ListTrash(ctx, TrashOptions{})
	if len(trash) != 1 || trash[0].Name != "a.txt" {
		t.Fatalf("trash after replace = %+v; want the replaced a.txt", trash)
	}
}

// TestTrashOnlyLastName: unlinking one of two hard links isn't a delete.
func TestTrashOnlyLastName(t *testing.T) {
	f, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()

	key := writeNew(t, ctx, f, root, "a", "x")
	if _, err := f.Link(ctx, root, "b", key, TypeFile); err != nil {
		t.Fatal(err)
	}
	if err := f.Unlink(ctx, root, "a"); err != nil {
		t.Fatal(err)
	}
	if trash, _ := f.ListTrash(ctx, TrashOptions{}); len(trash) != 0 {
		t.Fatalf("trash after unlinking one of two names = %v", trash)
	}
	if err := f.Unlink(ctx, root, "b"); err != nil {
		t.Fatal(err)
	}
	if trash, _ := f.ListTrash(ctx, TrashOptions{}); len(trash) != 1 || trash[0].Name != "b" {
		t.Fatalf("trash = %+v; want b", trash)
	}
}

// TestTrashDirectoryAndRenameReplace: an rmdir'd directory comes back
// (with its metadata); a file a rename replaced is in the trash; a
// placeholder delete (NoTrash) isn't.
func TestTrashDirectoryAndRenameReplace(t *testing.T) {
	f, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()

	if _, _, err := f.Mkdir(ctx, root, "d", 0700, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Rmdir(ctx, root, "d"); err != nil {
		t.Fatal(err)
	}
	trash, _ := f.ListTrash(ctx, TrashOptions{})
	if len(trash) != 1 || trash[0].Type != TypeDir {
		t.Fatalf("trash = %+v", trash)
	}
	if _, err := f.Restore(ctx, trash[0].EntryID, RestoreOptions{}); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	_, attr, err := f.Lookup(ctx, root, "d")
	if err != nil || attr.Mode&0777 != 0700 {
		t.Fatalf("restored dir: mode %o, %v", attr.Mode, err)
	}

	writeNew(t, ctx, f, root, "x", "replaced")
	writeNew(t, ctx, f, root, "y", "mover")
	if err := f.Rename(ctx, root, "y", root, "x", true); err != nil {
		t.Fatal(err)
	}
	trash, _ = f.ListTrash(ctx, TrashOptions{})
	if len(trash) != 1 || trash[0].Name != "x" || trash[0].Size != 8 {
		t.Fatalf("trash after rename-replace = %+v", trash)
	}

	writeNew(t, ctx, f, root, "p", "")
	if err := f.Unlink(WithDeleteInfo(ctx, DeleteInfo{NoTrash: true}), root, "p"); err != nil {
		t.Fatal(err)
	}
	if trash, _ := f.ListTrash(ctx, TrashOptions{}); len(trash) != 1 {
		t.Fatalf("NoTrash delete was recorded: %+v", trash)
	}
}

// TestTrashDeletedWhileOpen: a file unlinked while open is in the trash
// right away, and restoring it before the close calls the deletion off.
func TestTrashDeletedWhileOpen(t *testing.T) {
	f, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()

	key := writeNew(t, ctx, f, root, "o", "open")
	of, err := f.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Unlink(ctx, root, "o"); err != nil {
		t.Fatal(err)
	}
	trash, _ := f.ListTrash(ctx, TrashOptions{})
	if len(trash) != 1 {
		t.Fatalf("trash = %+v", trash)
	}
	if _, err := f.Restore(ctx, trash[0].EntryID, RestoreOptions{}); err != nil {
		t.Fatalf("restore while open: %v", err)
	}
	if err := of.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readName(t, ctx, f, root, "o"); got != "open" {
		t.Fatalf("after close = %q", got)
	}
}

// TestTrashRecordsOrigin: however an item goes (unlink, rmdir, or replaced
// by a rename, as through a mount), its trash entry records the origin
// app data it had, so a mirror can find it when the source brings the
// item back; an item with none records none.
func TestTrashRecordsOrigin(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()
	withOrigin := func(key string, typ EntryType, o string) {
		t.Helper()
		if err := fsys.SetAppData(ctx, key, typ, AppDriveOrigin, []byte(o)); err != nil {
			t.Fatal(err)
		}
	}
	file := func(name, o string) {
		t.Helper()
		key, _, _, err := fsys.Create(ctx, root, name, 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if o != "" {
			withOrigin(key, TypeFile, o)
		}
	}
	dir := func(name, o string) {
		t.Helper()
		key, _, err := fsys.Mkdir(ctx, root, name, 0755, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		withOrigin(key, TypeDir, o)
	}
	file("unlinked", `{"id":"u"}`)
	dir("removed", `{"id":"r"}`)
	file("replaced", `{"id":"f"}`)
	file("newer", "")
	dir("replaced-dir", `{"id":"d"}`)
	dir("newer-dir", `{"id":"n"}`)
	file("plain", "")

	if err := fsys.Unlink(ctx, root, "unlinked"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Rmdir(ctx, root, "removed"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Rename(ctx, root, "newer", root, "replaced", true); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Rename(ctx, root, "newer-dir", root, "replaced-dir", true); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Unlink(ctx, root, "plain"); err != nil {
		t.Fatal(err)
	}

	entries, err := fsys.ListTrash(ctx, TrashOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = string(e.Origin)
	}
	want := map[string]string{
		"unlinked": `{"id":"u"}`, "removed": `{"id":"r"}`,
		"replaced": `{"id":"f"}`, "replaced-dir": `{"id":"d"}`, "plain": "",
	}
	for name, o := range want {
		if g, ok := got[name]; !ok || g != o {
			t.Errorf("%s: trash entry origin %q (present %v), want %q", name, g, ok, o)
		}
	}
}
