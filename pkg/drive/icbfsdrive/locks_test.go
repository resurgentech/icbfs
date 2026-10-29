package icbfsdrive

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestLocks: a lock through the drive keeps every other writer out
// (another holder's write, move, delete, a second lock) and lets its own
// holder through; it is the mount's lock (one the filesystem's own lock
// functions see, and one they take is honoured by the drive); it
// expires; it is released; and a drive with locks off neither takes nor
// honours them.
func TestLocks(t *testing.T) {
	store := miniotest.Store(t)
	settings := icbfs.Settings{Name: "locks", Size: 1 << 30, RootMode: 0o777}
	d := newDrive(t, store, settings, "")
	other := newDrive(t, store, settings, "") // another process, same filesystem
	ctx := system()
	if !d.Capabilities().Locks {
		t.Fatal("the drive doesn't say it locks")
	}
	write(t, ctx, d, "/set/a.txt", "a", drive.Condition{})
	write(t, ctx, d, "/set/b.txt", "b", drive.Condition{})
	write(t, ctx, d, "/elsewhere.txt", "e", drive.Condition{})

	lease, err := d.Lock(ctx, drive.ByPath("/set/a.txt"), "tool-1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Item.Path != "/set/a.txt" || lease.Item.ID == "" || lease.Holder != "tool-1" || time.Until(lease.Expires) < 25*time.Second {
		t.Fatalf("lease: %+v", lease)
	}

	// Everyone else is kept out, whatever the operation.
	if _, err := other.Lock(ctx, drive.ByPath("/set/a.txt"), "tool-2", time.Minute); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("second lock: %v; want ErrLocked", err)
	}
	if _, err := other.Write(ctx, "/set/a.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("write without a holder: %v; want ErrLocked", err)
	}
	as2 := drive.WithLockHolder(ctx, "tool-2")
	if _, err := other.Write(as2, "/set/a.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("write as another holder: %v; want ErrLocked", err)
	}
	if _, err := other.Move(as2, drive.ByPath("/set/a.txt"), "/moved.txt", false); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("move: %v; want ErrLocked", err)
	}
	if _, err := other.Move(as2, drive.ByPath("/elsewhere.txt"), "/set/a.txt", true); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("move over it: %v; want ErrLocked", err)
	}
	// Nor is its folder moved out from under it, or another moved over it.
	if _, err := other.Move(as2, drive.ByPath("/set"), "/moved", false); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("move the folder: %v; want ErrLocked", err)
	}
	write(t, as2, other, "/other/x.txt", "x", drive.Condition{})
	if _, err := other.Move(as2, drive.ByPath("/other"), "/set", true); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("move a folder over its folder: %v; want ErrLocked", err)
	}
	if read(t, ctx, d, drive.ByPath("/set/a.txt"), drive.At{}) != "a" {
		t.Fatal("the folder moved")
	}
	if err := other.Delete(as2, drive.ByPath("/set/a.txt"), false, drive.Condition{}); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("delete: %v; want ErrLocked", err)
	}
	if err := other.Delete(as2, drive.ByPath("/set"), true, drive.Condition{}); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("delete the folder: %v; want ErrLocked", err)
	}
	if read(t, ctx, d, drive.ByPath("/set/b.txt"), drive.At{}) != "b" {
		t.Fatal("the folder delete touched b.txt before refusing")
	}
	// The unlocked neighbour is anyone's.
	write(t, as2, other, "/set/b.txt", "b2", drive.Condition{})

	// The holder gets through, even from another process.
	as1 := drive.WithLockHolder(ctx, "tool-1")
	write(t, as1, other, "/set/a.txt", "a2", drive.Condition{})
	if read(t, ctx, d, drive.ByPath("/set/a.txt"), drive.At{}) != "a2" {
		t.Fatal("the holder's write didn't land")
	}
	// Locking it again extends the lease, and another process can renew
	// and release it: the lease is the holder's, not the Drive's.
	if _, err := d.Lock(as1, drive.ByPath("/set/a.txt"), "tool-1", time.Minute); err != nil {
		t.Fatalf("lock again: %v", err)
	}
	if err := other.Renew(ctx, lease, time.Minute); err != nil {
		t.Fatalf("renew elsewhere: %v", err)
	}

	// It is the mount's lock: the filesystem's own functions see it, and
	// a lock they take (as a mount does) stops the drive.
	if _, held, err := d.fs.FindConflictingLockRange(ctx, lease.Item.ID, 0, icbfs.LockEOF, "someone", false); err != nil || !held {
		t.Fatalf("the filesystem doesn't see the drive's lock: %v, %v", held, err)
	}
	if err := d.fs.TryAcquireLock(ctx, itemKey(t, d, "/set/b.txt"), "mount-1/pid-7", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Write(as2, "/set/b.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrLocked) {
		t.Fatalf("write over a mount's lock: %v; want ErrLocked", err)
	}
	if err := d.fs.ReleaseLock(ctx, itemKey(t, d, "/set/b.txt"), "mount-1/pid-7"); err != nil {
		t.Fatal(err)
	}

	// Unlock, and the file is anyone's again; the lease is spent.
	if err := other.Unlock(ctx, lease); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	write(t, as2, other, "/set/a.txt", "a3", drive.Condition{})
	if err := d.Unlock(ctx, lease); err != nil {
		t.Fatalf("unlock a released lock: %v; want nothing (releasing a free range is a no-op)", err)
	}
	if _, err := other.Lock(ctx, drive.ByPath("/set/a.txt"), "tool-2", time.Minute); err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	if err := d.Renew(ctx, lease, time.Minute); !errors.Is(err, drive.ErrNotLockHolder) {
		t.Fatalf("renew another's lock: %v; want ErrNotLockHolder", err)
	}
	if err := d.Unlock(ctx, lease); !errors.Is(err, drive.ErrNotLockHolder) {
		t.Fatalf("unlock another's lock: %v; want ErrNotLockHolder", err)
	}

	// A lease lapses.
	short, err := d.Lock(ctx, drive.ByPath("/elsewhere.txt"), "tool-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	write(t, as2, other, "/elsewhere.txt", "e2", drive.Condition{})
	if err := d.Renew(ctx, short, time.Minute); !errors.Is(err, drive.ErrNotLockHolder) {
		t.Fatalf("renew a lapsed lease: %v; want ErrNotLockHolder", err)
	}

	// What can't be locked.
	if _, err := d.Lock(ctx, drive.ByPath("/set"), "tool-1", time.Minute); !errors.Is(err, drive.ErrIsDir) {
		t.Fatalf("lock a folder: %v; want ErrIsDir", err)
	}
	if _, err := d.Lock(ctx, drive.ByPath("/set/a.txt"), "", time.Minute); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("lock with no holder: %v; want ErrInvalid", err)
	}
	if _, err := d.Lock(ctx, drive.ByPath("/nowhere.txt"), "tool-1", time.Minute); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("lock nothing: %v; want ErrNotFound", err)
	}
	mode := uint32(0o644) // the root's, readable by all, written by none but its owner
	if _, err := d.Write(ctx, "/mine.txt", strings.NewReader("m"), drive.Condition{}, drive.WriteOptions{Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Lock(as(1000), drive.ByPath("/mine.txt"), "tool-1", time.Minute); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("lock without the right to write: %v; want ErrForbidden", err)
	}

	// Locks off: none taken, none honoured.
	off := newDriveWith(t, store, settings, Config{NoLocking: true})
	if off.Capabilities().Locks {
		t.Fatal("a drive with locks off says it locks")
	}
	if _, err := off.Lock(ctx, drive.ByPath("/set/a.txt"), "tool-3", time.Minute); !errors.Is(err, drive.ErrNotSupported) {
		t.Fatalf("lock with locks off: %v; want ErrNotSupported", err)
	}
	write(t, ctx, off, "/set/a.txt", "a4", drive.Condition{}) // tool-2's lock, ignored
}

// itemKey is the filesystem key (the id) of the item at p.
func itemKey(t *testing.T, d *Drive, p string) string {
	t.Helper()
	it, err := d.Stat(system(), drive.ByPath(p), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	return it.ID
}
