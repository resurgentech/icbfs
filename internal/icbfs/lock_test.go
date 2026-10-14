package icbfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/notify"
)

func createLockTestFile(t *testing.T, fsys *Filesystem) string {
	t.Helper()
	key, _, _, err := fsys.Create(context.Background(), fsys.RootKey(), "lockme.bin", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return key
}

// TestLockAcquireReleaseRoundTrip: after a release, a second holder
// acquires at once, well before the first lease would have expired.
func TestLockAcquireReleaseRoundTrip(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
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

// TestLockSecondAcquireWhileValidIsRejected: while a lease is valid,
// another holder's acquire is refused.
func TestLockSecondAcquireWhileValidIsRejected(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := fsys.TryAcquireLock(ctx, key, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("second acquire = %v, want ErrLocked", err)
	}
}

// TestLockAcquireAfterExpiryStealsSucceeds: after the lease expires,
// another holder acquires without a release.
func TestLockAcquireAfterExpiryStealSucceeds(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
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

// TestLockConcurrentAcquireOnFreeLockHasExactlyOneWinner: several
// concurrent acquires of a free lock have exactly one winner, so the first
// claim, with no ETag to condition on, is race-free too.
func TestLockConcurrentAcquireOnFreeLockHasExactlyOneWinner(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
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
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	// Expiry is anchored to the store's clock, which reads in whole
	// seconds, so a lease under a couple of seconds can look expired as
	// soon as it's written. 3 s is clear of that.
	const initialTTL = 3 * time.Second
	if err := fsys.TryAcquireLock(ctx, key, "holder-a", initialTTL); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := fsys.RenewLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("renew by current holder: %v", err)
	}
	if err := fsys.RenewLock(ctx, key, "holder-b", 30*time.Second); err != ErrNotLockHolder {
		t.Fatalf("renew by non-holder = %v, want ErrNotLockHolder", err)
	}

	// The renewal pushed the lease past its original TTL: a third holder
	// still can't take it after that TTL.
	time.Sleep(initialTTL + 500*time.Millisecond)
	if err := fsys.TryAcquireLock(ctx, key, "holder-c", 30*time.Second); err != ErrLocked {
		t.Fatalf("acquire after renew = %v, want ErrLocked (renew should have extended the lease)", err)
	}
}

// TestLockReleaseFailsForNonHolder: a caller that doesn't hold the lock (or
// lost it to expiry) can't release someone else's claim.
func TestLockReleaseFailsForNonHolder(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
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

// TestLockReleaseOfAlreadyFreeLockIsNoop: releasing a lock nobody holds is
// a no-op, not an error.
func TestLockReleaseOfAlreadyFreeLockIsNoop(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.ReleaseLock(ctx, key, "holder-a"); err != nil {
		t.Fatalf("release of never-acquired lock = %v, want nil", err)
	}
}

// TestLockingDisabledByDefaultAndGatedByEnableLocking: acquiring fails
// cleanly without EnableLocking, and works with it.
func TestLockingDisabledByDefaultAndGatedByEnableLocking(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 30*time.Second); !errors.Is(err, ErrLockingDisabled) {
		t.Fatalf("acquire with locking not enabled = %v, want ErrLockingDisabled", err)
	}
	if err := fsys.ReleaseLock(ctx, key, "holder-a"); !errors.Is(err, ErrLockingDisabled) {
		t.Fatalf("release with locking not enabled = %v, want ErrLockingDisabled", err)
	}
	if err := fsys.RenewLock(ctx, key, "holder-a", 30*time.Second); !errors.Is(err, ErrLockingDisabled) {
		t.Fatalf("renew with locking not enabled = %v, want ErrLockingDisabled", err)
	}
	blockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := fsys.AcquireLock(blockCtx, key, "holder-a", 30*time.Second); !errors.Is(err, ErrLockingDisabled) {
		t.Fatalf("blocking acquire with locking not enabled = %v, want ErrLockingDisabled", err)
	}

	fsys.EnableLocking(true)
	if err := fsys.TryAcquireLock(ctx, key, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire after EnableLocking(true) = %v, want nil", err)
	}
}

// TestBlockingAcquireCompletesPromptlyAfterRelease: a blocking acquire
// started while another holder's lease is valid completes soon after the
// release, well before the lease would have expired.
func TestBlockingAcquireCompletesPromptlyAfterRelease(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
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

// TestByteRangeLocksNonOverlappingRangesCoexist: different holders hold
// non-overlapping ranges at once.
func TestByteRangeLocksNonOverlappingRangesCoexist(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLockRange(ctx, key, 0, 100, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire [0,100): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 100, 200, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("acquire [100,200), non-overlapping with [0,100): %v", err)
	}
}

// TestByteRangeLocksOverlappingRangeRejectedThenSucceedsAfterExpiry: an
// overlapping range is refused while held, and granted once that lease
// expires.
func TestByteRangeLocksOverlappingRangeRejectedThenSucceedsAfterExpiry(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	// Whole-second store clock; see
	// TestLockRenewOnlySucceedsForCurrentHolder.
	const initialTTL = 3 * time.Second
	if err := fsys.TryAcquireLockRange(ctx, key, 0, 100, "holder-a", initialTTL); err != nil {
		t.Fatalf("acquire [0,100): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("overlapping acquire [50,150) while [0,100) held = %v, want ErrLocked", err)
	}

	time.Sleep(initialTTL + 500*time.Millisecond)
	if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("overlapping acquire after expiry = %v, want nil", err)
	}
}

// TestByteRangeLocksOverlappingRangeSucceedsAfterRelease is the same,
// with a release instead of expiry.
func TestByteRangeLocksOverlappingRangeSucceedsAfterRelease(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLockRange(ctx, key, 0, 100, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire [0,100): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("overlapping acquire while held = %v, want ErrLocked", err)
	}
	if err := fsys.ReleaseLockRange(ctx, key, 0, 100, "holder-a"); err != nil {
		t.Fatalf("release [0,100): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("overlapping acquire after release = %v, want nil", err)
	}
}

// TestByteRangeLocksSameHolderCanHoldMultipleDisjointRanges: one holder
// holds several disjoint ranges of a file, each released on its own.
func TestByteRangeLocksSameHolderCanHoldMultipleDisjointRanges(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLockRange(ctx, key, 0, 10, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire [0,10): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 20, 30, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire second disjoint range [20,30) for the same holder = %v, want nil", err)
	}

	if err := fsys.TryAcquireLockRange(ctx, key, 0, 10, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("[0,10) should still be held by holder-a: got %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 20, 30, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("[20,30) should still be held by holder-a: got %v", err)
	}

	if err := fsys.ReleaseLockRange(ctx, key, 0, 10, "holder-a"); err != nil {
		t.Fatalf("release [0,10): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 0, 10, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("[0,10) should now be free: got %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 20, 30, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("[20,30) should still be held by holder-a after releasing only [0,10): got %v", err)
	}
}

// TestByteRangeLocksSameHolderOverlappingReacquireMergesAndSplits: a holder
// re-locking an overlapping range adjusts its own claims as fcntl(2) does
// (see mergeOwnRanges), rather than adding a second entry.
func TestByteRangeLocksSameHolderOverlappingReacquireMergesAndSplits(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	// Same type: locking [5,15) exclusive while holding [0,10) exclusive
	// makes one [0,15) claim.
	if err := fsys.TryAcquireLockRange(ctx, key, 0, 10, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire [0,10): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 5, 15, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire overlapping [5,15) for the same holder: %v", err)
	}
	// Another holder must now see the whole merged [0,15) as held —
	// including [10,15), which was never separately requested.
	if err := fsys.TryAcquireLockRange(ctx, key, 10, 15, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("[10,15) should be covered by holder-a's merged [0,15): got %v", err)
	}
	// And [0,5), the other half of the original [0,10), must also
	// still be held — proving it wasn't dropped, only absorbed.
	if err := fsys.TryAcquireLockRange(ctx, key, 0, 5, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("[0,5) should still be covered by holder-a's merged [0,15): got %v", err)
	}
	if err := fsys.ReleaseLockRange(ctx, key, 0, 15, "holder-a"); err != nil {
		t.Fatalf("release merged [0,15): %v", err)
	}

	// Other type: locking [5,10) shared while holding [0,20) exclusive
	// leaves exclusive [0,5) and [10,20), and shared [5,10).
	if err := fsys.TryAcquireLockRange(ctx, key, 0, 20, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire [0,20) exclusive: %v", err)
	}
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 5, 10, "holder-a", 30*time.Second); err != nil {
		t.Fatalf("acquire overlapping [5,10) shared for the same holder: %v", err)
	}
	// The remainders [0,5) and [10,20) are still exclusive: another
	// holder's shared request there must still conflict.
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 5, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("[0,5) remainder should still be holder-a's exclusive claim: got %v", err)
	}
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 10, 20, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("[10,20) remainder should still be holder-a's exclusive claim: got %v", err)
	}
	// [5,10) is now shared: another holder's shared request there
	// must succeed (coexists), but an exclusive one must not.
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 5, 10, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("[5,10) should now be holder-a's shared claim, coexisting with holder-b's: %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 5, 10, "holder-c", 30*time.Second); err != ErrLocked {
		t.Fatalf("[5,10) exclusive request should conflict with the two shared holders: got %v", err)
	}
}

// TestBlockingAcquireRespectsContextDeadline: a blocking acquire stops
// waiting when its ctx is done.
func TestBlockingAcquireRespectsContextDeadline(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
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

// TestChangeNotificationsAcceleratesWaitBackoffOrSignal: a matching signal
// ends a wait well before its backoff. Tested on waitBackoffOrSignal
// directly with a long (5 s) backoff, since against real polling (20 ms to
// 1 s) the difference would be hard to tell from scheduler noise.
func TestChangeNotificationsAcceleratesWaitBackoffOrSignal(t *testing.T) {
	signals := make(chan notify.Signal, 1)
	fsys := &Filesystem{}
	fsys.EnableChangeNotifications(signals)

	const longBackoff = 5 * time.Second
	const wantKey = "0000-target.lock"

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- fsys.waitBackoffOrSignal(context.Background(), longBackoff, wantKey)
	}()

	time.Sleep(50 * time.Millisecond)
	signals <- notify.Signal{Key: wantKey}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitBackoffOrSignal: %v", err)
		}
		if elapsed := time.Since(start); elapsed >= longBackoff {
			t.Fatalf("waitBackoffOrSignal took %v, want well under the %v backoff — the matching signal should have short-circuited it", elapsed, longBackoff)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the matching signal to short-circuit the wait")
	}
}

// TestChangeNotificationsIgnoresNonMatchingSignal: a signal for another key
// doesn't end the wait.
func TestChangeNotificationsIgnoresNonMatchingSignal(t *testing.T) {
	signals := make(chan notify.Signal, 1)
	fsys := &Filesystem{}
	fsys.EnableChangeNotifications(signals)

	const shortBackoff = 100 * time.Millisecond
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- fsys.waitBackoffOrSignal(context.Background(), shortBackoff, "0000-target.lock")
	}()

	signals <- notify.Signal{Key: "0000-some-other-key.lock"}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitBackoffOrSignal: %v", err)
		}
		if elapsed := time.Since(start); elapsed < shortBackoff {
			t.Fatalf("waitBackoffOrSignal returned after %v, before the %v backoff elapsed — a non-matching signal should not have short-circuited it", elapsed, shortBackoff)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out — a non-matching signal may have wedged the wait instead of just being ignored")
	}
}

