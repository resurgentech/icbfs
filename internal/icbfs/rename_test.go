package icbfs

import (
	"context"
	"errors"
	"testing"
)

// TestRenameWithinSameDirectory covers the simplest case: a plain
// name change, no directory move.
func TestRenameWithinSameDirectory(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "old.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := fsys.Rename(ctx, root, "old.txt", root, "new.txt", false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, _, err := fsys.Lookup(ctx, root, "old.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old name still found: %v", err)
	}
	entry, _, err := fsys.Lookup(ctx, root, "new.txt")
	if err != nil {
		t.Fatalf("lookup new name: %v", err)
	}
	if entry.UUID != uuid {
		t.Fatalf("UUID changed across rename: got %s, want %s", entry.UUID, uuid)
	}
}

// TestRenameAcrossDirectories moves an entry to a different parent.
func TestRenameAcrossDirectories(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	dirUUID, _, err := fsys.Mkdir(ctx, root, "dest", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fileUUID, _, _, err := fsys.Create(ctx, root, "f.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := fsys.Rename(ctx, root, "f.txt", dirUUID, "f.txt", false); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, _, err := fsys.Lookup(ctx, root, "f.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source still found in old directory: %v", err)
	}
	entry, _, err := fsys.Lookup(ctx, dirUUID, "f.txt")
	if err != nil {
		t.Fatalf("lookup in new directory: %v", err)
	}
	if entry.UUID != fileUUID {
		t.Fatalf("UUID changed across rename: got %s, want %s", entry.UUID, fileUUID)
	}
}

// TestRenameOntoExistingWithoutReplaceFails covers RENAME_NOREPLACE
// semantics.
func TestRenameOntoExistingWithoutReplaceFails(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	if _, _, _, err := fsys.Create(ctx, root, "a.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, _, _, err := fsys.Create(ctx, root, "b.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create b: %v", err)
	}
	if err := fsys.Rename(ctx, root, "a.txt", root, "b.txt", false); !errors.Is(err, ErrExists) {
		t.Fatalf("rename without replace onto existing = %v, want ErrExists", err)
	}
	// Both names must still exist, untouched.
	if _, _, err := fsys.Lookup(ctx, root, "a.txt"); err != nil {
		t.Fatalf("a.txt disturbed by failed rename: %v", err)
	}
	if _, _, err := fsys.Lookup(ctx, root, "b.txt"); err != nil {
		t.Fatalf("b.txt disturbed by failed rename: %v", err)
	}
}

// TestRenameOntoExistingFileWithReplaceOverwrites covers the common
// rsync/git-style "write a temp file, rename it into place" pattern.
func TestRenameOntoExistingFileWithReplaceOverwrites(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	srcUUID, _, _, err := fsys.Create(ctx, root, "src.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	dstUUID, _, _, err := fsys.Create(ctx, root, "dst.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create dst: %v", err)
	}
	if err := fsys.Rename(ctx, root, "src.txt", root, "dst.txt", true); err != nil {
		t.Fatalf("rename with replace: %v", err)
	}
	if _, _, err := fsys.Lookup(ctx, root, "src.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("src.txt still found after rename: %v", err)
	}
	entry, _, err := fsys.Lookup(ctx, root, "dst.txt")
	if err != nil {
		t.Fatalf("lookup dst.txt: %v", err)
	}
	if entry.UUID != srcUUID {
		t.Fatalf("dst.txt UUID = %s, want src's %s", entry.UUID, srcUUID)
	}
	// The old dst.txt object itself should be gone (nlink dropped to 0
	// and deleted) — Stat on its old UUID should now fail.
	if _, err := fsys.Stat(ctx, dstUUID, TypeFile); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old dst.txt object still exists after being replaced: %v", err)
	}
}

// TestRenameFileOntoDirectoryFails matches rename(2)'s EISDIR.
func TestRenameFileOntoDirectoryFails(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	if _, _, _, err := fsys.Create(ctx, root, "f.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := fsys.Mkdir(ctx, root, "d", 0755, 0, 0); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := fsys.Rename(ctx, root, "f.txt", root, "d", true); !errors.Is(err, ErrIsDir) {
		t.Fatalf("rename file onto directory = %v, want ErrIsDir", err)
	}
}

// TestRenameDirectoryOntoFileFails matches rename(2)'s ENOTDIR.
func TestRenameDirectoryOntoFileFails(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	if _, _, err := fsys.Mkdir(ctx, root, "d", 0755, 0, 0); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, _, err := fsys.Create(ctx, root, "f.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := fsys.Rename(ctx, root, "d", root, "f.txt", true); !errors.Is(err, ErrNotDir) {
		t.Fatalf("rename directory onto file = %v, want ErrNotDir", err)
	}
}

