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

// locksConflict reports whether a claim of type aShared and one of
// type bShared, both covering an already-confirmed-overlapping range
// from different holders, actually conflict — real POSIX fcntl/flock
// semantics (not attempted in this codebase's earlier version; a
// holder-type-insensitive "any overlap from a different holder
// conflicts" rule is wrong for F_RDLCK/shared locks specifically):
// two shared (read) claims never conflict with each other, no matter
// how many different holders hold them; anything else (shared vs.
// exclusive, exclusive vs. exclusive) does.
func locksConflict(aShared, bShared bool) bool {
	return !(aShared && bShared)
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
// expires_at is anchored to the object store's clock (objstore.Store.
// ServerTime, a real HTTP Date header, not a typed field — see its
// doc comment), not this acquiring client's own local clock, per
// ARCHITECTURE.md's Locking section: a client's local clock could be
// skewed relative to other clients, which the store's own clock isn't
// relative to itself. See ASSUMPTIONS.md's B2 entry for the earlier,
// client-clock version of this and why it was replaced.
//
// This claims an exclusive (POSIX F_WRLCK-equivalent) range — see
// TryAcquireSharedLockRange for the F_RDLCK-equivalent, which allows
// multiple different holders to hold overlapping claims at once.
func (f *Filesystem) TryAcquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	return f.tryAcquireLockRange(ctx, key, start, end, holder, ttl, false, false)
}

// TryAcquireSharedLockRange is TryAcquireLockRange's shared/read
// (POSIX F_RDLCK-equivalent) counterpart: it conflicts with another
// holder's exclusive claim on an overlapping range, but never with
// another holder's own shared claim — real POSIX fcntl/flock
// semantics, not the earlier, incorrect version of this codebase's
// lock model, which treated every claim as exclusive regardless of
// what the caller actually asked for.
func (f *Filesystem) TryAcquireSharedLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	return f.tryAcquireLockRange(ctx, key, start, end, holder, ttl, false, true)
}

// tryAcquireLockRange is TryAcquireLockRange/TryAcquireSharedLockRange's
// shared implementation.
//
// escalationOnly tags the written claim as internal-escalation-only
// (task B5), never a real application lock — see LockRange.EscalationOnly's
// doc comment in lock.proto on why task B6's write-conflict check must
// be able to tell the difference. Acquiring (here) always conflict-
// checks against every entry regardless of that flag: an internal
// escalation attempt must still respect a genuine application lock,
// and a genuine application lock attempt must still respect another
// session's in-flight escalation claim — only task B6's *separate*
// check treats them differently.
//
// shared tags the claim as a POSIX shared/read lock (see
// locksConflict for the conflict rule this enables).
func (f *Filesystem) tryAcquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration, escalationOnly, shared bool) error {
	lockKey := lockObjectKey(key)

	// Fetched once per call, not once per retry attempt below: a real
	// round trip to the store, so retries (expected to be fast, all
	// within this one call) reuse it rather than each paying for their
	// own. This is the value the new claim's expires_at is anchored
	// to; the expiry *check* against already-held entries a few lines
	// down deliberately keeps using the local clock (checkNow) — see
	// ARCHITECTURE.md: clock skew there can only shift when a lease
	// looks expired by a few seconds, never let two clients both
	// successfully steal it, so it's accepted rather than also
	// anchored to the store.
	serverNow, err := f.store.ServerTime(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock %q: %w", key, err)
	}

	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLockRanges(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		checkNow := time.Now()

		var kept []*pb.LockRange
		if cur != nil {
			for _, e := range cur.Ranges {
				if rangeExpired(e, checkNow) {
					continue // pruned: an expired lease is simply gone
				}
				if e.Holder == holder {
					if e.Start == start && e.End == end {
						continue // the exact entry being replaced below
					}
					kept = append(kept, e) // this holder's other ranges, untouched
					continue
				}
				if rangesOverlap(start, end, e.Start, e.End) && locksConflict(shared, e.Shared) {
					return ErrLocked
				}
				kept = append(kept, e)
			}
		}

		claim := &pb.LockRange{Start: start, End: end, Holder: holder, ExpiresAtUnixMs: serverNow.Add(ttl).UnixMilli(), EscalationOnly: escalationOnly, Shared: shared}
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

	// See tryAcquireLockRange's identical comment: fetched once per
	// call, anchors the renewed claim's expires_at to the store's
	// clock, not the local one.
	serverNow, err := f.store.ServerTime(ctx)
	if err != nil {
		return fmt.Errorf("renew lock %q: %w", key, err)
	}

	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLockRanges(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		if cur == nil {
			return ErrNotLockHolder
		}

		checkNow := time.Now()
		var kept []*pb.LockRange
		found := false
		escalationOnly := false
		shared := false
		for _, e := range cur.Ranges {
			if rangeExpired(e, checkNow) {
				continue
			}
			if e.Start == start && e.End == end && e.Holder == holder {
				found = true
				escalationOnly = e.EscalationOnly // preserved across renewal
				shared = e.Shared                 // preserved across renewal
				continue // replaced below with the renewed expiry
			}
			kept = append(kept, e)
		}
		if !found {
			return ErrNotLockHolder
		}

		renewed := &pb.LockRange{Start: start, End: end, Holder: holder, ExpiresAtUnixMs: serverNow.Add(ttl).UnixMilli(), EscalationOnly: escalationOnly, Shared: shared}
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
	return f.acquireLockRange(ctx, key, start, end, holder, ttl, false, false)
}

// AcquireSharedLockRange is AcquireLockRange's shared/read
// (F_RDLCK-equivalent) counterpart — see TryAcquireSharedLockRange.
func (f *Filesystem) AcquireSharedLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	return f.acquireLockRange(ctx, key, start, end, holder, ttl, false, true)
}

// acquireEscalationLockRange is AcquireLockRange's logic for
// OpenFile.Flush's own internal escalation (task B5) — see
// tryAcquireLockRange's escalationOnly parameter. Always exclusive
// (never shared): escalation exists purely to guarantee a content
// write succeeds, which is inherently an exclusive operation on the
// range it touches.
func (f *Filesystem) acquireEscalationLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	return f.acquireLockRange(ctx, key, start, end, holder, ttl, true, false)
}

