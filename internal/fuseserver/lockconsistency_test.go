package fuseserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// mountAttached mounts the existing filesystem fsName from store a second
// time, as another machine would: its own Filesystem, its own lock holder
// identity, its own kernel inode and page cache.
func mountAttached(t *testing.T, store objstore.Store, fsName string) string {
	t.Helper()
	fsys := icbfs.New(store, fsName)
	if err := fsys.Attach(context.Background()); err != nil {
		t.Fatalf("attach: %v", err)
	}
	fsys.EnableLocking(true)
	fsys.EnablePosixACLs(true)
	dir := t.TempDir()
	server, err := Mount(dir, Root(fsys, nil), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "icbfs-test", Name: "icbfs-test",
			EnableLocks: true, EnableAcl: true,
			Options: []string{"default_permissions"},
		},
		NullPermissions: true,
	})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Logf("unmount: %v", err)
		}
	})
	return dir
}

// lockFile and unlockFile take and drop a whole-file exclusive lock by
// flock or by fcntl.
func lockFile(f *os.File, useFcntl bool) error {
	if useFcntl {
		return syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, &syscall.Flock_t{Type: syscall.F_WRLCK})
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File, useFcntl bool) error {
	if useFcntl {
		return syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &syscall.Flock_t{Type: syscall.F_UNLCK})
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// TestLockCyclesOnOneOpenFileAcrossMounts: two mounts of one filesystem
// each keep the file open for the whole run and increment a counter in it
// under a lock, read-modify-write, never closing or fsyncing in between.
// As with NFS, a release stores what was written under the lock, and an
// acquire brings the open file up to what the last holder stored, pages
// the kernel kept included, so no increment is lost.
func TestLockCyclesOnOneOpenFileAcrossMounts(t *testing.T) {
	for _, useFcntl := range []bool{false, true} {
		name := "flock"
		if useFcntl {
			name = "fcntl"
		}
		t.Run(name, func(t *testing.T) {
			store := newMountTestStore(t)
			a := mountFSWithLocking(t, store, "fs", true)
			b := mountAttached(t, store, "fs")
			if err := os.WriteFile(filepath.Join(a, "counter"), []byte(fmt.Sprintf("%8d", 0)), 0644); err != nil {
				t.Fatal(err)
			}

			const each = 8
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for _, mnt := range []string{a, b} {
				f, err := os.OpenFile(filepath.Join(mnt, "counter"), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				wg.Add(1)
				go func(f *os.File) {
					defer wg.Done()
					buf := make([]byte, 8)
					for range each {
						if err := lockFile(f, useFcntl); err != nil {
							errs <- fmt.Errorf("lock: %w", err)
							return
						}
						if _, err := f.ReadAt(buf, 0); err != nil {
							errs <- fmt.Errorf("read: %w", err)
							return
						}
						n, err := strconv.Atoi(strings.TrimSpace(string(buf)))
						if err != nil {
							errs <- fmt.Errorf("counter reads %q: %w", buf, err)
							return
						}
						if _, err := f.WriteAt([]byte(fmt.Sprintf("%8d", n+1)), 0); err != nil {
							errs <- fmt.Errorf("write: %w", err)
							return
						}
						if err := unlockFile(f, useFcntl); err != nil {
							errs <- fmt.Errorf("unlock: %w", err)
							return
						}
					}
				}(f)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}

			// Read from a fresh client, so it's the store's answer, not
			// either mount's.
			other := icbfs.New(store, "fs")
			ctx := context.Background()
			if err := other.Attach(ctx); err != nil {
				t.Fatal(err)
			}
			entry, _, err := other.Lookup(ctx, other.RootKey(), "counter")
			if err != nil {
				t.Fatal(err)
			}
			got, _, _, err := other.ReadFile(ctx, entry.UUID)
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := strconv.Atoi(strings.TrimSpace(string(got))); n != 2*each {
				t.Fatalf("the store's counter is %q, want %d: increments were lost", got, 2*each)
			}
		})
	}
}
