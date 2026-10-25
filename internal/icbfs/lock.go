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

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// ErrLocked is returned by a non-blocking lock acquire attempt when the
// requested range conflicts with a different holder's current,
// unexpired claim.
var ErrLocked = errors.New("lock is currently held")

// ErrNotLockHolder is Release's and Renew's answer when the caller doesn't
// hold that exact claim: its lease expired and someone else took the range,
// or it never held it.
var ErrNotLockHolder = errors.New("not the current lock holder")

// ErrLockingDisabled is every application lock operation's answer when
// EnableLocking wasn't called: a caller must not believe it holds a lock it
// doesn't.
var ErrLockingDisabled = errors.New("locking is not enabled for this mount")

// maxLockCASRetries bounds the retry loops of the lock operations below.
const maxLockCASRetries = 20

// lockWholeFileEnd is the end of a whole-file lock: "to end of file" as a
// fixed value, not the file's size, so the lock also covers bytes appended
// after it was taken. Range ends are exclusive and compared unsigned (see
// rangesOverlap), and this one is 2^63: past the last possible offset,
// 2^63-1, so a range ending here holds that byte too, and a range that
// ends just before it ([x, 2^63-1)) is told apart from one to the end of
// the file. As int64, which ranges are stored in, it's math.MinInt64.
const lockWholeFileEnd = math.MinInt64

// LockEOF is the end of a range "to the end of the file", for access
// layers converting their locks' ranges (lockWholeFileEnd).
const LockEOF int64 = lockWholeFileEnd

// lockLayout is the lock object format this package reads and writes.
const lockLayout = 1

// lockObjectKey is the object holding a file's locks. It's separate from
// .metadata because locks are read by other callers than stat, and can
// change far more often than attributes.
func lockObjectKey(uuid string) string { return uuid + ".lock" }

// readLockRanges returns the current lock state at lockKey, or (nil,
// nil, nil) if no lock object exists (the "nothing ever claimed" case,
// equivalent to an empty/absent object per docs/architecture.md).
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
		if err := block.CheckLayout("lock", l.Layout, lockLayout); err != nil {
			return nil, nil, err
		}
	}
	return &l, obj, nil
}

func rangeExpired(e *pb.LockRange, now time.Time) bool {
	return now.UnixMilli() >= e.ExpiresAtUnixMs
}

func rangesOverlap(aStart, aEnd, bStart, bEnd int64) bool {
	// Unsigned: an end of LockEOF (2^63) is past every offset, the last
	// included (Connectathon's lock tests 2.7 and 6).
	return uint64(aStart) < uint64(bEnd) && uint64(bStart) < uint64(aEnd)
}

// locksConflict reports whether two overlapping claims of different holders
// conflict: two shared (read) claims never do; anything with an exclusive
// claim does.
func locksConflict(aShared, bShared bool) bool {
	return !(aShared && bShared)
}

// mergeOwnRanges applies fcntl(2)'s rule for one owner's own locks:
// acquiring [start, end) adjusts every entry of the holder's that overlaps
// it, as the kernel's posix_lock_file does on a repeated F_SETLK.
//
//   - An overlapping entry of the same type (shared or exclusive) is
//     absorbed: the new claim widens to cover it.
//   - An overlapping entry of the other type is trimmed to what lies
//     outside the new range (none, one or two pieces), keeping its type.
//
// It repeats until nothing changes, since widening can reach entries the
// original range didn't. owned must hold only this holder's unexpired
// entries; other holders' are the caller's conflict check.
func mergeOwnRanges(owned []*pb.LockRange, start, end int64, shared bool) (remainder []*pb.LockRange, newStart, newEnd int64) {
	newStart, newEnd = start, end
	remaining := owned
	for {
		changed := false
		var next []*pb.LockRange
		for _, e := range remaining {
			if !rangesOverlap(newStart, newEnd, e.Start, e.End) {
				next = append(next, e)
				continue
			}
			changed = true
			if e.Shared == shared {
				if e.Start < newStart {
					newStart = e.Start
				}
				if uint64(e.End) > uint64(newEnd) {
					newEnd = e.End
				}
				continue // absorbed into the new claim, not kept
			}
			if e.Start < newStart {
				next = append(next, &pb.LockRange{Start: e.Start, End: newStart, Holder: e.Holder, ExpiresAtUnixMs: e.ExpiresAtUnixMs, EscalationOnly: e.EscalationOnly, Shared: e.Shared})
			}
			if uint64(e.End) > uint64(newEnd) {
				next = append(next, &pb.LockRange{Start: newEnd, End: e.End, Holder: e.Holder, ExpiresAtUnixMs: e.ExpiresAtUnixMs, EscalationOnly: e.EscalationOnly, Shared: e.Shared})
			}
		}
		remaining = next
		if !changed {
			return remaining, newStart, newEnd
		}
	}
}

