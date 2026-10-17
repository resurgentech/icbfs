package fuseserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// countingStore counts the reads a mount makes of its store.
type countingStore struct {
	objstore.Store
	reads atomic.Int64
}

func (s *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.reads.Add(1)
	return s.Store.Get(ctx, key)
}

func (s *countingStore) Head(ctx context.Context, key string) (*objstore.Object, error) {
	s.reads.Add(1)
	return s.Store.Head(ctx, key)
}

func (s *countingStore) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.reads.Add(1)
	return s.Store.List(ctx, prefix)
}

// mountCached mounts filesystem fsName from store with the kernel trusting
// names and attributes for d, creating the filesystem first if create is
// set, or attaching to it as another machine would.
func mountCached(t *testing.T, store objstore.Store, fsName string, create bool, d time.Duration) string {
	t.Helper()
	ctx := context.Background()
	fsys := icbfs.New(store, fsName)
	if create {
		fsys.SetCreationDirBlockEntries(testDirBlockEntries)
		if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	} else if err := fsys.Attach(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	fsys.EnableLocking(true)
	fsys.EnablePosixACLs(true)
	dir := t.TempDir()
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "icbfs-test", Name: "icbfs-test",
			EnableLocks: true, EnableAcl: true,
			Options: []string{"default_permissions"},
		},
		NullPermissions: true,
	}
	MetaCacheTimeouts(opts, d)
	server, err := Mount(dir, Root(fsys, nil, WithMetaCache(d)), opts)
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

// TestMetaCacheRepeatedStatCostsNothing: within the timeout, a stat of a
// path seven levels deep, already walked, is answered by the kernel with
// no store reads; without the cache, every component is asked again.
func TestMetaCacheRepeatedStatCostsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache time.Duration
	}{{"cached", time.Minute}, {"strict", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			store := &countingStore{Store: newMountTestStore(t)}
			mnt := mountCached(t, store, "fs", true, tc.cache)
			deep := filepath.Join(mnt, "a", "b", "c", "d", "e", "f")
			if err := os.MkdirAll(deep, 0755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(deep, "file")
			if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal(err)
			}
			before := store.reads.Load()
			for range 5 {
				if _, err := os.Stat(file); err != nil {
					t.Fatal(err)
				}
			}
			reads := store.reads.Load() - before
			if tc.cache > 0 && reads != 0 {
				t.Fatalf("5 cached stats made %d store reads, want 0", reads)
			}
			if tc.cache == 0 && reads < 5*7 {
				t.Fatalf("5 strict stats made %d store reads, want at least one per component each", reads)
			}
		})
	}
}

// eventually polls cond for up to limit.
func eventually(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, limit)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMetaCacheOtherMountsChangesShowWithinTimeout: another machine's
// rename, create and chmod reach this mount's cached view once the
// timeout passes.
func TestMetaCacheOtherMountsChangesShowWithinTimeout(t *testing.T) {
	store := newMountTestStore(t)
	const cache = time.Second
	a := mountCached(t, store, "fs", true, cache)
	b := mountCached(t, store, "fs", false, cache)
	if err := os.WriteFile(filepath.Join(a, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, "f")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat new before it exists: %v", err)
	}

	if err := os.Rename(filepath.Join(a, "f"), filepath.Join(a, "g")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "new"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(a, "g"), 0600); err != nil {
		t.Fatal(err)
	}
	limit := 3 * cache
	eventually(t, limit, "the rename away", func() bool {
		_, err := os.Stat(filepath.Join(b, "f"))
		return errors.Is(err, os.ErrNotExist)
	})
	eventually(t, limit, "the create (cached as not found)", func() bool {
		_, err := os.Stat(filepath.Join(b, "new"))
		return err == nil
	})
	eventually(t, limit, "the chmod", func() bool {
		fi, err := os.Stat(filepath.Join(b, "g"))
		return err == nil && fi.Mode().Perm() == 0600
	})
}

// TestMetaCacheOpenSeesNewerVersion: with attributes cached for far longer
// than the test, an open still reads another machine's newer, longer
// version whole, as close-to-open promises.
func TestMetaCacheOpenSeesNewerVersion(t *testing.T) {
	store := newMountTestStore(t)
	a := mountCached(t, store, "fs", true, time.Hour)
	b := mountCached(t, store, "fs", false, time.Hour)
	if err := os.WriteFile(filepath.Join(a, "f"), []byte("short"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(b, "f")); err != nil || string(got) != "short" {
		t.Fatalf("first read %q, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(b, "f")); err != nil || fi.Size() != 5 {
		t.Fatalf("stat %v, %v", fi, err)
	}
	const longer = "a good deal longer than before"
	if err := os.WriteFile(filepath.Join(a, "f"), []byte(longer), 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(b, "f")); err != nil || string(got) != longer {
		t.Fatalf("reopened and read %q, %v; want the newer version whole", got, err)
	}
}

// TestMetaCacheOpenByStaleName: a name cached for far longer than the test,
// removed or replaced on another machine, opens as what it means now, not
// as the item it used to lead to.
func TestMetaCacheOpenByStaleName(t *testing.T) {
	store := newMountTestStore(t)
	a := mountCached(t, store, "fs", true, time.Hour)
	b := mountCached(t, store, "fs", false, time.Hour)
	for _, name := range []string{"gone", "replaced"} {
		if err := os.WriteFile(filepath.Join(a, name), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(b, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(a, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(a, "replaced"), filepath.Join(a, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "replaced"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(b, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open of a name removed elsewhere: %v, want not found", err)
	}
	if got, err := os.ReadFile(filepath.Join(b, "replaced")); err != nil || string(got) != "new" {
		t.Fatalf("open of a name replaced elsewhere read %q, %v; want the new item", got, err)
	}
}

// TestMetaCacheExclusiveCreateStillExclusive: a name cached as not found
// doesn't let an O_EXCL create succeed over another machine's file.
func TestMetaCacheExclusiveCreateStillExclusive(t *testing.T) {
	store := newMountTestStore(t)
	a := mountCached(t, store, "fs", true, time.Hour)
	b := mountCached(t, store, "fs", false, time.Hour)
	if _, err := os.Stat(filepath.Join(b, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(a, "x"), []byte("a's"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(b, "x"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err == nil {
		f.Close()
		t.Fatal("O_EXCL create over another machine's file succeeded")
	}
	if !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("O_EXCL create: %v, want EEXIST", err)
	}
}
