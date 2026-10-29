package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.Locker = (*Drive)(nil)

// Locks through the drive are the mount's locks: a whole-file exclusive
// claim in the file's .lock object, leased, taken and released with the
// filesystem's own lock functions, holder as given (a mount qualifies its
// holders with its own id; the drive doesn't, so a lease outlives the
// Drive that took it, and another process can renew or release it). The
// write path checks them as the mount's flush does (lockCheck), unless
// the drive was opened with Config.NoLocking, when Lock is refused and
// writes look at no locks, as a mount with -locking=false.

// Lock takes item for holder, for ttl, without waiting.
func (d *Drive) Lock(ctx context.Context, item drive.ItemRef, holder string, ttl time.Duration) (_ drive.Lease, err error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return drive.Lease{}, err
	}
	defer done(&err)
	if holder == "" {
		return drive.Lease{}, fmt.Errorf("lock: a holder is needed: %w", drive.ErrInvalid)
	}
	if ttl <= 0 {
		return drive.Lease{}, fmt.Errorf("lock: ttl %v: %w", ttl, drive.ErrInvalid)
	}
	n, err := d.lockable(ctx, item)
	if err != nil {
		return drive.Lease{}, err
	}
	// The lease's expiry by this clock; the store's clock is what the
	// claim carries, so a renewal well before this is the thing to do.
	expires := time.Now().Add(ttl)
	if err := d.fs.TryAcquireLock(ctx, n.key, holder, ttl); err != nil {
		return drive.Lease{}, mapErr(err)
	}
	return drive.Lease{Item: drive.ItemRef{Path: n.path, ID: n.key}, Holder: holder, Expires: expires}, nil
}

// Renew extends l for ttl more.
func (d *Drive) Renew(ctx context.Context, l drive.Lease, ttl time.Duration) (err error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done(&err)
	if ttl <= 0 {
		return fmt.Errorf("renew: ttl %v: %w", ttl, drive.ErrInvalid)
	}
	n, err := d.lockable(ctx, drive.ByID(l.Item.ID))
	if err != nil {
		return err
	}
	return mapErr(d.fs.RenewLock(ctx, n.key, l.Holder, ttl))
}

// Unlock releases l.
func (d *Drive) Unlock(ctx context.Context, l drive.Lease) (err error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done(&err)
	n, err := d.lockable(ctx, drive.ByID(l.Item.ID))
	if err != nil {
		return err
	}
	return mapErr(d.fs.ReleaseLock(ctx, n.key, l.Holder))
}

// lockable resolves item for a lock operation: a file the actor may
// write (a lock keeps writers out, so it takes a writer's right), on a
// drive with locks on.
func (d *Drive) lockable(ctx context.Context, item drive.ItemRef) (*node, error) {
	if d.cfg.NoLocking {
		return nil, fmt.Errorf("locks are off for this drive: %w", drive.ErrNotSupported)
	}
	n, err := d.resolve(ctx, d.fs, item)
	if err != nil {
		return nil, err
	}
	if n.typ == icbfs.TypeDir {
		return nil, drive.ErrIsDir
	}
	if err := d.check(ctx, n, needWrite); err != nil {
		return nil, err
	}
	return n, nil
}

// lockCheck is what a write, move or delete of file n does before
// storing anything, as the mount's flush does: one read of the file's
// lock object, and drive.ErrLocked if another holder than the context's
// (drive.LockHolder) has an unexpired claim on it. A drive with locks
// off reads nothing.
func (d *Drive) lockCheck(ctx context.Context, n *node) error {
	if d.cfg.NoLocking || n.typ == icbfs.TypeDir {
		return nil
	}
	err := d.fs.CheckRangeLockConflict(ctx, n.key, 0, icbfs.LockEOF, drive.LockHolder(ctx))
	if errors.Is(err, icbfs.ErrLocked) {
		return fmt.Errorf("%s: %w", n.path, drive.ErrLocked)
	}
	return mapErr(err)
}

// lockCheckBelow is lockCheck for n and, if n is a directory, every file
// under it: what a move of n does before storing anything, as a
// recursive delete does. A drive with locks off reads nothing.
func (d *Drive) lockCheckBelow(ctx context.Context, n *node) error {
	if d.cfg.NoLocking {
		return nil
	}
	if n.typ != icbfs.TypeDir {
		return d.lockCheck(ctx, n)
	}
	return d.walkBelow(ctx, n, func(c *node) error { return d.lockCheck(ctx, c) })
}