// TestBlockingAcquireConvergesWithoutChangeNotifications: without
// notifications, blocking acquisition still works, by polling.
func TestBlockingAcquireConvergesWithoutChangeNotifications(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	const ttl = 10 * time.Second
	if err := fsys.TryAcquireLock(ctx, key, "holder-a", ttl); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		blockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		done <- fsys.AcquireLock(blockCtx, key, "holder-b", ttl)
	}()

	time.Sleep(200 * time.Millisecond)
	if err := fsys.ReleaseLock(ctx, key, "holder-a"); err != nil {
		t.Fatalf("release: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocking acquire without Change notifications: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking acquire did not converge via plain polling within 5s of the release")
	}
}

// TestBlockingAcquireWithChangeNotificationsEnabledAgainstRealFilesystem:
// with notifications wired, through the whole AcquireLockRange path against
// MinIO, a waiter still gets the lock once the holder releases. (The tests
// above measure the speed-up.)
func TestBlockingAcquireWithChangeNotificationsEnabledAgainstRealFilesystem(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	src := notify.NewFakeSource(4)
	defer src.Close()
	fsys.EnableChangeNotifications(src.Signals())

	const ttl = 10 * time.Second
	if err := fsys.TryAcquireLock(ctx, key, "holder-a", ttl); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		blockCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		done <- fsys.AcquireLock(blockCtx, key, "holder-b", ttl)
	}()

	time.Sleep(200 * time.Millisecond)
	if err := fsys.ReleaseLock(ctx, key, "holder-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Simulate the real backend adapter having observed the
	// release and delivered a signal for this exact .lock key.
	src.Emit(notify.Signal{Key: key + ".lock"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocking acquire with Change notifications enabled: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking acquire did not converge within 5s of the release")
	}
}

// TestSharedLocksFromDifferentHoldersCoexist: different holders hold shared
// claims on overlapping ranges at once.
func TestSharedLocksFromDifferentHoldersCoexist(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 100, "reader-a", 30*time.Second); err != nil {
		t.Fatalf("reader-a shared acquire: %v", err)
	}
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 50, 150, "reader-b", 30*time.Second); err != nil {
		t.Fatalf("reader-b shared acquire on an overlapping range = %v, want nil (shared locks from different holders must coexist)", err)
	}
	// A third shared reader on the exact same range as reader-a too.
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 100, "reader-c", 30*time.Second); err != nil {
		t.Fatalf("reader-c shared acquire on the same range as reader-a = %v, want nil", err)
	}
}

