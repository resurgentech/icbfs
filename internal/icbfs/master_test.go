package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestBootstrapMasterIsIdempotent covers D1's "Done when": a fresh
// bucket's master block can be bootstrapped, and bootstrapping an
// already-existing one is a no-op.
func TestBootstrapMasterIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := bootstrapMaster(ctx, store); err != nil {
		t.Fatalf("bootstrap master (fresh): %v", err)
	}
	before, obj, err := readMaster(ctx, store)
	if err != nil {
		t.Fatalf("read master: %v", err)
	}
	if len(before.Filesystems) != 0 {
		t.Fatalf("fresh master block should be empty, got %d entries", len(before.Filesystems))
	}

	if err := bootstrapMaster(ctx, store); err != nil {
		t.Fatalf("bootstrap master (already exists): %v", err)
	}
	after, obj2, err := readMaster(ctx, store)
	if err != nil {
		t.Fatalf("read master again: %v", err)
	}
	if len(after.Filesystems) != 0 {
		t.Fatalf("re-bootstrapping should not change the master block, got %d entries", len(after.Filesystems))
	}
	if obj.ETag != obj2.ETag {
		t.Fatalf("re-bootstrapping an existing master block should be a no-op, but its ETag changed: %s -> %s", obj.ETag, obj2.ETag)
	}
}

// TestRegisterFilesystemAllocatesSequentialIDs covers part of D2's
// "Done when": creating a filesystem allocates the expected ID.
func TestRegisterFilesystemAllocatesSequentialIDs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id0, archived, _, err := registerFilesystem(ctx, store, "fs-a", 1<<30, false)
	if err != nil {
		t.Fatalf("register fs-a: %v", err)
	}
	if id0 != "0000" || archived {
		t.Fatalf("fs-a = (%q, archived=%v), want (0000, false)", id0, archived)
	}

	id1, archived, _, err := registerFilesystem(ctx, store, "fs-b", 1<<30, false)
	if err != nil {
		t.Fatalf("register fs-b: %v", err)
	}
	if id1 != "0001" || archived {
		t.Fatalf("fs-b = (%q, archived=%v), want (0001, false)", id1, archived)
	}

	// Re-registering an existing name is idempotent: same ID back, no
	// new slot consumed.
	again, _, _, err := registerFilesystem(ctx, store, "fs-a", 999, false)
	if err != nil {
		t.Fatalf("re-register fs-a: %v", err)
	}
	if again != id0 {
		t.Fatalf("re-registering fs-a returned %q, want the original %q", again, id0)
	}
}

// TestRegisterFilesystemConcurrentCreatesGetDistinctIDs covers the rest
// of D2's "Done when": two concurrent creation attempts both succeed
// with distinct IDs (one retries after losing the CAS race, not both
// landing on the same slot).
func TestRegisterFilesystemConcurrentCreatesGetDistinctIDs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const n = 10
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _, _, err := registerFilesystem(ctx, store, fmt.Sprintf("concurrent-%02d", i), 1<<30, false)
			ids[i] = id
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("register concurrent-%02d: %v", i, err)
		}
		if seen[ids[i]] {
			t.Fatalf("ID %q was allocated to more than one filesystem", ids[i])
		}
		seen[ids[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct IDs, want %d", len(seen), n)
	}
}

// TestPruneFreesSlotForReuse covers D2's slot-reuse "Done when" and
// D5's "frees the slot for reuse" requirement together: after a
// filesystem is pruned, a new creation reuses that same freed slot
// rather than only ever appending.
func TestPruneFreesSlotForReuse(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	idA, _, _, err := registerFilesystem(ctx, store, "to-prune", 1<<30, false)
	if err != nil {
		t.Fatalf("register to-prune: %v", err)
	}
	fsys := New(store, "to-prune")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap to-prune: %v", err)
	}
	if _, _, _, err := fsys.Create(ctx, fsys.RootKey(), "somefile", 0644, 0, 0); err != nil {
		t.Fatalf("create file in to-prune: %v", err)
	}

	if err := Prune(ctx, store, "to-prune"); err != nil {
		t.Fatalf("prune: %v", err)
	}

	objs, err := store.ListByPrefix(ctx, idA+"-")
	if err != nil {
		t.Fatalf("list by prefix after prune: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("prune left %d objects behind under prefix %q, want 0", len(objs), idA+"-")
	}

	idB, _, _, err := registerFilesystem(ctx, store, "reuses-slot", 1<<30, false)
	if err != nil {
		t.Fatalf("register reuses-slot: %v", err)
	}
	if idB != idA {
		t.Fatalf("new filesystem got ID %q, want the freed slot %q reused", idB, idA)
	}
}

