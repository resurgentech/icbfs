package icbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
	fsys.EnableLocking(true)
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
	fsys.EnableLocking(true)
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
	fsys.EnableLocking(true)
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

// TestWriteAtRejectsHugeOffsetInsteadOfCrashing is a real regression
// test found running pjdfstest's full suite against a real,
// memory-constrained Linux VM: a write far past the current end of a
// file (pjdfstest's `open` category tests pwrite-style sparse-file/
// 32-bit-off_t-overflow behavior, specifically a 1-byte write at
// offset 2147483649 = 2^31+1) made applyLocked's make([]byte, end) try
// to materialize the entire ~2GB logical gap as one real allocation —
// this implementation has no sparse-region support at all. On a 4GB
// test VM already running a long pjdfstest suite, that allocation got
// the whole process OOM-killed by the kernel outright (no Go panic at
// all — SIGKILL can't be recovered), which is strictly worse than
// SetAttr's "makeslice: len out of range" panic for an equally huge
// *truncate* request (ROADMAP.md/TESTING.md's own already-fixed
// finding): a panic at least stops only the one request; this took
// down every other in-flight operation on the same mount too.
// TestOpenFileTouchAtimeOnlyOncePerSession is a real regression test:
// a single large read gets split by the kernel into many smaller
// FUSE/WinFsp Read callback invocations, and an earlier version of
// OpenFile.TouchAtime called Filesystem.TouchAtime — a real network
// metadata PUT — on every single one of them, turning one 500 MB
// read into thousands of synchronous round trips and hanging a real
// mount in practice (found running task G3's own VM verification,
// not hypothetical). Confirmed here as a plain call-count assertion,
// not a timing-based test, since the real-world symptom (a hang) is
// exactly what a slow CI run must never risk reproducing.
func TestOpenFileTouchAtimeOnlyOncePerSession(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	key, _, _, err := fsys.Create(ctx, root, "touch-once.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	open, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	before, err := fsys.Stat(ctx, key, TypeFile)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	if !before.Atime.IsZero() {
		t.Fatalf("freshly created file's Atime = %v, want zero", before.Atime)
	}

	open.mu.Lock()
	if open.atimeTouched {
		open.mu.Unlock()
		t.Fatalf("atimeTouched = true before any TouchAtime call, want false")
	}
	open.mu.Unlock()

	if err := open.TouchAtime(ctx); err != nil {
		t.Fatalf("first TouchAtime: %v", err)
	}
	open.mu.Lock()
	if !open.atimeTouched {
		open.mu.Unlock()
		t.Fatalf("atimeTouched = false after the first TouchAtime call, want true")
	}
	open.mu.Unlock()

	after, err := fsys.Stat(ctx, key, TypeFile)
	if err != nil {
		t.Fatalf("stat after first touch: %v", err)
	}
	if after.Atime.IsZero() {
		t.Fatalf("Atime still zero after the first TouchAtime call — the real update never happened at all")
	}

	// A second, explicitly different-looking call must be a genuine
	// no-op (not just "happens not to change anything observable") —
	// confirmed by making it fail outright if it were to actually run:
	// Filesystem.TouchAtime against a key that no longer exists would
	// return a real error, so a nil here can only mean the guard
	// skipped calling through at all.
	if err := fsys.Unlink(ctx, root, "touch-once.txt"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	for i := 0; i < 50; i++ {
		if err := open.TouchAtime(ctx); err != nil {
			t.Fatalf("TouchAtime call %d after unlink = %v, want nil (should be a guarded no-op, never reaching the now-deleted key)", i, err)
		}
	}
}

func TestWriteAtRejectsHugeOffsetInsteadOfCrashing(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	key, _, _, err := fsys.Create(ctx, root, "sparse-write-target.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	open, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const hugeOffset = 1<<31 + 1 // the exact value pjdfstest's own open/*.t tests use
	if err := open.WriteAt([]byte("x"), hugeOffset); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("WriteAt at huge offset = %v, want ErrInvalidArgument", err)
	}

	// A negative offset must be rejected the same way — pwrite(2)'s
	// own EINVAL for the identical real-world reason.
	if err := open.WriteAt([]byte("x"), -1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("WriteAt at negative offset = %v, want ErrInvalidArgument", err)
	}

	// The handle must still be usable afterward — a rejected write is
	// just an error for that one call, not a corrupted session.
	if err := open.WriteAt([]byte("hello"), 0); err != nil {
		t.Fatalf("WriteAt after rejected huge write: %v", err)
	}
	if got := open.ReadAt(0, 5); string(got) != "hello" {
		t.Fatalf("content after rejected huge write + real write: got %q, want %q", got, "hello")
	}
}

// TestOpenFileCloseRemovesScratchFile covers ROADMAP.md's Part G, task
// G3: OpenFile's content lives in a private local scratch file, not an
// in-memory []byte — this confirms that file genuinely exists on disk
// while the session is open, and is genuinely removed (not just
// forgotten about) once Close releases it, since a leaked scratch
// file per open-then-close cycle would defeat the whole point of this
// task (a real, unbounded local-disk leak instead of an unbounded RAM
// one).
func TestOpenFileCloseRemovesScratchFile(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	key, _, _, err := fsys.Create(ctx, root, "scratch-lifecycle.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	open, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	open.mu.Lock()
	scratchPath := open.file.Name()
	open.mu.Unlock()

	if _, err := os.Stat(scratchPath); err != nil {
		t.Fatalf("scratch file missing while session is open: %v", err)
	}
	if err := open.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(scratchPath); !os.IsNotExist(err) {
		t.Fatalf("scratch file still present after Close: stat err = %v, want IsNotExist", err)
	}
}

// TestDiskCacheAvoidsRedundantFetchOnValidatedHit covers ROADMAP.md's
// Part G, task G3's actual point: a second seedScratch call for the
// same key+etag must not call fetch again — it should be served from
// the shared cache's own local copy. A fetch counter proves this
// directly rather than inferring it from timing.
func TestDiskCacheAvoidsRedundantFetchOnValidatedHit(t *testing.T) {
	dir := t.TempDir()
	cache := newDiskCache(dir, 1<<20)
	ctx := context.Background()

	fetchCount := 0
	content := []byte("cached content, fetched only once")
	fetch := func(ctx context.Context) (io.ReadCloser, error) {
		fetchCount++
		return io.NopCloser(bytes.NewReader(content)), nil
	}

	f1, size1, err := cache.seedScratch(ctx, "k1", "etag-a", fetch)
	if err != nil {
		t.Fatalf("seedScratch (first, expected miss): %v", err)
	}
	defer f1.Close()
	if fetchCount != 1 {
		t.Fatalf("fetchCount after first seedScratch = %d, want 1 (a real miss)", fetchCount)
	}
	if size1 != int64(len(content)) {
		t.Fatalf("first seedScratch size = %d, want %d", size1, len(content))
	}

	f2, size2, err := cache.seedScratch(ctx, "k1", "etag-a", fetch)
	if err != nil {
		t.Fatalf("seedScratch (second, expected hit): %v", err)
	}
	defer f2.Close()
	if fetchCount != 1 {
		t.Fatalf("fetchCount after second seedScratch (same etag) = %d, want still 1 (a cache hit)", fetchCount)
	}
	got := make([]byte, size2)
	if _, err := f2.ReadAt(got, 0); err != nil {
		t.Fatalf("read back cached copy: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("cached copy content = %q, want %q", got, content)
	}

	// A different etag for the same key must be treated as a real
	// miss — proving this isn't just "fetch is never called twice,"
	// but a genuine validity check against the caller-supplied etag.
	if _, _, err := cache.seedScratch(ctx, "k1", "etag-b", fetch); err != nil {
		t.Fatalf("seedScratch (stale etag, expected miss): %v", err)
	}
	if fetchCount != 2 {
		t.Fatalf("fetchCount after a different etag = %d, want 2 (a real miss, not a stale hit)", fetchCount)
	}
}
