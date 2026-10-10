package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// ErrLocked is returned by a non-blocking lock acquire attempt when the
// requested range conflicts with a different holder's current,
// unexpired claim.
var ErrLocked = errors.New("lock is currently held")

// ErrNotLockHolder is returned by Release/Renew when the caller does
// not currently hold the exact (holder, range) claim being operated
// on — e.g. the lease already expired and a different caller claimed
// it in the meantime, or it was never held at all under that holder.
var ErrNotLockHolder = errors.New("not the current lock holder")

// ErrLockingDisabled is returned by every real (non-escalation) lock
// operation when this Filesystem wasn't opted into the Locking feature
// via EnableLocking (task B7) — ARCHITECTURE.md's Locking section
// requires a caller that thinks it's holding a lock when it isn't to
// get a clear error, not a silent no-op, so this is returned rather
// than treating every acquire as if it always trivially succeeded.
var ErrLockingDisabled = errors.New("locking is not enabled for this mount")

// maxLockCASRetries bounds the CAS retry loop in the Acquire/Release/
// Renew functions below — same shape of bound as maxTreeRetries, a
// separate constant since it's an unrelated contention domain (lock
// objects, not directory blocks).
const maxLockCASRetries = 20

// lockWholeFileEnd is the "to end of file" sentinel the whole-file
// lock wrappers use as a range's end — matching the common real-world
// convention (e.g. flock(2)'s l_len == 0 meaning "to EOF") of a fixed
// value rather than the file's actual current size, since a lock
// taken now must still cover bytes appended after it was acquired.
const lockWholeFileEnd = math.MaxInt64

// lockObjectKey is the dedicated side object holding a file's lock
// state — see ARCHITECTURE.md's Locking section on why this is its own
// object, not folded into .metadata: locks are written by a different
// caller than stat() touches, and can churn far more frequently under
// contention than attributes ever do.
func lockObjectKey(uuid string) string { return uuid + ".lock" }

// readLockRanges returns the current lock state at lockKey, or (nil,
// nil, nil) if no lock object exists (the "nothing ever claimed" case,
// equivalent to an empty/absent object per ARCHITECTURE.md).
func readLockRanges(ctx context.Context, store objstore.Store, lockKey string) (*pb.Lock, *objstore.Object, error) {
	body, obj, err := store.Get(ctx, lockKey)
	if err != nil {
		if objstore.IsNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, err
	}
	var l pb.Lock
	if len(data) > 0 {
		if err := proto.Unmarshal(data, &l); err != nil {
			return nil, nil, err
		}
	}
	return &l, obj, nil
}

func rangeExpired(e *pb.LockRange, now time.Time) bool {
	return now.UnixMilli() >= e.ExpiresAtUnixMs
}

func rangesOverlap(aStart, aEnd, bStart, bEnd int64) bool {
	return aStart < bEnd && bStart < aEnd
}

// TryAcquireLockRange attempts to claim [start, end) of key for
// holder, non-blocking: checked against every other holder's current,
// unexpired range entries for overlap (task B4's generalization of
// task B2's single-holder body to a list). If any conflicting range is
// actually held and unexpired, it returns ErrLocked immediately rather
// than waiting (task B3's AcquireLockRange builds a blocking wrapper on
// top of this). A missing lock object, an expired entry, or an entry
// already held by the same holder (idempotent re-acquire/replace) are
// all treated as non-conflicting. A holder may hold several disjoint
// ranges simultaneously — acquiring a new range never conflicts with
// that same holder's own existing entries, and leaves them untouched;
// only the exact (holder, start, end) entry being reacquired here is
// replaced.
//
// expires_at is computed from this acquiring client's own local clock
// plus ttl, not a server-anchored timestamp — see ASSUMPTIONS.md's B2
// entry for why, which still applies unchanged to the generalized
// byte-range form here: the safety property (at most one conflicting
// claim ever wins) comes from the CAS write below, not from whose
// clock set expires_at.
func (f *Filesystem) TryAcquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	return f.tryAcquireLockRange(ctx, key, start, end, holder, ttl, false)
}