// TestIDPrefixingIsPresentOnDisk covers D3's "Done when": every key a
// filesystem writes for a file, a directory, and the root itself
// actually carries the expected "<id>-" prefix on disk — not just that
// the filesystem still works end to end.
func TestIDPrefixingIsPresentOnDisk(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	fsys := New(store, "prefixed")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	id := fsys.id
	wantPrefix := id + "-"

	if !strings.HasPrefix(fsys.RootKey(), wantPrefix) {
		t.Fatalf("root key %q does not carry prefix %q", fsys.RootKey(), wantPrefix)
	}

	fileUUID, _, _, err := fsys.Create(ctx, fsys.RootKey(), "myfile", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if !strings.HasPrefix(fileUUID, wantPrefix) {
		t.Fatalf("file key %q does not carry prefix %q", fileUUID, wantPrefix)
	}

	dirUUID, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "mydir", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !strings.HasPrefix(dirUUID, wantPrefix) {
		t.Fatalf("dir key %q does not carry prefix %q", dirUUID, wantPrefix)
	}

	objs, err := store.ListByPrefix(ctx, wantPrefix)
	if err != nil {
		t.Fatalf("list by prefix: %v", err)
	}
	foundRoot, foundFile, foundFileMeta, foundDir := false, false, false, false
	for _, o := range objs {
		switch {
		case o.Key == fsys.RootKey():
			foundRoot = true
		case o.Key == fileUUID:
			foundFile = true
		case o.Key == metadataObjectKey(fileUUID):
			foundFileMeta = true
		case o.Key == dirUUID:
			foundDir = true
		}
	}
	if !foundRoot || !foundFile || !foundFileMeta || !foundDir {
		t.Fatalf("prefix-scoped listing missing expected objects: root=%v file=%v fileMeta=%v dir=%v (listed %d objects)",
			foundRoot, foundFile, foundFileMeta, foundDir, len(objs))
	}
}

// TestArchivedFilesystemRejectsWritesButAllowsReads covers D5's "Done
// when" for archive enforcement: writes against an archived filesystem
// are rejected (checked at mount/session start, per
// Filesystem.checkWritable's doc comment), while reads still succeed.
func TestArchivedFilesystemRejectsWritesButAllowsReads(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	fsys := New(store, "to-archive")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	fileUUID, _, _, err := fsys.Create(ctx, fsys.RootKey(), "before-archive", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create before archive: %v", err)
	}
	if _, _, err := fsys.WriteFile(ctx, fileUUID, []byte("hello"), ""); err != nil {
		t.Fatalf("write before archive: %v", err)
	}

	if err := Archive(ctx, store, "to-archive"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// This Filesystem handle was opened before the archive and, per the
	// documented mount/session-start enforcement model, does not notice
	// it until the next Bootstrap.
	if _, _, _, err := fsys.ReadFile(ctx, fileUUID); err != nil {
		t.Fatalf("read on already-open handle after archive should still succeed: %v", err)
	}

	reopened := New(store, "to-archive")
	if err := reopened.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("re-bootstrap archived filesystem: %v", err)
	}

	if _, _, _, err := reopened.ReadFile(ctx, fileUUID); err != nil {
		t.Fatalf("read against archived filesystem should succeed, got: %v", err)
	}
	if _, _, _, err := reopened.Create(ctx, reopened.RootKey(), "after-archive", 0644, 0, 0); err != ErrArchived {
		t.Fatalf("create against archived filesystem = %v, want ErrArchived", err)
	}
	if _, _, err := reopened.WriteFile(ctx, fileUUID, []byte("nope"), ""); err != ErrArchived {
		t.Fatalf("write against archived filesystem = %v, want ErrArchived", err)
	}
	if _, _, err := reopened.Mkdir(ctx, reopened.RootKey(), "nope-dir", 0755, 0, 0); err != ErrArchived {
		t.Fatalf("mkdir against archived filesystem = %v, want ErrArchived", err)
	}
	if err := reopened.Unlink(ctx, reopened.RootKey(), "before-archive"); err != ErrArchived {
		t.Fatalf("unlink against archived filesystem = %v, want ErrArchived", err)
	}
}

// TestPrimaryWindowsSetAtCreationAndImmutableAfterward covers
// ROADMAP.md's Part F, task F5: a filesystem's primary-mode is
// specified at creation and correctly retrievable afterward — and,
// matching size/mode/uid/gid's existing precedent for "only used the
// first time a filesystem name is created," a later Bootstrap call
// passing a *different* value for an already-registered name does not
// change the stored one.
func TestPrimaryWindowsSetAtCreationAndImmutableAfterward(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	winFS := New(store, "win-primary")
	if err := winFS.Bootstrap(ctx, 1<<30, 0755, 0, 0, true); err != nil {
		t.Fatalf("bootstrap primary-Windows: %v", err)
	}
	if !winFS.PrimaryWindows() {
		t.Fatalf("PrimaryWindows() = false, want true")
	}

	posixFS := New(store, "posix-primary")
	if err := posixFS.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap primary-POSIX: %v", err)
	}
	if posixFS.PrimaryWindows() {
		t.Fatalf("PrimaryWindows() = true, want false")
	}

	// Re-bootstrapping the same (already-registered) name with the
	// opposite value must not change what's stored — same idempotency
	// rule size already follows.
	reopened := New(store, "win-primary")
	if err := reopened.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("re-bootstrap primary-Windows filesystem: %v", err)
	}
	if !reopened.PrimaryWindows() {
		t.Fatalf("re-bootstrapped PrimaryWindows() = false, want true (the originally-stored value, not the second call's argument)")
	}
}