// TestRenameDirectoryOntoNonEmptyDirectoryFails matches rename(2)'s
// ENOTEMPTY.
func TestRenameDirectoryOntoNonEmptyDirectoryFails(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	if _, _, err := fsys.Mkdir(ctx, root, "src", 0755, 0, 0); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	dstUUID, _, err := fsys.Mkdir(ctx, root, "dst", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	if _, _, _, err := fsys.Create(ctx, dstUUID, "child.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := fsys.Rename(ctx, root, "src", root, "dst", true); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("rename directory onto non-empty directory = %v, want ErrNotEmpty", err)
	}
}

// TestRenameDirectoryOntoEmptyDirectorySucceeds confirms the
// replace=true, both-sides-directories, empty-target case actually
// works (the "it isn't ENOTEMPTY" case TestRenameDirectoryOntoNonEmptyDirectoryFails
// doesn't cover).
func TestRenameDirectoryOntoEmptyDirectorySucceeds(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	srcUUID, _, err := fsys.Mkdir(ctx, root, "src", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	dstUUID, _, err := fsys.Mkdir(ctx, root, "dst", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	if err := fsys.Rename(ctx, root, "src", root, "dst", true); err != nil {
		t.Fatalf("rename directory onto empty directory: %v", err)
	}
	entry, _, err := fsys.Lookup(ctx, root, "dst")
	if err != nil {
		t.Fatalf("lookup dst: %v", err)
	}
	if entry.UUID != srcUUID {
		t.Fatalf("dst UUID = %s, want src's %s", entry.UUID, srcUUID)
	}
	if _, err := fsys.Stat(ctx, dstUUID, TypeDir); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old dst directory object still exists: %v", err)
	}
}

// TestRenameDirectoryIntoOwnSubdirectoryFails covers the
// dirContains/EINVAL check — the classic "mv a a/b" bug.
func TestRenameDirectoryIntoOwnSubdirectoryFails(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	parentUUID, _, err := fsys.Mkdir(ctx, root, "parent", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	childUUID, _, err := fsys.Mkdir(ctx, parentUUID, "child", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	// Move "parent" to become a new entry inside its own child
	// "child" — directly into one of its own descendants.
	if err := fsys.Rename(ctx, root, "parent", childUUID, "parent-moved", false); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("rename directory into own descendant = %v, want ErrInvalidRename", err)
	}
	// Moving "parent" to be renamed as itself (newDirKey == parent's
	// own UUID) is equally invalid.
	if err := fsys.Rename(ctx, root, "parent", parentUUID, "self", false); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("rename directory into itself = %v, want ErrInvalidRename", err)
	}
	// Both directories must be completely untouched after the refused
	// attempts.
	if _, _, err := fsys.Lookup(ctx, root, "parent"); err != nil {
		t.Fatalf("parent disturbed: %v", err)
	}
	if _, _, err := fsys.Lookup(ctx, parentUUID, "child"); err != nil {
		t.Fatalf("child disturbed: %v", err)
	}
}

// TestRenameNonexistentSourceFails covers the plain not-found case.
func TestRenameNonexistentSourceFails(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	if err := fsys.Rename(ctx, root, "does-not-exist.txt", root, "whatever.txt", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename nonexistent source = %v, want ErrNotFound", err)
	}
}

// TestRenameSameNameAndDirectoryIsNoop covers the identity/self-rename
// fast path.
func TestRenameSameNameAndDirectoryIsNoop(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "same.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := fsys.Rename(ctx, root, "same.txt", root, "same.txt", false); err != nil {
		t.Fatalf("self-rename: %v", err)
	}
	entry, _, err := fsys.Lookup(ctx, root, "same.txt")
	if err != nil {
		t.Fatalf("lookup after self-rename: %v", err)
	}
	if entry.UUID != uuid {
		t.Fatalf("UUID changed by self-rename: got %s, want %s", entry.UUID, uuid)
	}
}

// TestRenameOntoHardlinkedSameObjectIsNoop covers POSIX rename(2)'s
// documented special case: "If oldpath and newpath are existing hard
// links referring to the same file, then rename() does nothing, and
// returns a success status" — regardless of the replace flag.
func TestRenameOntoHardlinkedSameObjectIsNoop(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "a.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := fsys.Link(ctx, root, "b.txt", uuid, TypeFile); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := fsys.Rename(ctx, root, "a.txt", root, "b.txt", false); err != nil {
		t.Fatalf("rename onto hardlinked same object (replace=false): %v", err)
	}
	// Both names must still resolve to the same object afterward —
	// genuinely nothing happened.
	for _, name := range []string{"a.txt", "b.txt"} {
		entry, _, err := fsys.Lookup(ctx, root, name)
		if err != nil {
			t.Fatalf("lookup %s after no-op rename: %v", name, err)
		}
		if entry.UUID != uuid {
			t.Fatalf("%s UUID = %s, want %s", name, entry.UUID, uuid)
		}
	}
}

// TestRenameRejectsReservedNameOnPrimaryWindows confirms Rename goes
// through the same checkName gate Create/Mkdir/Symlink/Link already
// do, and that a primary-POSIX filesystem allows it.
func TestRenameRejectsReservedNameOnPrimaryWindows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	winFS := New(store, "win-rename")
	if err := winFS.Bootstrap(ctx, 1<<30, 0755, 0, 0, true); err != nil {
		t.Fatalf("bootstrap primary-Windows: %v", err)
	}
	winRoot := winFS.RootKey()
	if _, _, _, err := winFS.Create(ctx, winRoot, "f.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := winFS.Rename(ctx, winRoot, "f.txt", winRoot, "CON", false); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("primary-Windows rename onto CON = %v, want ErrInvalidName", err)
	}

	posixFS := New(store, "posix-rename")
	if err := posixFS.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap primary-POSIX: %v", err)
	}
	posixRoot := posixFS.RootKey()
	if _, _, _, err := posixFS.Create(ctx, posixRoot, "f.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := posixFS.Rename(ctx, posixRoot, "f.txt", posixRoot, "CON", false); err != nil {
		t.Fatalf("primary-POSIX rename onto CON = %v, want nil", err)
	}
}