// tryAcquireLockRange is TryAcquireLockRange's shared implementation.
// escalationOnly tags the written claim as internal-escalation-only
// (task B5), never a real application lock — see LockRange.EscalationOnly's
// doc comment in lock.proto on why task B6's write-conflict check must
// be able to tell the difference. Acquiring (here) always conflict-
// checks against every entry regardless of that flag: an internal
// escalation attempt must still respect a genuine application lock,
// and a genuine application lock attempt must still respect another
// session's in-flight escalation claim — only task B6's *separate*
// check treats them differently.
func (f *Filesystem) tryAcquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration, escalationOnly bool) error {
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLockRanges(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		now := time.Now()

		var kept []*pb.LockRange
		if cur != nil {
			for _, e := range cur.Ranges {
				if rangeExpired(e, now) {
					continue // pruned: an expired lease is simply gone
				}
				if e.Holder == holder {
					if e.Start == start && e.End == end {
						continue // the exact entry being replaced below
					}
					kept = append(kept, e) // this holder's other ranges, untouched
					continue
				}
				if rangesOverlap(start, end, e.Start, e.End) {
					return ErrLocked
				}
				kept = append(kept, e)
			}
		}

		claim := &pb.LockRange{Start: start, End: end, Holder: holder, ExpiresAtUnixMs: now.Add(ttl).UnixMilli(), EscalationOnly: escalationOnly}
		data, err := proto.Marshal(&pb.Lock{Ranges: append(kept, claim)})
		if err != nil {
			return err
		}

		if obj == nil {
			// No lock object exists yet: a plain Put with an empty
			// ifMatch would be unconditional and let two concurrent
			// first-time claimants both "succeed" (the exact race
			// found and fixed in master.go's bootstrapMaster) — use
			// the true create-if-absent primitive instead.
			_, err = f.store.PutIfAbsent(ctx, lockKey, bytes.NewReader(data), nil)
		} else {
			_, err = f.store.Put(ctx, lockKey, bytes.NewReader(data), nil, obj.ETag)
		}
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("acquire lock %q [%d,%d): exceeded %d retries under contention", key, start, end, maxLockCASRetries)
}

// ReleaseLockRange releases the exact [start, end) claim holder holds
// on key, but only if holder is still its current holder — a holder
// whose lease already expired and was stolen by someone else must not
// release the new holder's claim out from under them. Releasing a
// range that's already free (never claimed, or already released) is a
// no-op, not an error. Other ranges this or any other holder holds on
// the same file are untouched.
func (f *Filesystem) ReleaseLockRange(ctx context.Context, key string, start, end int64, holder string) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLockRanges(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		if cur == nil || len(cur.Ranges) == 0 {
			return nil
		}

		now := time.Now()
		var kept []*pb.LockRange
		found := false
		conflictingOtherHolder := false
		for _, e := range cur.Ranges {
			if rangeExpired(e, now) {
				continue
			}
			if e.Start == start && e.End == end {
				if e.Holder == holder {
					found = true
					continue // dropped: this is the release
				}
				conflictingOtherHolder = true
			}
			kept = append(kept, e)
		}
		if !found {
			if conflictingOtherHolder {
				return ErrNotLockHolder
			}
			return nil
		}

		data, err := proto.Marshal(&pb.Lock{Ranges: kept})
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, lockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("release lock %q [%d,%d): exceeded %d retries under contention", key, start, end, maxLockCASRetries)
}

// RenewLockRange extends holder's exact [start, end) claim on key, but
// only succeeds if holder is still its current holder — renewing after
// your lease already expired and was stolen must fail, not resurrect a
// claim you no longer actually hold.
func (f *Filesystem) RenewLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLockRanges(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		if cur == nil {
			return ErrNotLockHolder
		}

		now := time.Now()
		var kept []*pb.LockRange
		found := false
		escalationOnly := false
		for _, e := range cur.Ranges {
			if rangeExpired(e, now) {
				continue
			}
			if e.Start == start && e.End == end && e.Holder == holder {
				found = true
				escalationOnly = e.EscalationOnly // preserved across renewal
				continue // replaced below with the renewed expiry
			}
			kept = append(kept, e)
		}
		if !found {
			return ErrNotLockHolder
		}

		renewed := &pb.LockRange{Start: start, End: end, Holder: holder, ExpiresAtUnixMs: now.Add(ttl).UnixMilli(), EscalationOnly: escalationOnly}
		data, err := proto.Marshal(&pb.Lock{Ranges: append(kept, renewed)})
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, lockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("renew lock %q [%d,%d): exceeded %d retries under contention", key, start, end, maxLockCASRetries)
}

// lockPollMinInterval/lockPollMaxInterval bound AcquireLockRange's
// polling backoff — per ARCHITECTURE.md's Locking section, blocking
// acquisition is client-side polling, not true wake-on-release (object
// storage has no such primitive), so a waiter sleeps with exponential
// backoff between non-blocking attempts rather than hammering the
// store at a fixed tight interval.
const (
	lockPollMinInterval = 20 * time.Millisecond
	lockPollMaxInterval = 1 * time.Second
)

// AcquireLockRange blocks until it acquires [start, end) of key for
// holder, or ctx is done — the blocking (LOCK_NB-equivalent-absent)
// counterpart to TryAcquireLockRange's non-blocking primitive. Callers
// that want a bounded wait pass a ctx with a deadline/timeout
// (context.WithTimeout); passing context.Background() waits
// indefinitely.
//
// This loops TryAcquireLockRange with exponential backoff between
// attempts (task B3) — see ARCHITECTURE.md: object storage has no
// wake-on-release primitive, so "wait for the lock" can only mean
// "retry the CAS-acquire in a loop." Task B10 may later let Change
// Notifications sharpen this loop's trigger on backends where that's a
// real win, without changing the fallback behavior here, which must
// stay correct on its own regardless.
func (f *Filesystem) AcquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	return f.acquireLockRange(ctx, key, start, end, holder, ttl, false)
}