// TestResizeChangesDeclaredSizeForEveryMount covers the Resize
// operation added at Jared's direction (see ASSUMPTIONS.md's
// D-cleanup entry, which originally flagged there being no way to
// change a filesystem's declared size after creation): unlike
// EnableLocking's per-mount, in-memory flag, Resize changes a real,
// shared master-block property — a *different*, already-open handle
// on the same filesystem name must see the new size on its very next
// StatFS, with no re-Bootstrap required (StatFS always re-reads the
// master block fresh, per its own implementation).
func TestResizeChangesDeclaredSizeForEveryMount(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const initialSize = 1 << 30
	fsys := New(store, "to-resize")
	if err := fsys.Bootstrap(ctx, initialSize, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	otherMount := New(store, "to-resize")
	if err := otherMount.Bootstrap(ctx, initialSize, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap other mount: %v", err)
	}

	total, _, err := fsys.StatFS(ctx)
	if err != nil {
		t.Fatalf("statfs before resize: %v", err)
	}
	if total != initialSize {
		t.Fatalf("total before resize = %d, want %d", total, initialSize)
	}

	const newSize = 5 << 30
	if err := Resize(ctx, store, "to-resize", newSize); err != nil {
		t.Fatalf("resize: %v", err)
	}

	total, _, err = fsys.StatFS(ctx)
	if err != nil {
		t.Fatalf("statfs after resize (original handle): %v", err)
	}
	if total != newSize {
		t.Fatalf("total after resize (original handle) = %d, want %d", total, newSize)
	}

	total, _, err = otherMount.StatFS(ctx)
	if err != nil {
		t.Fatalf("statfs after resize (other mount): %v", err)
	}
	if total != newSize {
		t.Fatalf("total after resize (other mount) = %d, want %d", total, newSize)
	}
}

func TestResizeOfUnknownFilesystemReturnsNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := Resize(ctx, store, "never-created", 1<<30); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resize of unknown filesystem = %v, want ErrNotFound", err)
	}
}

