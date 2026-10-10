package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// ErrLocked is returned by TryAcquireLock when the lock is currently
// held by a different holder and its lease hasn't expired yet.
var ErrLocked = errors.New("lock is currently held")

// ErrNotLockHolder is returned by ReleaseLock/RenewLock when the caller
// is not (or is no longer) the lock's current holder — e.g. the lease
// already expired and a different caller claimed it in the meantime.
var ErrNotLockHolder = errors.New("not the current lock holder")

// maxLockCASRetries bounds the CAS retry loop in TryAcquireLock/
// ReleaseLock/RenewLock — same shape of bound as maxTreeRetries, a
// separate constant since it's an unrelated contention domain (lock
// objects, not directory blocks).
const maxLockCASRetries = 20

// lockObjectKey is the dedicated side object holding a file's whole-
// file lock state — see ARCHITECTURE.md's Locking section on why this
// is its own object, not folded into .metadata: locks are written by a
// different caller than stat() touches, and can churn far more
// frequently under contention than attributes ever do.
func lockObjectKey(uuid string) string { return uuid + ".lock" }

// readLock returns the current lock state at lockKey, or (nil, nil,
// nil) if no lock object exists (the "free, never claimed" case,
// equivalent to an empty/absent object per ARCHITECTURE.md).
func readLock(ctx context.Context, store objstore.Store, lockKey string) (*pb.Lock, *objstore.Object, error) {
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

// isHeldByOther reports whether l represents a lease some other holder
// currently holds and hasn't expired, as of now.
func isHeldByOther(l *pb.Lock, holder string, now time.Time) bool {
	if l == nil || l.Holder == "" || l.Holder == holder {
		return false
	}
	return now.UnixMilli() < l.ExpiresAtUnixMs
}

// TryAcquireLock attempts to claim key's whole-file lock for holder,
// non-blocking: if it's currently held by a different holder and not
// expired, it returns ErrLocked immediately rather than waiting (task
// B3 builds a blocking wrapper on top of this). A missing lock object,
// an expired lease, or a lease already held by the same holder
// (idempotent re-acquire) are all treated as free to claim.
//
// expires_at is computed from this acquiring client's own local clock
// plus ttl, not a server-anchored timestamp — a deliberate, documented
// simplification from ARCHITECTURE.md's stated ideal (anchoring to the
// claiming write's own resulting Last-Modified). See ASSUMPTIONS.md:
// capturing your own Put's resulting Last-Modified *before* that same
// Put's body is constructed is not actually possible in one round
// trip, and ARCHITECTURE.md itself treats the fully-precise version
// (raw HTTP Date-header capture) as a soft edge "not worth building."
// The actual safety property — at most one concurrent claim ever wins —
// comes from the CAS write below, not from whose clock set expires_at,
// exactly as ARCHITECTURE.md says.
func (f *Filesystem) TryAcquireLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLock(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		now := time.Now()
		if isHeldByOther(cur, holder, now) {
			return ErrLocked
		}

		claim := &pb.Lock{Holder: holder, ExpiresAtUnixMs: now.Add(ttl).UnixMilli()}
		data, err := proto.Marshal(claim)
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
	return fmt.Errorf("acquire lock %q: exceeded %d retries under contention", key, maxLockCASRetries)
}

// ReleaseLock releases key's whole-file lock, but only if holder is
// still the current holder — a holder whose lease already expired and
// was stolen by someone else must not release the new holder's claim
// out from under them. Releasing an already-free lock is a no-op, not
// an error.
func (f *Filesystem) ReleaseLock(ctx context.Context, key, holder string) error {
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLock(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		if cur == nil || cur.Holder == "" {
			return nil
		}
		if cur.Holder != holder {
			return ErrNotLockHolder
		}
		err = f.store.Delete(ctx, lockKey, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("release lock %q: exceeded %d retries under contention", key, maxLockCASRetries)
}

// RenewLock extends key's whole-file lock, but only succeeds if holder
// is still the current holder — renewing after your lease already
// expired and was stolen must fail, not resurrect a claim you no
// longer actually hold.
func (f *Filesystem) RenewLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	lockKey := lockObjectKey(key)
	for attempt := 0; attempt < maxLockCASRetries; attempt++ {
		cur, obj, err := readLock(ctx, f.store, lockKey)
		if err != nil {
			return err
		}
		if cur == nil || cur.Holder != holder {
			return ErrNotLockHolder
		}
		claim := &pb.Lock{Holder: holder, ExpiresAtUnixMs: time.Now().Add(ttl).UnixMilli()}
		data, err := proto.Marshal(claim)
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, lockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("renew lock %q: exceeded %d retries under contention", key, maxLockCASRetries)
}

// lockPollMinInterval/lockPollMaxInterval bound AcquireLock's polling
// backoff — per ARCHITECTURE.md's Locking section, blocking acquisition
// is client-side polling, not true wake-on-release (object storage has
// no such primitive), so a waiter sleeps with exponential backoff
// between non-blocking attempts rather than hammering the store at a
// fixed tight interval.
const (
	lockPollMinInterval = 20 * time.Millisecond
	lockPollMaxInterval = 1 * time.Second
)

// AcquireLock blocks until it acquires key's whole-file lock for
// holder, or ctx is done — the blocking (LOCK_NB-equivalent-absent)
// counterpart to TryAcquireLock's non-blocking primitive. Callers that
// want a bounded wait pass a ctx with a deadline/timeout
// (context.WithTimeout); passing context.Background() waits
// indefinitely.
//
// This loops TryAcquireLock with exponential backoff between attempts
// (task B3) — see ARCHITECTURE.md: object storage has no wake-on-
// release primitive, so "wait for the lock" can only mean "retry the
// CAS-acquire in a loop." Task B10 may later let Change Notifications
// sharpen this loop's trigger on backends where that's a real win,
// without changing the fallback behavior here, which must stay correct
// on its own regardless.
func (f *Filesystem) AcquireLock(ctx context.Context, key, holder string, ttl time.Duration) error {
	backoff := lockPollMinInterval
	for {
		err := f.TryAcquireLock(ctx, key, holder, ttl)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > lockPollMaxInterval {
			backoff = lockPollMaxInterval
		}
	}
}
