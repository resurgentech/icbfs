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

// TestConcurrentNonOverlappingWritesBothSurvive: two concurrent writers
// edit different byte ranges of one file, and both edits survive. Without
// re-applying, the second Flush would overwrite the first's range with its
// stale copy; with it, the loser rebases onto the winner's content and
// applies only its own edit.
// otherClient is another client of fsys's filesystem: a Filesystem of its
// own on the same store, as another mount is. Opens within one Filesystem
// share their content, so a race between writers needs two.
func otherClient(t *testing.T, fsys *Filesystem) *Filesystem {
	t.Helper()
	c := New(fsys.store, fsys.fsName)
	c.EnableLocking(fsys.lockingEnabled)
	if err := c.Attach(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

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
	open2, err := otherClient(t, fsys).Open(ctx, key)
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

// TestFlushEscalatesWhenPlainRetryBudgetIsExhausted, deterministically: the
// retry budget is zero, and another write lands before Flush, so its one
// conditional attempt loses with no retries left. Without escalation that's
// ErrWriteContention; with it, Flush takes a lock, rebases onto the other
// write, and lands its edit.
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

	// Another writer changes the object first, so the session's ETag is
	// stale and its only attempt fails.
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

	// The other writer's content with this session's edit on it: Flush
	// rebased before its final write.
	final, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	want := append([]byte("hello"), concurrent[len("hello"):]...)
	if !bytes.Equal(final, want) {
		t.Fatalf("final content = %q, want %q", final, want)
	}
}

// TestFlushRefusedWhenTouchedRangeOverlapsExternalLock: while one client
// holds a lock on a range, another client's write overlapping it is
// refused, and its write elsewhere succeeds.
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
	// (as fcntl would) over
	// [0, 10).
	if err := fsys.TryAcquireLockRange(ctx, key, 0, 10, "client-1", 30*time.Second); err != nil {
		t.Fatalf("client-1 acquire [0,10): %v", err)
	}

	// Simulated client 2's write overlapping that locked range must be
	// refused outright, not retried/escalated — this is someone else's
	// cooperating lock, not same-codebase contention.
	overlapping, err := otherClient(t, fsys).Open(ctx, key)
	if err != nil {
		t.Fatalf("client-2 open: %v", err)
	}
	overlapping.WriteAt([]byte("XXXXX"), 5) // [5,10) overlaps client-1's [0,10)
	if _, err := overlapping.Flush(ctx); !errors.Is(err, ErrLocked) {
		t.Fatalf("client-2 overlapping write = %v, want ErrLocked", err)
	}

	// Client 2's write to a genuinely non-overlapping range succeeds
	// normally while client-1's lock is still held.
	nonOverlapping, err := otherClient(t, fsys).Open(ctx, key)
	if err != nil {
		t.Fatalf("client-3 open: %v", err)
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

// TestCheckRangeLockConflictIgnoresEscalationOnlyEntries: another session's
// escalation claim isn't an application lock to the write check (see
// LockRange.EscalationOnly).
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

// TestOpenFileTouchAtimeOnlyOncePerSession: the kernel splits a large read
// into many Read calls, and recording the access on each would make one 500
// MB read thousands of synchronous metadata writes. Counted, not timed.
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

	// The second call must not reach Filesystem.TouchAtime at all: the file
	// is gone, so reaching it would return an error.
	if err := fsys.Unlink(ctx, root, "touch-once.txt"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	for i := 0; i < 50; i++ {
		if err := open.TouchAtime(ctx); err != nil {
			t.Fatalf("TouchAtime call %d after unlink = %v, want nil (should be a guarded no-op, never reaching the now-deleted key)", i, err)
		}
	}
}

// TestWriteAtHugeOffsets: pjdfstest's open/25.t writes 1 byte at 2^31+1.
// The sparse scratch file accepts it; only a write past the max file size
// is refused, before anything is extended.
func TestWriteAtHugeOffsets(t *testing.T) {
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

	// pjdfstest's open/25.t writes at 2^31+1: a sparse write on the
	// local scratch file now, well within the 1 TiB default limit.
	const pjdOffset = 1<<31 + 1
	if err := open.WriteAt([]byte("x"), pjdOffset); err != nil {
		t.Fatalf("WriteAt at 2 GiB + 1 = %v, want it accepted", err)
	}
	if err := open.Truncate(0); err != nil {
		t.Fatalf("truncate back to 0: %v", err)
	}
	// Past the filesystem's max file size: EFBIG, before the scratch
	// file is extended at all.
	if err := open.WriteAt([]byte("x"), fsys.MaxFileSize()); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("WriteAt past the max file size = %v, want ErrFileTooLarge", err)
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

// TestOpenFileCloseRemovesScratchFile: the session's scratch file exists
// while it's open and is removed by Close, so open-close cycles don't leak
// local disk.
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

	open.c.mu.Lock()
	scratchPath := open.c.file.Name()
	open.c.mu.Unlock()

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

// TestDiskCacheAvoidsRedundantFetchOnValidatedHit: a second seedScratch for
// the same key and ETag is served from the cache, without calling fetch
// (counted).
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

// TestInterleavedWritesAllSurvive: one writer edits bytes 0 and 1000 of a
// file, another edits byte 500, in between, and stores first. The first
// writer's flush loses the race and rebases: re-applying only what it
// wrote, the other writer's byte 500 survives. With one span from its
// first written byte to its last, the rebase would write its stale copy of
// byte 500 back over it.
func TestInterleavedWritesAllSurvive(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key, _, etag, err := fsys.Create(ctx, fsys.RootKey(), "interleaved.bin", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, bytes.Repeat([]byte("."), 1100), etag); err != nil {
		t.Fatal(err)
	}
	ours, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close()
	theirs, err := otherClient(t, fsys).Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()

	if err := ours.WriteAt([]byte("A"), 0); err != nil {
		t.Fatal(err)
	}
	if err := ours.WriteAt([]byte("C"), 1000); err != nil {
		t.Fatal(err)
	}
	if err := theirs.WriteAt([]byte("B"), 500); err != nil {
		t.Fatal(err)
	}
	if _, err := theirs.Flush(ctx); err != nil {
		t.Fatalf("their flush: %v", err)
	}
	if _, err := ours.Flush(ctx); err != nil {
		t.Fatalf("our flush: %v", err)
	}

	final, _, _, err := fsys.ReadFile(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := string([]byte{final[0], final[500], final[1000]}); got != "ABC" {
		t.Fatalf("bytes 0, 500, 1000 are %q, want \"ABC\": an edit was lost", got)
	}
}

// TestFlushNotRefusedByLockBetweenEdits: another client's lock on bytes
// 500-510 doesn't refuse a write of bytes 0 and 1000, which overlaps
// neither; a lock check on one span from the first edit to the last did.
func TestFlushNotRefusedByLockBetweenEdits(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "between.bin", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, make([]byte, 1100), ""); err != nil {
		t.Fatal(err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 500, 510, "client-1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	open, err := otherClient(t, fsys).Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	open.WriteAt([]byte("A"), 0)
	open.WriteAt([]byte("C"), 1000)
	if _, err := open.Flush(ctx); err != nil {
		t.Fatalf("flush of edits on either side of a lock: %v, want nil", err)
	}
	open.WriteAt([]byte("B"), 505)
	if _, err := open.Flush(ctx); !errors.Is(err, ErrLocked) {
		t.Fatalf("flush of an edit inside the lock: %v, want ErrLocked", err)
	}
}
