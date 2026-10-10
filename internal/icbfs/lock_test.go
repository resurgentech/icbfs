package icbfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func createLockTestFile(t *testing.T, fsys *Filesystem) string {
	t.Helper()
	key, _, _, err := fsys.Create(context.Background(), fsys.RootKey(), "lockme.bin", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return key
}

// TestLockAcquireReleaseRoundTrip covers B2's "Done when": a basic
// acquire-then-release round trip, confirmed by a second holder being
// able to acquire immediately after release (well before the first
// lease's TTL would have expired on its own).
func TestLockAcquireReleaseRoundTrip(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := fsys.ReleaseLock(ctx, key, "holder-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := fsys.TryAcquireLock(ctx, key, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("acquire after release = %v, want nil (released lock should be immediately claimable, not waiting out the original TTL)", err)
	}
}

// TestLockSecondAcquireWhileValidIsRejected covers B2's "Done when": a
// second acquire attempt while the first holder's lease is still valid
// is rejected.
func TestLockSecondAcquireWhileValidIsRejected(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := fsys.TryAcquireLock(ctx, key, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("second acquire = %v, want ErrLocked", err)
	}
}

// TestLockAcquireAfterExpiryStealsSucceeds covers B2's "Done when": an
// acquire attempt after the TTL has passed succeeds even without an
// explicit release (expired-lease steal).
func TestLockAcquireAfterExpiryStealSucceeds(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 50*time.Millisecond); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := fsys.TryAcquireLock(ctx, key, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("steal after expiry = %v, want nil", err)
	}
}

// TestLockConcurrentAcquireOnFreeLockHasExactlyOneWinner covers B2's
// "Done when": two (here, several) concurrent acquire attempts against
// the same free lock result in exactly one winner — the real proof
// that the first-ever claim (where there's no existing ETag to
// condition on) is still race-free, not just the renew/steal paths.
func TestLockConcurrentAcquireOnFreeLockHasExactlyOneWinner(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fsys.TryAcquireLock(ctx, key, fmt.Sprintf("holder-%02d", i), 30*time.Second)
		}(i)
	}
	wg.Wait()

	wins := 0
	for i, err := range errs {
		switch err {
		case nil:
			wins++
		case ErrLocked:
			// expected for every loser
		default:
			t.Fatalf("holder-%02d: unexpected error: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("got %d winners out of %d concurrent acquires, want exactly 1", wins, n)
	}
}

// TestLockRenewOnlySucceedsForCurrentHolder confirms Renew's holder
// check: the current holder can extend its own lease, but a non-holder
// cannot renew a lease it doesn't hold.
func TestLockRenewOnlySucceedsForCurrentHolder(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 200*time.Millisecond); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := fsys.RenewLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("renew by current holder: %v", err)
	}
	if err := fsys.RenewLock(ctx, key, "holder-b", 30*time.Second); err != ErrNotLockHolder {
		t.Fatalf("renew by non-holder = %v, want ErrNotLockHolder", err)
	}

	// The renew should have pushed the lease well past its original
	// 200ms TTL — confirm a third party still can't steal it yet.
	time.Sleep(300 * time.Millisecond)
	if err := fsys.TryAcquireLock(ctx, key, "holder-c", 30*time.Second); err != ErrLocked {
		t.Fatalf("acquire after renew = %v, want ErrLocked (renew should have extended the lease)", err)
	}
}

// TestLockReleaseFailsForNonHolder confirms Release's holder check: a
// caller that never held the lock (or lost it to an expired-lease
// steal) cannot release someone else's active claim.
func TestLockReleaseFailsForNonHolder(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := fsys.ReleaseLock(ctx, key, "holder-b"); err != ErrNotLockHolder {
		t.Fatalf("release by non-holder = %v, want ErrNotLockHolder", err)
	}
	// And the real holder's lock should be untouched by that attempt.
	if err := fsys.TryAcquireLock(ctx, key, "holder-c", 30*time.Second); err != ErrLocked {
		t.Fatalf("acquire after a rejected foreign release = %v, want still ErrLocked", err)
	}
}

// TestLockReleaseOfAlreadyFreeLockIsNoop confirms releasing a lock
// nobody holds (never acquired, or already released) is a harmless
// no-op rather than an error.
func TestLockReleaseOfAlreadyFreeLockIsNoop(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.ReleaseLock(ctx, key, "holder-a"); err != nil {
		t.Fatalf("release of never-acquired lock = %v, want nil", err)
	}
}

// TestBlockingAcquireCompletesPromptlyAfterRelease covers B3's "Done
// when": a blocking acquire from a second holder, started while the
// first holder's lease is still valid, completes promptly after the
// first holder releases — not just eventually, and definitely before
// the lease's own TTL would have expired on its own (proving it's
// reacting to the release via polling, not just waiting out the lease).
func TestBlockingAcquireCompletesPromptlyAfterRelease(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	const ttl = 10 * time.Second
	if err := fsys.TryAcquireLock(ctx, key, "holder-a", ttl); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		blockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		done <- fsys.AcquireLock(blockCtx, key, "holder-b", ttl)
	}()

	// Give the blocking acquire time to start polling and observe the
	// lock as held before we release it.
	time.Sleep(200 * time.Millisecond)
	if err := fsys.ReleaseLock(ctx, key, "holder-a"); err != nil {
		t.Fatalf("release: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocking acquire: %v", err)
		}
		if elapsed := time.Since(start); elapsed >= ttl {
			t.Fatalf("blocking acquire took %v, which is >= the lease TTL (%v) — looks like it waited out the lease instead of reacting to the release", elapsed, ttl)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking acquire did not complete within 5s of the release")
	}
}

// TestBlockingAcquireRespectsContextDeadline confirms AcquireLock
// actually stops waiting (rather than blocking forever) once its ctx
// is done, when the lock is never released.
func TestBlockingAcquireRespectsContextDeadline(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	blockCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := fsys.AcquireLock(blockCtx, key, "holder-b", 30*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocking acquire with no release = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("blocking acquire took %v to give up after a 300ms deadline — not actually bounded", elapsed)
	}
}