func (f *Filesystem) acquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration, escalationOnly, shared bool) error {
	backoff := lockPollMinInterval
	wantKey := lockObjectKey(key)
	for {
		err := f.tryAcquireLockRange(ctx, key, start, end, holder, ttl, escalationOnly, shared)
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
//
// Deliberately type-insensitive (unlike acquiring, which uses
// locksConflict's shared-vs-shared exception): a content write always
// conflicts with *any* other holder's claim on the overlapping range,
// shared or exclusive. A shared (read) lock holder expects a stable
// view of the data for as long as they hold it — an uncoordinated
// write landing underneath them while they believe they hold a read
// lock would violate exactly that expectation, even though two
// *readers* are perfectly compatible with each other.
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
// needs to report back *which* range/holder/type conflicts, not just
// whether one does.
type LockRangeInfo struct {
	Start, End int64
	Holder     string
	Shared     bool
}

// FindConflictingLockRange returns the first currently-held, unexpired
// range entry on key that overlaps [start, end), doesn't belong to
// selfHolder, and would actually conflict with a querying claim of
// type querySharedType (same locksConflict rule TryAcquireLockRange/
// TryAcquireSharedLockRange use — this simulates "would this request
// succeed," matching real fcntl(F_GETLK) semantics: querying with a
// shared/read type does not report another holder's shared claim as a
// conflict, only an exclusive one). Returns ok=false if none conflicts.
func (f *Filesystem) FindConflictingLockRange(ctx context.Context, key string, start, end int64, selfHolder string, querySharedType bool) (info LockRangeInfo, ok bool, err error) {
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
		if rangesOverlap(start, end, e.Start, e.End) && locksConflict(querySharedType, e.Shared) {
			return LockRangeInfo{Start: e.Start, End: e.End, Holder: e.Holder, Shared: e.Shared}, true, nil
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

// TryAcquireSharedLock is TryAcquireLock's shared/read
// (F_RDLCK/LOCK_SH-equivalent) counterpart.
func (f *Filesystem) TryAcquireSharedLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	return f.TryAcquireSharedLockRange(ctx, key, 0, lockWholeFileEnd, holder, ttl)
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

// AcquireSharedLock is AcquireLock's shared/read
// (F_RDLCK/LOCK_SH-equivalent) counterpart.
func (f *Filesystem) AcquireSharedLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	return f.AcquireSharedLockRange(ctx, key, 0, lockWholeFileEnd, holder, ttl)
}