// TestExclusiveLockConflictsWithSharedFromOtherHolder: exclusive while
// another's shared is held is refused, and shared while another's exclusive
// is held is refused too.
func TestExclusiveLockConflictsWithSharedFromOtherHolder(t *testing.T) {
	ctx := context.Background()

	t.Run("exclusive request while shared held", func(t *testing.T) {
		fsys, _ := newTestFilesystem(t)
		fsys.EnableLocking(true)
		key := createLockTestFile(t, fsys)
		if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 100, "reader-a", 30*time.Second); err != nil {
			t.Fatalf("shared acquire: %v", err)
		}
		if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "writer-b", 30*time.Second); err != ErrLocked {
			t.Fatalf("exclusive acquire overlapping a shared holder = %v, want ErrLocked", err)
		}
	})

	t.Run("shared request while exclusive held", func(t *testing.T) {
		fsys, _ := newTestFilesystem(t)
		fsys.EnableLocking(true)
		key := createLockTestFile(t, fsys)
		if err := fsys.TryAcquireLockRange(ctx, key, 0, 100, "writer-a", 30*time.Second); err != nil {
			t.Fatalf("exclusive acquire: %v", err)
		}
		if err := fsys.TryAcquireSharedLockRange(ctx, key, 50, 150, "reader-b", 30*time.Second); err != ErrLocked {
			t.Fatalf("shared acquire overlapping an exclusive holder = %v, want ErrLocked", err)
		}
	})

	t.Run("exclusive request while exclusive held", func(t *testing.T) {
		fsys, _ := newTestFilesystem(t)
		fsys.EnableLocking(true)
		key := createLockTestFile(t, fsys)
		if err := fsys.TryAcquireLockRange(ctx, key, 0, 100, "writer-a", 30*time.Second); err != nil {
			t.Fatalf("exclusive acquire: %v", err)
		}
		if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "writer-b", 30*time.Second); err != ErrLocked {
			t.Fatalf("exclusive acquire overlapping another exclusive holder = %v, want ErrLocked", err)
		}
	})
}

