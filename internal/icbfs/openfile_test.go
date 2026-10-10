package icbfs

import (
	"context"
	"sync"
	"testing"
)

// TestConcurrentNonOverlappingWritesBothSurvive covers ROADMAP.md's
// task B1 "Done when": two concurrent writers edit non-overlapping byte
// ranges of the same file, and both edits survive in the final
// content. Before task B1 (an unconditional Filesystem.WriteFile with
// no retry-and-reapply), this is exactly the lost-update bug
// ARCHITECTURE.md describes: whichever OpenFile.Flush lands second
// would silently overwrite the first one's byte range with its own
// stale copy of it. After B1 (OpenFile's CAS-protected
// retry-and-reapply), the loser re-fetches the winner's committed
// content and replays only its own recorded edit onto it, so both
// survive.
func TestConcurrentNonOverlappingWritesBothSurvive(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	const size = 20
	key, _, etag, err := fsys.Create(ctx, root, "shared.bin", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	base := make([]byte, size)
	if _, _, err := fsys.WriteFile(ctx, key, base, etag); err != nil {
		t.Fatalf("seed content: %v", err)
	}

	open1, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	open2, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}

	open1.WriteAt([]byte("AAAAA"), 0)  // bytes [0,5)
	open2.WriteAt([]byte("BBBBB"), 10) // bytes [10,15)

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = open1.Flush(ctx)
	}()
	go func() {
		defer wg.Done()
		_, err2 = open2.Flush(ctx)
	}()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("flush 1: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("flush 2: %v", err2)
	}

	final, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if len(final) != size {
		t.Fatalf("final size = %d, want %d", len(final), size)
	}
	if string(final[0:5]) != "AAAAA" {
		t.Fatalf("lost writer 1's edit: final[0:5] = %q, want %q", final[0:5], "AAAAA")
	}
	if string(final[10:15]) != "BBBBB" {
		t.Fatalf("lost writer 2's edit: final[10:15] = %q, want %q", final[10:15], "BBBBB")
	}
}
