package icbfs

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

// TestFlushEscalatesWhenPlainRetryBudgetIsExhausted covers ROADMAP.md's
// task B5 "Done when" deterministically, rather than relying on timing
// -sensitive luck to ever actually observe real exhaustion in a short
// test run: contentWriteRetryBudget is forced to zero, and a
// concurrent write is landed on the object *before* Flush ever gets a
// chance to retry — guaranteeing its one and only plain CAS attempt
// loses the race, with zero budget left to retry. Without task B5's
// escalation, Flush would have no option left but to return
// ErrWriteContention right there, every single time this happens —
// exactly the "one writer thrashing against the other's retries"
// failure mode ARCHITECTURE.md describes. With escalation, Flush
// instead blocks on a lock, rebases onto the concurrent writer's
// content, and lands its own edit anyway: real forward progress
// despite the exhausted plain-retry budget.
func TestFlushEscalatesWhenPlainRetryBudgetIsExhausted(t *testing.T) {
	orig := contentWriteRetryBudget
	contentWriteRetryBudget = 0
	defer func() { contentWriteRetryBudget = orig }()

	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()
	key, _, _, err := fsys.Create(ctx, root, "contended.bin", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	open, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	open.WriteAt([]byte("hello"), 0)

	// A concurrent writer (not going through this OpenFile at all)
	// lands its own change on the object before Flush is called —
	// open's pristine ETag is now stale, so its first (and, with a
	// zero budget, only) plain CAS attempt is guaranteed to fail.
	concurrent := []byte("someone-else-wrote-this")
	if _, _, err := fsys.WriteFile(ctx, key, concurrent, ""); err != nil {
		t.Fatalf("simulate a concurrent writer: %v", err)
	}

	attr, err := open.Flush(ctx)
	if err != nil {
		t.Fatalf("flush with an exhausted plain-retry budget = %v, want nil (escalation should recover it)", err)
	}
	if attr.Size != int64(len(concurrent)) {
		t.Fatalf("flushed size = %d, want %d", attr.Size, len(concurrent))
	}

	// The successful write should be the concurrent writer's content
	// with this session's edit replayed onto it — proof Flush actually
	// rebased before its final write, not that it just happened to
	// land something.
	final, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	want := append([]byte("hello"), concurrent[len("hello"):]...)
	if !bytes.Equal(final, want) {
		t.Fatalf("final content = %q, want %q", final, want)
	}
}

// TestFlushRefusedWhenTouchedRangeOverlapsExternalLock covers
// ROADMAP.md's task B6 "Done when": an explicit lock held by one
// simulated client over a byte range causes a second simulated
// client's overlapping write to be refused, while that second
// client's write to a genuinely non-overlapping range still succeeds
// normally while the first client's lock is still held.
func TestFlushRefusedWhenTouchedRangeOverlapsExternalLock(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()
	key, _, _, err := fsys.Create(ctx, root, "cooperative.bin", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, make([]byte, 20), ""); err != nil {
		t.Fatalf("seed content: %v", err)
	}

	// Simulated client 1 places a real, explicit application lock
	// (e.g. via fcntl/LockFileEx, once task B8 wires that up) over
	// [0, 10).
	if err := fsys.TryAcquireLockRange(ctx, key, 0, 10, "client-1", 30*time.Second); err != nil {
		t.Fatalf("client-1 acquire [0,10): %v", err)
	}

	// Simulated client 2's write overlapping that locked range must be
	// refused outright, not retried/escalated — this is someone else's
	// cooperating lock, not same-codebase contention.
	overlapping, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("client-2 open: %v", err)
	}
	overlapping.WriteAt([]byte("XXXXX"), 5) // [5,10) overlaps client-1's [0,10)
	if _, err := overlapping.Flush(ctx); !errors.Is(err, ErrLocked) {
		t.Fatalf("client-2 overlapping write = %v, want ErrLocked", err)
	}

	// Client 2's write to a genuinely non-overlapping range succeeds
	// normally while client-1's lock is still held.
	nonOverlapping, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("client-2 open (second session): %v", err)
	}
	nonOverlapping.WriteAt([]byte("YYYYY"), 15) // [15,20), disjoint from [0,10)
	if _, err := nonOverlapping.Flush(ctx); err != nil {
		t.Fatalf("client-2 non-overlapping write = %v, want nil", err)
	}

	final, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if string(final[15:20]) != "YYYYY" {
		t.Fatalf("non-overlapping write did not land: final[15:20] = %q, want %q", final[15:20], "YYYYY")
	}
	if string(final[5:10]) == "XXXXX" {
		t.Fatalf("overlapping write landed despite being refused: final[5:10] = %q", final[5:10])
	}
}

// TestCheckRangeLockConflictIgnoresEscalationOnlyEntries confirms the
// specific reason LockRange.EscalationOnly exists: another session's
// own in-flight B5 escalation claim must never be mistaken for a real
// external application lock by B6's check — doing so would spuriously
// refuse writes that tasks B1/B5 already guarantee converge on their
// own shortly after.
func TestCheckRangeLockConflictIgnoresEscalationOnlyEntries(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()
	key, _, _, err := fsys.Create(ctx, root, "f.bin", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := fsys.acquireEscalationLockRange(ctx, key, 0, 5, "some-other-session", 30*time.Second); err != nil {
		t.Fatalf("simulate an in-flight escalation claim: %v", err)
	}
	defer fsys.ReleaseLockRange(ctx, key, 0, 5, "some-other-session")

	if err := fsys.CheckRangeLockConflict(ctx, key, 0, 5, "yet-another-holder"); err != nil {
		t.Fatalf("check against an escalation-only claim = %v, want nil (not a real application lock)", err)
	}
}