// acquireEscalationLockRange is AcquireLockRange's logic for
// OpenFile.Flush's own internal escalation (task B5) — see
// tryAcquireLockRange's escalationOnly parameter.
func (f *Filesystem) acquireEscalationLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	return f.acquireLockRange(ctx, key, start, end, holder, ttl, true)
}

func (f *Filesystem) acquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration, escalationOnly bool) error {
	backoff := lockPollMinInterval
	wantKey := lockObjectKey(key)
	for {
		err := f.tryAcquireLockRange(ctx, key, start, end, holder, ttl, escalationOnly)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrLocked) {
			return err
		}
		if err := f.waitBackoffOrSignal(ctx, backoff, wantKey); err != nil {
			return err
		}
		backoff *= 2
		if backoff > lockPollMaxInterval {
			backoff = lockPollMaxInterval
		}
	}
}

// waitBackoffOrSignal waits for backoff to elapse, or (task B10) for
// a Change-notifications signal naming wantKey (the exact .lock
// object this acquire is waiting on) to arrive first on f.lockNotify,
// whichever comes first.
//
// If f.lockNotify is nil (EnableChangeNotifications was never called,
// or Change notifications just isn't enabled for this mount at all),
// this reduces to a plain, fixed backoff wait with no special-casing
// needed: a nil channel's select case never becomes ready, so it
// simply never fires.
//
// A non-matching signal (some other key changed) is not an error —
// it's just ignored, and this keeps waiting for the timer or a real
// match. This is why the accelerator is a pure latency improvement,
// never a correctness dependency: even a backend (task E4's MinIO
// adapter is the only one actually this fast) that reliably delivers
// matching signals still has this same plain-polling fallback sitting
// underneath it for every case a signal is missed, delayed, or never
// enabled at all.
func (f *Filesystem) waitBackoffOrSignal(ctx context.Context, backoff time.Duration, wantKey string) error {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case sig, ok := <-f.lockNotify:
			if !ok {
				// The underlying signal stream ended (e.g. Close was
				// called on the backend Source) — fall back to a
				// plain wait for the rest of this backoff rather than
				// re-entering a select where this case, now
				// permanently ready on a closed channel, would spin.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
				}
				return nil
			}
			if sig.Key == wantKey {
				return nil
			}
		}
	}
}