// TryAcquireLockRange claims [start, end) of key for holder, exclusively
// (F_WRLCK), without waiting: if another holder's unexpired claim
// conflicts, it returns ErrLocked. A missing lock object or an expired
// entry is no conflict. A holder can hold several ranges; its own
// overlapping entries are merged or split as fcntl(2) does (see
// mergeOwnRanges).
//
// The claim's expiry is the store's clock (objstore.Store.ServerTime) plus
// ttl, not this client's clock, which could be skewed against other
// clients'.
func (f *Filesystem) TryAcquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	return f.tryAcquireLockRange(ctx, key, start, end, holder, ttl, false, false)
}

// TryAcquireSharedLockRange is TryAcquireLockRange for a shared (read,
// F_RDLCK) claim: it conflicts with another holder's exclusive claim on an
// overlapping range, never with a shared one.
func (f *Filesystem) TryAcquireSharedLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	return f.tryAcquireLockRange(ctx, key, start, end, holder, ttl, false, true)
}

// tryAcquireLockRange is TryAcquireLockRange's and
// TryAcquireSharedLockRange's implementation.
//
// escalationOnly marks the claim as Flush's own escalation, not an
// application lock (see LockRange.EscalationOnly in lock.proto). Acquiring
// checks every entry either way: escalation respects application locks and
// the other way round. Only CheckRangeLockConflict tells them apart.
func (f *Filesystem) tryAcquireLockRange(ctx context.Context, key string, start, end int64, holder string, ttl time.Duration, escalationOnly, shared bool) error {
	lockKey := lockObjectKey(key)

	// The store's clock, fetched once per call, anchors the new claim's
	// expiry. Checking whether existing entries have expired uses the local
	// clock (checkNow): skew there only shifts when a lease looks expired,
	// and stealing is a conditional write, so two clients can't both take a
	// lock.
	serverNow, err := f.store.ServerTime(ctx)
	if err != nil {
		return fmt.Errorf("acquire lock %q: %w", key, err)
	}

	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		retryPause(ctx, attempt)
		cur, obj, err := readLockRanges(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		checkNow := time.Now()

		var kept []*pb.LockRange
		var owned []*pb.LockRange
		if cur != nil {
			for _, e := range cur.Ranges {
				if rangeExpired(e, checkNow) {
					continue // pruned: an expired lease is simply gone
				}
				if e.Holder == holder {
					owned = append(owned, e) // merged/split below, not just left untouched
					continue
				}
				if rangesOverlap(start, end, e.Start, e.End) && locksConflict(shared, e.Shared) {
					return ErrLocked
				}
				kept = append(kept, e)
			}
		}

		// Real fcntl(2) same-owner merge/split, not just an exact-tuple
		// replace — see mergeOwnRanges's doc comment.
		remainder, claimStart, claimEnd := mergeOwnRanges(owned, start, end, shared)
		kept = append(kept, remainder...)

		claim := &pb.LockRange{Start: claimStart, End: claimEnd, Holder: holder, ExpiresAtUnixMs: serverNow.Add(ttl).UnixMilli(), EscalationOnly: escalationOnly, Shared: shared}
		data, err := proto.Marshal(&pb.Lock{Ranges: append(kept, claim), Layout: lockLayout})
		if err != nil {
			return err
		}

		if obj == nil {
			// No lock object yet: create it with PutIfAbsent, since an
			// unconditional Put would let two first claimants both succeed.
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

// ReleaseLockRange releases holder's exact [start, end) claim on key, only
// if holder still holds it: a holder whose lease expired and was taken must
// not release the new holder's claim. Releasing a free range is a no-op.
// Other claims on the file are untouched.
func (f *Filesystem) ReleaseLockRange(ctx context.Context, key string, start, end int64, holder string) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		retryPause(ctx, attempt)
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

		data, err := proto.Marshal(&pb.Lock{Ranges: kept, Layout: lockLayout})
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

// UnlockRange is fcntl(2)'s F_UNLCK: it removes holder's claims on key
// wherever they overlap [start, end), keeping the parts outside it, so
// unlocking the middle of a held range leaves two. Other holders' claims
// are untouched, and unlocking a range holder doesn't hold succeeds and
// does nothing, as POSIX says. (ReleaseLockRange, by contrast, releases one
// exact claim, for the holder that made it.)
func (f *Filesystem) UnlockRange(ctx context.Context, key string, start, end int64, holder string) error {
	if !f.lockingEnabled {
		return ErrLockingDisabled
	}
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		retryPause(ctx, attempt)
		cur, obj, err := readLockRanges(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		if cur == nil {
			return nil
		}
		now := time.Now()
		var kept []*pb.LockRange
		changed := false
		for _, e := range cur.Ranges {
			if rangeExpired(e, now) {
				changed = true
				continue
			}
			if e.Holder != holder || e.EscalationOnly || !rangesOverlap(start, end, e.Start, e.End) {
				kept = append(kept, e)
				continue
			}
			changed = true
			if e.Start < start {
				kept = append(kept, &pb.LockRange{Start: e.Start, End: start, Holder: e.Holder, ExpiresAtUnixMs: e.ExpiresAtUnixMs, Shared: e.Shared})
			}
			if uint64(e.End) > uint64(end) {
				kept = append(kept, &pb.LockRange{Start: end, End: e.End, Holder: e.Holder, ExpiresAtUnixMs: e.ExpiresAtUnixMs, Shared: e.Shared})
			}
		}
		if !changed {
			return nil
		}
		data, err := proto.Marshal(&pb.Lock{Ranges: kept, Layout: lockLayout})
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, lockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("unlock %q [%d,%d): exceeded %d retries under contention", key, start, end, maxLockCASRetries)
}

// RenewLockRange extends holder's exact [start, end) claim on key, only if
// holder still holds it: renewing after the lease was taken must fail, not
// revive the claim.
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
		retryPause(ctx, attempt)
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
				continue                          // replaced below with the renewed expiry
			}
			kept = append(kept, e)
		}
		if !found {
			return ErrNotLockHolder
		}

		renewed := &pb.LockRange{Start: start, End: end, Holder: holder, ExpiresAtUnixMs: serverNow.Add(ttl).UnixMilli(), EscalationOnly: escalationOnly, Shared: shared}
		data, err := proto.Marshal(&pb.Lock{Ranges: append(kept, renewed), Layout: lockLayout})
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

// lockPollMinInterval and lockPollMaxInterval bound the exponential backoff
// of AcquireLockRange's polling: object storage has no wake-on-release.
const (
	lockPollMinInterval = 20 * time.Millisecond
	lockPollMaxInterval = 1 * time.Second
)

// AcquireLockRange waits until it acquires [start, end) of key for holder,
// or ctx is done; give ctx a deadline for a bounded wait.
//
// It retries TryAcquireLockRange with exponential backoff. With
// EnableChangeNotifications, a signal on the lock's object cuts a wait
// short (waitBackoffOrSignal); without one, polling alone is correct.
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

// acquireEscalationLockRange is AcquireLockRange for Flush's escalation:
// always exclusive, and marked as escalation (see tryAcquireLockRange).
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

// waitBackoffOrSignal waits for backoff to pass, or for a signal naming
// wantKey (the .lock object being waited on), whichever comes first.
// Without notifications f.lockNotify is nil, and a nil channel never fires,
// so this is a plain wait. Signals for other keys are ignored.
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
				// The signal stream ended (its Source was closed): wait out
				// the rest of the backoff plainly, rather than spin on a
				// closed channel.
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

// CheckRangeLockConflict is the write-time check: a caller about to write
// refuses if its touched range overlaps another mount's unexpired claim.
// This mount's own claims don't count, whichever process holds them: its
// opens share the content being written, so the write is theirs too, and
// on a local disk advisory locks don't stop writes.
//
// Escalation claims don't count either, whoever holds them: they are
// another Flush making progress, not a lock an application placed, and
// treating them as one would refuse writes that are about to succeed
// anyway.
//
// Unlike acquiring, a write conflicts with shared claims too: a reader
// holding a shared lock expects the data not to change under it, though
// readers don't conflict with each other.
func (f *Filesystem) CheckRangeLockConflict(ctx context.Context, key string, start, end int64, selfHolder string) error {
	return f.checkRangesLockConflict(ctx, key, [][2]int64{{start, end}}, selfHolder)
}

// checkRangesLockConflict is CheckRangeLockConflict for several ranges, on
// one read of the lock object: ErrLocked if any of them overlaps another
// holder's lock.
func (f *Filesystem) checkRangesLockConflict(ctx context.Context, key string, ranges [][2]int64, selfHolder string) error {
	if !f.lockingEnabled {
		// Without locking no application lock can exist, so there is
		// nothing to check, and no round trip.
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
		if e.Holder == selfHolder || f.ownHolder(e.Holder) || e.EscalationOnly || rangeExpired(e, now) {
			continue
		}
		for _, r := range ranges {
			if rangesOverlap(r[0], r[1], e.Start, e.End) {
				return ErrLocked
			}
		}
	}
	return nil
}

// LockRangeInfo describes one claim, as FindConflictingLockRange returns
// it, for fcntl(F_GETLK), which reports which range, holder and type
// conflict.
type LockRangeInfo struct {
	Start, End int64
	Holder     string
	Shared     bool
}

// FindConflictingLockRange returns the first unexpired claim on key that
// overlaps [start, end), isn't selfHolder's, and would conflict with a
// claim of type querySharedType (F_GETLK's question: would this request
// succeed?). ok is false if none would.
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

// LockRangesHeldBy returns holder's unexpired application claims on key as
// they are now, which after merges and splits (mergeOwnRanges) may not
// match any one earlier request. Lease renewal uses it: renew whatever is
// held now.
func (f *Filesystem) LockRangesHeldBy(ctx context.Context, key, holder string) ([]LockRangeInfo, error) {
	if !f.lockingEnabled {
		return nil, nil
	}
	cur, _, err := readLockRanges(ctx, f.store, lockObjectKey(key))
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, nil
	}
	now := time.Now()
	var out []LockRangeInfo
	for _, e := range cur.Ranges {
		if e.Holder != holder || e.EscalationOnly || rangeExpired(e, now) {
			continue
		}
		out = append(out, LockRangeInfo{Start: e.Start, End: e.End, Holder: e.Holder, Shared: e.Shared})
	}
	return out, nil
}

// TryAcquireLock, ReleaseLock, RenewLock, and AcquireLock are the
// whole-file convenience wrappers, implemented as the
// [0, lockWholeFileEnd) special case of the byte-range primitives — see lockWholeFileEnd's doc comment.

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