// TestRenewLockRangePreservesSharedType: a renewed shared lock stays
// shared.
func TestRenewLockRangePreservesSharedType(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	// Whole-second store clock; see
	// TestLockRenewOnlySucceedsForCurrentHolder.
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 100, "reader-a", 3*time.Second); err != nil {
		t.Fatalf("shared acquire: %v", err)
	}
	if err := fsys.RenewLockRange(ctx, key, 0, 100, "reader-a", 30*time.Second); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 100, "reader-b", 30*time.Second); err != nil {
		t.Fatalf("second shared acquire after renewal = %v, want nil (renewal must preserve the shared type)", err)
	}
}

// TestFindConflictingLockRangeIsTypeAware: as F_GETLK does, a shared query
// doesn't report another holder's shared claim, an exclusive query does,
// and both report an exclusive claim.
func TestFindConflictingLockRangeIsTypeAware(t *testing.T) {
	ctx := context.Background()

	t.Run("shared query against a shared holder", func(t *testing.T) {
		fsys, _ := newTestFilesystem(t)
		fsys.EnableLocking(true)
		key := createLockTestFile(t, fsys)
		if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 100, "reader-a", 30*time.Second); err != nil {
			t.Fatalf("shared acquire: %v", err)
		}
		_, conflict, err := fsys.FindConflictingLockRange(ctx, key, 0, 100, "reader-b", true)
		if err != nil {
			t.Fatalf("find conflicting: %v", err)
		}
		if conflict {
			t.Fatal("shared query against a shared holder reported a conflict, want none")
		}
	})

	t.Run("exclusive query against a shared holder", func(t *testing.T) {
		fsys, _ := newTestFilesystem(t)
		fsys.EnableLocking(true)
		key := createLockTestFile(t, fsys)
		if err := fsys.TryAcquireSharedLockRange(ctx, key, 0, 100, "reader-a", 30*time.Second); err != nil {
			t.Fatalf("shared acquire: %v", err)
		}
		info, conflict, err := fsys.FindConflictingLockRange(ctx, key, 0, 100, "writer-b", false)
		if err != nil {
			t.Fatalf("find conflicting: %v", err)
		}
		if !conflict {
			t.Fatal("exclusive query against a shared holder reported no conflict, want one")
		}
		if !info.Shared {
			t.Fatalf("reported conflict's Shared = %v, want true (it should reflect the real holder's type)", info.Shared)
		}
	})

	t.Run("either query against an exclusive holder", func(t *testing.T) {
		fsys, _ := newTestFilesystem(t)
		fsys.EnableLocking(true)
		key := createLockTestFile(t, fsys)
		if err := fsys.TryAcquireLockRange(ctx, key, 0, 100, "writer-a", 30*time.Second); err != nil {
			t.Fatalf("exclusive acquire: %v", err)
		}
		for _, sharedQuery := range []bool{true, false} {
			info, conflict, err := fsys.FindConflictingLockRange(ctx, key, 0, 100, "other", sharedQuery)
			if err != nil {
				t.Fatalf("find conflicting (sharedQuery=%v): %v", sharedQuery, err)
			}
			if !conflict {
				t.Fatalf("query (shared=%v) against an exclusive holder reported no conflict, want one", sharedQuery)
			}
			if info.Shared {
				t.Fatalf("reported conflict's Shared = %v, want false", info.Shared)
			}
		}
	})
}