// CheckRangeLockConflict implements task B6's stronger-than-advisory
// enforcement: before issuing a content write, a caller checks its
// touched range against current lock state and refuses the write if it
// overlaps a currently-held, unexpired lock belonging to a different
// holder. selfHolder is excluded the same way acquiring already
// excludes a holder's own entries (so a session never blocks itself).
//
// Entries tagged EscalationOnly are also excluded unconditionally,
// regardless of holder: those are OpenFile.Flush's own internal
// bookkeeping (task B5), not a real application lock placed via
// fcntl/LockFileEx — ARCHITECTURE.md's distinction between task B5
// ("this driver's own write succeeding eventually under contention
// with itself") and task B6 ("respecting a lock an external,
// cooperating caller explicitly placed"). Treating another session's
// in-flight escalation claim as an external lock here would spuriously
// refuse writes that tasks B1/B5 already guarantee converge on their
// own shortly after.
func (f *Filesystem) CheckRangeLockConflict(ctx context.Context, key string, start, end int64, selfHolder string) error {
	if !f.lockingEnabled {
		// Task B7: Locking's real costs (an extra round trip here, on
		// every write) are opt-in — a mount that never opted in never
		// writes a real application lock either, via
		// TryAcquireLockRange's own gate above, so there is nothing
		// for this mount to ever need to respect.
		return nil
	}
	cur, _, err := readLockRanges(ctx, f.store, lockObjectKey(key))
	if err != nil {
		return err
	}
	if cur == nil {
		return nil
	}
	now := time.Now()
	for _, e := range cur.Ranges {
		if e.Holder == selfHolder || e.EscalationOnly || rangeExpired(e, now) {
			continue
		}
		if rangesOverlap(start, end, e.Start, e.End) {
			return ErrLocked
		}
	}
	return nil
}

// LockRangeInfo describes one conflicting claim, returned by
// FindConflictingLockRange — e.g. for FUSE's fcntl(F_GETLK), which
// needs to report back *which* range/holder conflicts, not just
// whether one does.
type LockRangeInfo struct {
	Start, End int64
	Holder     string
}

// FindConflictingLockRange returns the first currently-held, unexpired
// range entry on key that overlaps [start, end) and doesn't belong to
// selfHolder (same exclusions as CheckRangeLockConflict), or ok=false
// if none conflicts.
func (f *Filesystem) FindConflictingLockRange(ctx context.Context, key string, start, end int64, selfHolder string) (info LockRangeInfo, ok bool, err error) {
	if !f.lockingEnabled {
		return LockRangeInfo{}, false, nil
	}
	cur, _, err := readLockRanges(ctx, f.store, lockObjectKey(key))
	if err != nil {
		return LockRangeInfo{}, false, err
	}
	if cur == nil {
		return LockRangeInfo{}, false, nil
	}
	now := time.Now()
	for _, e := range cur.Ranges {
		if e.Holder == selfHolder || e.EscalationOnly || rangeExpired(e, now) {
			continue
		}
		if rangesOverlap(start, end, e.Start, e.End) {
			return LockRangeInfo{Start: e.Start, End: e.End, Holder: e.Holder}, true, nil
		}
	}
	return LockRangeInfo{}, false, nil
}

// TryAcquireLock, ReleaseLock, RenewLock, and AcquireLock are the
// whole-file convenience wrappers task B2 originally shipped, now
// implemented as the [0, lockWholeFileEnd) special case of task B4's
// byte-range primitives — see lockWholeFileEnd's doc comment.

func (f *Filesystem) TryAcquireLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	return f.TryAcquireLockRange(ctx, key, 0, lockWholeFileEnd, holder, ttl)
}

func (f *Filesystem) ReleaseLock(ctx context.Context, key, holder string) error {
	return f.ReleaseLockRange(ctx, key, 0, lockWholeFileEnd, holder)
}

func (f *Filesystem) RenewLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	return f.RenewLockRange(ctx, key, 0, lockWholeFileEnd, holder, ttl)
}

func (f *Filesystem) AcquireLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	return f.AcquireLockRange(ctx, key, 0, lockWholeFileEnd, holder, ttl)
}
