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

// TestLockAcquireReleaseRoundTrip covers B2's "Done when": a basic
// acquire-then-release round trip, confirmed by a second holder being
// able to acquire immediately after release (well before the first
// lease's TTL would have expired on its own).
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

// TestLockSecondAcquireWhileValidIsRejected covers B2's "Done when": a
// second acquire attempt while the first holder's lease is still valid
// is rejected.
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

// TestLockAcquireAfterExpiryStealsSucceeds covers B2's "Done when": an
// acquire attempt after the TTL has passed succeeds even without an
// explicit release (expired-lease steal).
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

// TestLockConcurrentAcquireOnFreeLockHasExactlyOneWinner covers B2's
// "Done when": two (here, several) concurrent acquire attempts against
// the same free lock result in exactly one winner — the real proof
// that the first-ever claim (where there's no existing ETag to
// condition on) is still race-free, not just the renew/steal paths.
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

// TestLockReleaseOfAlreadyFreeLockIsNoop confirms releasing a lock
// nobody holds (never acquired, or already released) is a harmless
// no-op rather than an error.
func TestLockReleaseOfAlreadyFreeLockIsNoop(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.ReleaseLock(ctx, key, "holder-a"); err != nil {
		t.Fatalf("release of never-acquired lock = %v, want nil", err)
	}
}

// TestLockingDisabledByDefaultAndGatedByEnableLocking covers B7's
// "Done when": lock acquisition fails cleanly when the mount wasn't
// started with locking enabled, and succeeds once it was.
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

// TestBlockingAcquireCompletesPromptlyAfterRelease covers B3's "Done
// when": a blocking acquire from a second holder, started while the
// first holder's lease is still valid, completes promptly after the
// first holder releases — not just eventually, and definitely before
// the lease's own TTL would have expired on its own (proving it's
// reacting to the release via polling, not just waiting out the lease).
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

// TestByteRangeLocksNonOverlappingRangesCoexist covers B4's "Done
// when": two non-overlapping ranges can be held concurrently by
// different holders.
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

// TestByteRangeLocksOverlappingRangeRejectedThenSucceedsAfterExpiry
// covers B4's "Done when": a request for an overlapping range is
// rejected while the conflicting range is held, then succeeds once
// that range's lease expires.
func TestByteRangeLocksOverlappingRangeRejectedThenSucceedsAfterExpiry(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnableLocking(true)
	ctx := context.Background()
	key := createLockTestFile(t, fsys)

	if err := fsys.TryAcquireLockRange(ctx, key, 0, 100, "holder-a", 150*time.Millisecond); err != nil {
		t.Fatalf("acquire [0,100): %v", err)
	}
	if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "holder-b", 30*time.Second); err != ErrLocked {
		t.Fatalf("overlapping acquire [50,150) while [0,100) held = %v, want ErrLocked", err)
	}

	time.Sleep(250 * time.Millisecond)
	if err := fsys.TryAcquireLockRange(ctx, key, 50, 150, "holder-b", 30*time.Second); err != nil {
		t.Fatalf("overlapping acquire after expiry = %v, want nil", err)
	}
}

// TestByteRangeLocksOverlappingRangeSucceedsAfterRelease covers the
// same "Done when" requirement via explicit release instead of expiry.
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

// TestByteRangeLocksSameHolderCanHoldMultipleDisjointRanges confirms
// the byte-range generalization doesn't regress to "one claim per
// holder per file": a holder can hold several disjoint ranges on the
// same file simultaneously, each independently released, without
// disturbing the others.
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

// TestBlockingAcquireRespectsContextDeadline confirms AcquireLock
// actually stops waiting (rather than blocking forever) once its ctx
// is done, when the lock is never released.
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

// TestChangeNotificationsAcceleratesWaitBackoffOrSignal covers half of
// ROADMAP.md's task B10 "Done when": a matching Change-notifications
// signal makes a blocking acquire's wait return promptly, measurably
// faster than the current backoff interval it would otherwise have
// waited out. Tested directly against waitBackoffOrSignal (not a full
// AcquireLockRange round trip against MinIO) specifically to get a
// deterministic, non-flaky comparison: a real end-to-end test would
// have to race against this project's already-fast 20ms-1s polling
// baseline, where "measurably better" is hard to distinguish from
// scheduler noise. Picking a deliberately large backoff here (5s) and
// confirming the signal still returns in well under it is the
// unambiguous version of the same proof.
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

// TestChangeNotificationsIgnoresNonMatchingSignal confirms
// waitBackoffOrSignal doesn't return early for a signal naming a
// different key — only an exact match on the specific .lock object
// this wait cares about should accelerate it.
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

// TestBlockingAcquireConvergesWithoutChangeNotifications covers the
// other half of ROADMAP.md's task B10 "Done when": with Change
// notifications never enabled for this Filesystem at all (the default
// — EnableChangeNotifications is never called), blocking acquisition
// must still converge correctly via task B3's plain-polling fallback,
// proving the B10 dependency is genuinely optional rather than
// load-bearing. Same scenario as
// TestBlockingAcquireCompletesPromptlyAfterRelease, named and framed
// explicitly around this requirement rather than left as incidental
// coverage.
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

// TestBlockingAcquireWithChangeNotificationsEnabledAgainstRealFilesystem
// is task B10's end-to-end sanity check against a real Filesystem (a
// real MinIO-backed store, per ROADMAP.md's literal "against MinIO"
// wording for this task's "Done when") with EnableChangeNotifications
// actually wired up: a waiter whose wait is accelerated by a matching
// signal still converges to a successful acquire once the holder
// releases, through the full real AcquireLockRange path (not the
// waitBackoffOrSignal-only unit tests above, which are what actually
// prove the latency improvement deterministically — see their own
// doc comments for why this level isn't a good place to measure that
// precisely, given this project's already-fast 20ms-1s polling
// baseline).
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
	// Simulate the real backend adapter (task E4) having observed the
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