// TestPruneRemovesEveryObjectUnderPrefix covers D5's "Done when" for
// prune: pruning actually removes every object under the prefix,
// confirmed by listing after, not just trusting the delete calls
// didn't error.
func TestPruneRemovesEveryObjectUnderPrefix(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	fsys := New(store, "prune-me")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	id := fsys.id

	for i := 0; i < 5; i++ {
		if _, _, _, err := fsys.Create(ctx, fsys.RootKey(), fmt.Sprintf("file-%d", i), 0644, 0, 0); err != nil {
			t.Fatalf("create file-%d: %v", i, err)
		}
	}
	if _, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "subdir", 0755, 0, 0); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	before, err := store.ListByPrefix(ctx, id+"-")
	if err != nil {
		t.Fatalf("list before prune: %v", err)
	}
	if len(before) == 0 {
		t.Fatalf("expected objects under prefix before prune, got none")
	}

	if err := Prune(ctx, store, "prune-me"); err != nil {
		t.Fatalf("prune: %v", err)
	}

	after, err := store.ListByPrefix(ctx, id+"-")
	if err != nil {
		t.Fatalf("list after prune: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("prune left %d objects behind under prefix %q, want 0", len(after), id+"-")
	}

	mb, _, err := readMaster(ctx, store)
	if err != nil {
		t.Fatalf("read master after prune: %v", err)
	}
	idx, err := parseFsID(id)
	if err != nil {
		t.Fatalf("parse id %q: %v", id, err)
	}
	if mb.Filesystems[idx].Name != "" {
		t.Fatalf("pruned entry's name = %q, want empty (tombstoned)", mb.Filesystems[idx].Name)
	}
}

// TestStatFSIsScopedPerFilesystem covers D4's "Done when": two
// filesystems in the same bucket, with differently-sized content, each
// get a "Used" figure reflecting only their own content, not the
// other's — the test that actually proves the multi-tenant scoping
// problem is solved, not just that a number comes back.
func TestStatFSIsScopedPerFilesystem(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const declaredSize = 1 << 30
	fsA := New(store, "tenant-a")
	if err := fsA.Bootstrap(ctx, declaredSize, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap tenant-a: %v", err)
	}
	fsB := New(store, "tenant-b")
	if err := fsB.Bootstrap(ctx, declaredSize, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap tenant-b: %v", err)
	}

	smallContent := bytes.Repeat([]byte("a"), 100)
	bigContent := bytes.Repeat([]byte("b"), 10000)

	fileA, _, _, err := fsA.Create(ctx, fsA.RootKey(), "small", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create in tenant-a: %v", err)
	}
	if _, _, err := fsA.WriteFile(ctx, fileA, smallContent, ""); err != nil {
		t.Fatalf("write in tenant-a: %v", err)
	}

	fileB, _, _, err := fsB.Create(ctx, fsB.RootKey(), "big", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create in tenant-b: %v", err)
	}
	if _, _, err := fsB.WriteFile(ctx, fileB, bigContent, ""); err != nil {
		t.Fatalf("write in tenant-b: %v", err)
	}

	totalA, usedA, err := fsA.StatFS(ctx)
	if err != nil {
		t.Fatalf("statfs tenant-a: %v", err)
	}
	totalB, usedB, err := fsB.StatFS(ctx)
	if err != nil {
		t.Fatalf("statfs tenant-b: %v", err)
	}

	if totalA != declaredSize || totalB != declaredSize {
		t.Fatalf("declared totals = (%d, %d), want both %d", totalA, totalB, uint64(declaredSize))
	}
	if usedA >= usedB {
		t.Fatalf("tenant-a (small file) Used=%d should be less than tenant-b (big file) Used=%d — each should only see its own content", usedA, usedB)
	}
	if usedA < uint64(len(smallContent)) {
		t.Fatalf("tenant-a Used=%d is smaller than its own file content (%d bytes)", usedA, len(smallContent))
	}
	if usedB < uint64(len(bigContent)) {
		t.Fatalf("tenant-b Used=%d is smaller than its own file content (%d bytes)", usedB, len(bigContent))
	}

	duA, err := fsA.DiskUsage(ctx)
	if err != nil {
		t.Fatalf("disk usage tenant-a: %v", err)
	}
	if uint64(duA) != usedA {
		t.Fatalf("DiskUsage()=%d should match StatFS's Used=%d", duA, usedA)
	}
}

func parseFsID(id string) (int, error) {
	var idx int
	_, err := fmt.Sscanf(id, "%04x", &idx)
	return idx, err
}
