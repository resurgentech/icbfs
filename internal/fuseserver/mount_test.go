package fuseserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/notify"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil"
)

// newMountTestStore starts a MinIO container with a versioned bucket and
// returns a Store on it, with no filesystem yet, for tests that act on the
// store before mounting (archiving a filesystem first, say).
func newMountTestStore(t *testing.T) objstore.Store {
	t.Helper()
	return newS3Store(newMinIO(t))
}

// testMinIO is how to reach a test MinIO's versioned bucket.
type testMinIO struct {
	endpoint, bucket, accessKey, secretKey string
}

func newS3Store(m testMinIO) objstore.Store {
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(m.endpoint),
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(m.accessKey, m.secretKey, ""),
	})
	return objstore.NewS3Store(client, m.bucket)
}

// newMinIO starts a MinIO container with a versioned bucket.
func newMinIO(t *testing.T) testMinIO {
	t.Helper()
	ctx := context.Background()

	container, err := minio.Run(ctx, "minio/minio:latest")
	if err != nil {
		t.Fatalf("start minio container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate minio container: %v", err)
		}
	})

	endpoint, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("get minio connection string: %v", err)
	}

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("http://" + endpoint),
		UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(
			container.Username, container.Password, "",
		),
	})

	const bucket = "icbfs-mount-test"
	testutil.RetryUntilReady(t, 10*time.Second, func() error {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		return err
	})
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		t.Fatalf("enable bucket versioning: %v", err)
	}

	return testMinIO{endpoint: "http://" + endpoint, bucket: bucket, accessKey: container.Username, secretKey: container.Password}
}

// mountFS bootstraps fsName against store and mounts it, returning the
// mountpoint.
func mountFS(t *testing.T, store objstore.Store, fsName string) string {
	t.Helper()
	return mountFSWithLocking(t, store, fsName, false)
}

func mountFSWithLocking(t *testing.T, store objstore.Store, fsName string, locking bool) string {
	t.Helper()
	ctx := context.Background()

	fsys := icbfs.New(store, fsName)
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
		t.Fatalf("bootstrap filesystem: %v", err)
	}
	fsys.EnableLocking(locking)
	fsys.EnablePosixACLs(true)

	mountDir := t.TempDir()
	server, err := Mount(mountDir, Root(fsys, nil), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "icbfs-test",
			Name:   "icbfs-test",
			// EnableLocks: without it the kernel never sends lock requests
			// to this driver and uses its own local lock table instead,
			// which passes same-host tests without exercising the
			// .lock-object logic at all.
			EnableLocks: true,
			EnableAcl:   true,
			Options:     []string{"default_permissions"},
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

	return mountDir
}

// mountTestFS spins up a real MinIO container, a versioned bucket in it,
// and a real FUSE mount backed by it, returning the mountpoint. This is
// the end-to-end proof that icbfs is an actual, working filesystem: every
// operation below goes mountpoint -> kernel -> FUSE -> icbfs core ->
// real object store calls against a real MinIO instance, and back.
func mountTestFS(t *testing.T) string {
	t.Helper()
	return mountFS(t, newMountTestStore(t), "test")
}

// mountTestFSWithLocking is mountTestFS with the Locking feature
// turned on.
func mountTestFSWithLocking(t *testing.T) string {
	t.Helper()
	return mountFSWithLocking(t, newMountTestStore(t), "test", true)
}

// mountTestFSWithFsys is mountTestFS that also returns the
// *icbfs.Filesystem, for tests that create an item through it with an owner
// the test process isn't, which no application could do through the mount,
// to exercise group and other mode bits without a second real user.
func mountTestFSWithFsys(t *testing.T) (string, *icbfs.Filesystem) {
	t.Helper()
	ctx := context.Background()
	store := newMountTestStore(t)
	fsys := icbfs.New(store, "test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
		t.Fatalf("bootstrap filesystem: %v", err)
	}
	fsys.EnablePosixACLs(true)

	mountDir := t.TempDir()
	server, err := Mount(mountDir, Root(fsys, nil), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:    "icbfs-test",
			Name:      "icbfs-test",
			EnableAcl: true,
			Options:   []string{"default_permissions"},
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

	return mountDir, fsys
}

func TestMountBasicFileLifecycle(t *testing.T) {
	mnt := mountTestFS(t)

	path := filepath.Join(mnt, "hello.txt")
	if err := os.WriteFile(path, []byte("hello icbfs"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello icbfs" {
		t.Fatalf("got %q, want %q", data, "hello icbfs")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(len("hello icbfs")) {
		t.Fatalf("got size %d, want %d", info.Size(), len("hello icbfs"))
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("got mode %o, want %o", info.Mode().Perm(), 0644)
	}

	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat after chmod: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("got mode %o after chmod, want %o", info.Mode().Perm(), 0600)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be gone, got err: %v", err)
	}
}

// TestMountChmodToZeroIsReportedAccurately: mode 0000 stats as 0000.
// go-fuse reports it as 0644 or 0755 unless fs.Options.NullPermissions
// is set.
func TestMountChmodToZeroIsReportedAccurately(t *testing.T) {
	mnt := mountTestFS(t)
	path := filepath.Join(mnt, "locked.txt")
	if err := os.WriteFile(path, []byte("secret"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod 0: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0 {
		t.Fatalf("got mode %o after chmod 0, want 0 (go-fuse's NullPermissions default would silently report 644/755 here)", info.Mode().Perm())
	}
}

// TestMountDefaultPermissionsEnforcesOwnerModeBits: a chmod 000 file can't
// be read or written even by its owner, and a chmod 444 file can be read
// but not written: enforced by the kernel through default_permissions, not
// just reported (TestMountChmodToZeroIsReportedAccurately covers
// reporting).
func TestMountDefaultPermissionsEnforcesOwnerModeBits(t *testing.T) {
	mnt := mountTestFS(t)
	path := filepath.Join(mnt, "perm-test.txt")
	if err := os.WriteFile(path, []byte("secret"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read a chmod 000 file as its own owner = %v, want a permission error", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0644); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write a chmod 000 file as its own owner = %v, want a permission error", err)
	}

	if err := os.Chmod(path, 0444); err != nil {
		t.Fatalf("chmod 444: %v", err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("read a chmod 444 file as its own owner = %v, want nil", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0644); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write a chmod 444 file as its own owner = %v, want a permission error", err)
	}
}

// TestMountDefaultPermissionsEnforcesOtherBits checks the "other" mode bits
// without a second real user (that would need root): it creates a file
// through the Filesystem API owned by a uid and gid that aren't the test
// process's. default_permissions judges only what Getattr reports, so
// accessing that file through the mount exercises the "other" bits.
//
// It doesn't cover the group bits, or the kernel's check of a caller's
// supplementary groups; those need a second real identity.
func TestMountDefaultPermissionsEnforcesOtherBits(t *testing.T) {
	mnt, fsys := mountTestFSWithFsys(t)
	ctx := context.Background()

	otherUID := uint32(os.Getuid()) + 12345
	otherGID := uint32(os.Getgid()) + 12345
	// mode 0604: owner rw-, group ---, other r--.
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "other-bits.txt", 0604, otherUID, otherGID)
	if err != nil {
		t.Fatalf("create via Filesystem API: %v", err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, []byte("secret"), ""); err != nil {
		t.Fatalf("seed content: %v", err)
	}

	path := filepath.Join(mnt, "other-bits.txt")
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("read as 'other' (mode 0604, other bits r--) = %v, want nil", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0644); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write as 'other' (mode 0604, other bits r--, no write) = %v, want a permission error", err)
	}
}

func TestMountDirectoryLifecycle(t *testing.T) {
	mnt := mountTestFS(t)

	dir := filepath.Join(mnt, "subdir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	filePath := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(filePath, []byte("a"), 0644); err != nil {
		t.Fatalf("write in subdir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "a.txt" {
		t.Fatalf("got entries %v, want [a.txt]", entries)
	}

	if err := os.Remove(dir); err == nil {
		t.Fatal("expected rmdir on a non-empty directory to fail")
	}

	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("rmdir after emptying: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected dir to be gone, got err: %v", err)
	}
}

func TestMountSymlink(t *testing.T) {
	mnt := mountTestFS(t)

	linkPath := filepath.Join(mnt, "link")
	if err := os.Symlink("/some/target", linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "/some/target" {
		t.Fatalf("got target %q, want %q", target, "/some/target")
	}

	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected a symlink, got mode %v", info.Mode())
	}
}

func TestMountHardlink(t *testing.T) {
	mnt := mountTestFS(t)

	original := filepath.Join(mnt, "original.txt")
	if err := os.WriteFile(original, []byte("shared content"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	linked := filepath.Join(mnt, "linked.txt")
	if err := os.Link(original, linked); err != nil {
		t.Fatalf("link: %v", err)
	}

	data, err := os.ReadFile(linked)
	if err != nil {
		t.Fatalf("read via hardlink: %v", err)
	}
	if string(data) != "shared content" {
		t.Fatalf("got %q via hardlink, want %q", data, "shared content")
	}

	origInfo, err := os.Stat(original)
	if err != nil {
		t.Fatalf("stat original: %v", err)
	}
	linkedInfo, err := os.Stat(linked)
	if err != nil {
		t.Fatalf("stat linked: %v", err)
	}
	origStat := origInfo.Sys().(*syscall.Stat_t)
	linkedStat := linkedInfo.Sys().(*syscall.Stat_t)
	if origStat.Ino != linkedStat.Ino {
		t.Fatalf("hardlinked paths report different inodes: %d vs %d", origStat.Ino, linkedStat.Ino)
	}
	if origStat.Nlink != 2 {
		t.Fatalf("got nlink %d, want 2", origStat.Nlink)
	}

	// Removing one name must not remove the content, since the other name
	// still references it.
	if err := os.Remove(original); err != nil {
		t.Fatalf("remove original: %v", err)
	}
	data, err = os.ReadFile(linked)
	if err != nil {
		t.Fatalf("read via remaining hardlink after removing original: %v", err)
	}
	if string(data) != "shared content" {
		t.Fatalf("got %q after removing original, want %q", data, "shared content")
	}
}

// TestMountRenameBasic exercises Rename against a real FUSE mount: plain rename within one
// directory, rename into a different directory, and rename replacing
// an existing destination file (the "write a temp file, rename it
// into place" pattern rsync and git depend on).
func TestMountRenameBasic(t *testing.T) {
	mnt := mountTestFS(t)

	oldPath := filepath.Join(mnt, "old.txt")
	if err := os.WriteFile(oldPath, []byte("hello"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	newPath := filepath.Join(mnt, "new.txt")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path still exists after rename, err: %v", err)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read new path: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q, want %q", data, "hello")
	}

	subdir := filepath.Join(mnt, "subdir")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	movedPath := filepath.Join(subdir, "new.txt")
	if err := os.Rename(newPath, movedPath); err != nil {
		t.Fatalf("rename across directories: %v", err)
	}
	data, err = os.ReadFile(movedPath)
	if err != nil {
		t.Fatalf("read moved path: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q after cross-directory rename, want %q", data, "hello")
	}

	// Replace-into-place: write a fresh temp file, rename it onto an
	// existing destination — the real rsync/git pattern.
	tempPath := filepath.Join(mnt, ".tmp-write")
	if err := os.WriteFile(tempPath, []byte("replaced"), 0644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.Rename(tempPath, movedPath); err != nil {
		t.Fatalf("rename-into-place over existing destination: %v", err)
	}
	data, err = os.ReadFile(movedPath)
	if err != nil {
		t.Fatalf("read after rename-into-place: %v", err)
	}
	if string(data) != "replaced" {
		t.Fatalf("got %q after rename-into-place, want %q", data, "replaced")
	}
}

// TestMountRenameOfOpenFileKeepsHandleUsable: rename(2) changes only a
// directory entry, so a descriptor opened under the old name keeps working,
// and what it writes is what the new name reads.
func TestMountRenameOfOpenFileKeepsHandleUsable(t *testing.T) {
	mnt := mountTestFS(t)

	oldPath := filepath.Join(mnt, "open-then-renamed.txt")
	if err := os.WriteFile(oldPath, []byte("before"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	f, err := os.OpenFile(oldPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	newPath := filepath.Join(mnt, "renamed-target.txt")
	if err := os.Rename(oldPath, newPath); err != nil {
		f.Close()
		t.Fatalf("rename while open: %v", err)
	}

	// The already-open handle must still work after its path's only
	// name changed out from under it.
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		t.Fatalf("seek on handle open across rename: %v", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		f.Close()
		t.Fatalf("read via handle open across rename: %v", err)
	}
	if string(data) != "before" {
		f.Close()
		t.Fatalf("got %q via still-open handle, want %q", data, "before")
	}
	if _, err := f.WriteAt([]byte("AFTER"), 0); err != nil {
		f.Close()
		t.Fatalf("write via handle open across rename: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close (flushes the write): %v", err)
	}

	// The write through the old handle must be visible at the new
	// name — same underlying object, unaffected by which directory
	// entry currently points at it.
	gotAfter, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read new path after write-through-old-handle: %v", err)
	}
	if string(gotAfter) != "AFTERe" { // "before" with its first 5 bytes overwritten
		t.Fatalf("got %q at new path, want %q", gotAfter, "AFTERe")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path still exists after rename, err: %v", err)
	}
}

func TestMountAppendAcrossOpens(t *testing.T) {
	mnt := mountTestFS(t)
	path := filepath.Join(mnt, "grow.txt")

	if err := os.WriteFile(path, []byte("first-"), 0644); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteAt([]byte("second"), int64(len("first-"))); err != nil {
		t.Fatalf("writeat: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first-second" {
		t.Fatalf("got %q, want %q", data, "first-second")
	}
}

func TestMountLargeDirectorySharding(t *testing.T) {
	mnt := mountTestFS(t)
	dir := filepath.Join(mnt, "many")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const n = 40 // forces several splits (maxEntriesPerBlock is 8)
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, "f"+itoa(i))
		if err := os.WriteFile(name, []byte(itoa(i)), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("got %d entries via ls, want %d", len(entries), n)
	}

	// Spot-check a few individual files resolve correctly too (point
	// lookup through the sharded tree, not just the full listing).
	for _, i := range []int{0, n / 2, n - 1} {
		data, err := os.ReadFile(filepath.Join(dir, "f"+itoa(i)))
		if err != nil {
			t.Fatalf("read f%d: %v", i, err)
		}
		if string(data) != itoa(i) {
			t.Fatalf("got %q, want %q", data, itoa(i))
		}
	}
}

func itoa(i int) string {
	return fmt.Sprintf("%03d", i)
}

func TestMountBirthtimeFromUUID(t *testing.T) {
	mnt := mountTestFS(t)
	path := filepath.Join(mnt, "fresh.txt")

	before := time.Now().Add(-2 * time.Second)
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	after := time.Now().Add(2 * time.Second)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// We don't surface Btime through os.FileInfo portably; this test just
	// confirms mtime (which we do surface) lands in the expected window,
	// as a smoke check that timestamps are flowing through at all.
	if info.ModTime().Before(before) || info.ModTime().After(after) {
		t.Fatalf("mtime %v not within [%v, %v]", info.ModTime(), before, after)
	}
}

// TestMountStatfsReflectsDeclaredSizeAndUsage: df's total is the declared
// size (1 GiB here), and writing a file moves its usage. An unwired statfs
// would report zeros.
func TestMountStatfsReflectsDeclaredSizeAndUsage(t *testing.T) {
	mnt := mountTestFS(t)

	var before syscall.Statfs_t
	if err := syscall.Statfs(mnt, &before); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	const declaredSize = uint64(1) << 30
	gotTotal := uint64(before.Blocks) * uint64(before.Bsize)
	if gotTotal != declaredSize {
		t.Fatalf("statfs Total = %d bytes, want the declared size %d", gotTotal, declaredSize)
	}
	if before.Bfree == 0 || before.Bfree != before.Bavail {
		t.Fatalf("statfs Bfree=%d Bavail=%d on a fresh filesystem look wrong (zeroed NodeStatfser result?)", before.Bfree, before.Bavail)
	}

	path := filepath.Join(mnt, "statfs-usage.bin")
	if err := os.WriteFile(path, make([]byte, 5*1024*1024), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var after syscall.Statfs_t
	if err := syscall.Statfs(mnt, &after); err != nil {
		t.Fatalf("statfs after write: %v", err)
	}
	if after.Bfree >= before.Bfree {
		t.Fatalf("statfs Bfree after a 5MB write = %d, want less than before (%d) — Used isn't moving", after.Bfree, before.Bfree)
	}
}

// TestMountArchivedFilesystemRejectsWritesButAllowsReads: a filesystem
// archived before mounting mounts read-only (EROFS on writes), because the
// mount reads the archived flag when it starts.
func TestMountArchivedFilesystemRejectsWritesButAllowsReads(t *testing.T) {
	store := newMountTestStore(t)
	ctx := context.Background()

	fsys := icbfs.New(store, "archived-mount-test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, _, _, err := fsys.Create(ctx, fsys.RootKey(), "before-archive.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create before archive: %v", err)
	}
	if err := icbfs.Archive(ctx, store, "archived-mount-test"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	mnt := mountFS(t, store, "archived-mount-test")

	if _, err := os.ReadFile(filepath.Join(mnt, "before-archive.txt")); err != nil {
		t.Fatalf("read on an archived filesystem should succeed, got: %v", err)
	}

	err := os.WriteFile(filepath.Join(mnt, "after-archive.txt"), []byte("nope"), 0644)
	if !errors.Is(err, syscall.EROFS) {
		t.Fatalf("create on an archived filesystem = %v, want EROFS", err)
	}

	if err := os.Mkdir(filepath.Join(mnt, "nope-dir"), 0755); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("mkdir on an archived filesystem = %v, want EROFS", err)
	}

	if err := os.Remove(filepath.Join(mnt, "before-archive.txt")); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("unlink on an archived filesystem = %v, want EROFS", err)
	}
}

// TestMountFlockWholeFileExclusive covers flock(2): real kernel flock syscalls against a real mount,
// not the Filesystem-level API directly. flock is always whole-file —
// a second fd's non-blocking exclusive attempt must fail while the
// first fd holds it, and succeed once released.
func TestMountFlockWholeFileExclusive(t *testing.T) {
	mnt := mountTestFSWithLocking(t)
	path := filepath.Join(mnt, "flock-test.bin")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	f1, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open f1: %v", err)
	}
	defer f1.Close()
	f2, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open f2: %v", err)
	}
	defer f2.Close()

	if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock f1 LOCK_EX: %v", err)
	}

	if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("flock f2 LOCK_EX|LOCK_NB while f1 holds it = %v, want EAGAIN", err)
	}

	if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("flock f1 LOCK_UN: %v", err)
	}

	if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock f2 after f1 released = %v, want nil", err)
	}
}

// TestMain runs the test binary as a lock-holding helper process when asked
// (runFcntlLockHolderHelper). fcntl locks belong to a process, not a
// descriptor, so two descriptors in one process never conflict: testing
// fcntl conflicts needs a second process.
func TestMain(m *testing.M) {
	if os.Getenv("ICBFS_FCNTL_HELPER") == "1" {
		runFcntlLockHolderHelper()
		return
	}
	os.Exit(m.Run())
}

// runFcntlLockHolderHelper opens ICBFS_FCNTL_HELPER_PATH (on the parent's
// mount), takes F_SETLK on [0,10) of type ICBFS_FCNTL_HELPER_TYPE
// ("F_RDLCK" or "F_WRLCK", default F_WRLCK), creates
// ICBFS_FCNTL_HELPER_READY, and waits for ICBFS_FCNTL_HELPER_RELEASE before
// exiting, which releases the lock.
func runFcntlLockHolderHelper() {
	path := os.Getenv("ICBFS_FCNTL_HELPER_PATH")
	readyPath := os.Getenv("ICBFS_FCNTL_HELPER_READY")
	releasePath := os.Getenv("ICBFS_FCNTL_HELPER_RELEASE")

	lockType := int16(syscall.F_WRLCK)
	if os.Getenv("ICBFS_FCNTL_HELPER_TYPE") == "F_RDLCK" {
		lockType = syscall.F_RDLCK
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open:", err)
		os.Exit(1)
	}
	lk := syscall.Flock_t{Type: lockType, Whence: 0, Start: 0, Len: 10}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		fmt.Fprintln(os.Stderr, "helper: F_SETLK [0,10):", err)
		os.Exit(1)
	}
	if err := os.WriteFile(readyPath, []byte("ready"), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "helper: signal ready:", err)
		os.Exit(1)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(releasePath); err == nil {
			os.Exit(0)
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "helper: timed out waiting for release signal")
			os.Exit(1)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForFileOrFail(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to appear", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// retryFcntlSetlkUntilNotEagain retries a non-blocking F_SETLK that should
// succeed once another process's exit releases its lock, allowing a short
// window of EAGAIN: the kernel sends FUSE's RELEASE asynchronously, after
// close or exit returns.
func retryFcntlSetlkUntilNotEagain(t *testing.T, fd uintptr, lk *syscall.Flock_t, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.FcntlFlock(fd, syscall.F_SETLK, lk)
		if !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMountFcntlByteRangeLocksAcrossProcesses, with a second process (see
// TestMain): while it holds a lock, an overlapping F_SETLK is refused, a
// non-overlapping one succeeds, and F_GETLK reports the conflict; once it
// exits, the overlapping range can be locked.
func TestMountFcntlByteRangeLocksAcrossProcesses(t *testing.T) {
	mnt := mountTestFSWithLocking(t)
	path := filepath.Join(mnt, "fcntl-cross-process.bin")
	if err := os.WriteFile(path, make([]byte, 100), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	signalDir := t.TempDir()
	readyPath := filepath.Join(signalDir, "ready")
	releasePath := filepath.Join(signalDir, "release")

	cmd := exec.Command(os.Args[0], "-test.run=NoSuchTest")
	cmd.Env = append(os.Environ(),
		"ICBFS_FCNTL_HELPER=1",
		"ICBFS_FCNTL_HELPER_PATH="+path,
		"ICBFS_FCNTL_HELPER_READY="+readyPath,
		"ICBFS_FCNTL_HELPER_RELEASE="+releasePath,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0644)
		_ = cmd.Wait()
	}()

	waitForFileOrFail(t, readyPath, 5*time.Second)

	f2, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open f2: %v", err)
	}
	defer f2.Close()

	overlapping := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 5, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_SETLK, &overlapping); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("F_SETLK f2 [5,15) while helper process holds [0,10) = %v, want EAGAIN", err)
	}

	query := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 5, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_GETLK, &query); err != nil {
		t.Fatalf("F_GETLK f2 [5,15): %v", err)
	}
	if query.Type != syscall.F_WRLCK {
		t.Fatalf("F_GETLK reported Type=%d, want F_WRLCK (a real conflict from the helper process)", query.Type)
	}

	nonOverlapping := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 20, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_SETLK, &nonOverlapping); err != nil {
		t.Fatalf("F_SETLK f2 [20,30) while helper holds [0,10) = %v, want nil", err)
	}

	if err := os.WriteFile(releasePath, []byte("release"), 0644); err != nil {
		t.Fatalf("signal release: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper process exited with error: %v", err)
	}

	if err := retryFcntlSetlkUntilNotEagain(t, f2.Fd(), &overlapping, time.Second); err != nil {
		t.Fatalf("F_SETLK f2 [5,15) after helper process exited (and released) = %v, want nil", err)
	}
}

// TestMountFcntlCloseOfAnyDescriptorReleases: closing any descriptor for a
// file releases the process's fcntl locks on it, not just one taken through
// that descriptor (POSIX).
func TestMountFcntlCloseOfAnyDescriptorReleases(t *testing.T) {
	mnt := mountTestFSWithLocking(t)
	path := filepath.Join(mnt, "fcntl-any-close.bin")
	if err := os.WriteFile(path, make([]byte, 100), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// helperLocks runs the lock-holder helper, which takes [0,10), and
	// reports whether it got it.
	helperLocks := func() bool {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=NoSuchTest")
		cmd.Env = append(os.Environ(),
			"ICBFS_FCNTL_HELPER=1",
			"ICBFS_FCNTL_HELPER_PATH="+path,
			"ICBFS_FCNTL_HELPER_READY="+filepath.Join(dir, "ready"),
			"ICBFS_FCNTL_HELPER_RELEASE="+filepath.Join(dir, "release"),
		)
		return cmd.Run() == nil
	}

	f1, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f1.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 10}
	if err := syscall.FcntlFlock(f1.Fd(), syscall.F_SETLK, &lk); err != nil {
		t.Fatalf("F_SETLK [0,10): %v", err)
	}
	if helperLocks() {
		t.Fatal("another process locked [0,10) while this one holds it")
	}

	f2, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}
	if !helperLocks() {
		t.Fatal("closing a second descriptor didn't release this process's lock")
	}
}

// TestMountFcntlLockSurvivesPastOriginalTTLViaRenewal: a lock held by a
// live process with the file open is still held after several lease
// periods, because renewal (ensureRenewal, runRenewal) keeps it alive.
//
// The lease is shrunk to 4 s, renewed every second, and checked after 10 s.
// A lease's expiry is the store's clock, which reads in whole seconds,
// plus the lease, so a new one can have as little as 3 s left: room for a
// renewal (three store round trips) to stall for about 2 s on a busy host
// without the lease lapsing. Without renewal it would lapse twice over.
func TestMountFcntlLockSurvivesPastOriginalTTLViaRenewal(t *testing.T) {
	origTTL, origInterval := lockLeaseTTL, lockRenewInterval
	// Restore the lease settings in t.Cleanup, registered before the mount
	// so it runs after the unmount: Release runs asynchronously after
	// close, and its renewal goroutines read these vars until the unmount
	// has drained them (a plain defer raced, under -race).
	t.Cleanup(func() { lockLeaseTTL, lockRenewInterval = origTTL, origInterval })
	lockLeaseTTL = 4 * time.Second
	lockRenewInterval = time.Second

	mnt := mountTestFSWithLocking(t)
	path := filepath.Join(mnt, "fcntl-renewal.bin")
	if err := os.WriteFile(path, make([]byte, 100), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	signalDir := t.TempDir()
	readyPath := filepath.Join(signalDir, "ready")
	releasePath := filepath.Join(signalDir, "release")

	cmd := exec.Command(os.Args[0], "-test.run=NoSuchTest")
	cmd.Env = append(os.Environ(),
		"ICBFS_FCNTL_HELPER=1",
		"ICBFS_FCNTL_HELPER_PATH="+path,
		"ICBFS_FCNTL_HELPER_READY="+readyPath,
		"ICBFS_FCNTL_HELPER_RELEASE="+releasePath,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0644)
		_ = cmd.Wait()
	}()

	waitForFileOrFail(t, readyPath, 5*time.Second)

	// The helper never renews; only this process's FileHandle serving its
	// open can. So the lock still conflicting after 2.5 lease periods
	// means renewal works.
	time.Sleep(lockLeaseTTL * 5 / 2)

	f2, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open f2: %v", err)
	}
	defer f2.Close()

	overlapping := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 5, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_SETLK, &overlapping); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("F_SETLK f2 [5,15) after 2.5 lease periods while the helper stayed alive = %v, want EAGAIN (renewal should have kept the lock held)", err)
	}
}

// TestMountWithNoNotificationBackendBehavesIdentically: a mount without a
// notification source (Root's signals nil, as cmd/icbfs without -notify)
// behaves as if the feature didn't exist, across create, read, write,
// mkdir, rmdir and unlink.
func TestMountWithNoNotificationBackendBehavesIdentically(t *testing.T) {
	mnt := mountTestFS(t) // mountTestFS always passes a nil source

	dir := filepath.Join(mnt, "e2-dir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "e2-file.txt")
	if err := os.WriteFile(path, []byte("no notifications here"), 0644); err != nil {
		t.Fatalf("create/write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "no notifications here" {
		t.Fatalf("got %q, want %q", data, "no notifications here")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected dir to be gone, got err: %v", err)
	}
}

// TestMountNotifyDispatchesSignalToCorrectInode: a change made by a second,
// independent client (another Filesystem on the same bucket) and signalled
// through a FakeSource reaches this mount as a NotifyContent call on the
// right inode, accepted by the kernel.
//
// It checks the call through the notifyContentHook seam, not through
// inotify or the page cache: NotifyContent produces no inotify event on
// this kernel and go-fuse, and this mount keeps no page cache to go stale.
// A naive registry once targeted an inode the kernel didn't know (ENOENT;
// see watchRegistry.register).
func TestMountNotifyDispatchesSignalToCorrectInode(t *testing.T) {
	ctx := context.Background()
	store := newMountTestStore(t)
	fsys := icbfs.New(store, "test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	source := notify.NewFakeSource(4)
	mountDir := t.TempDir()
	server, err := Mount(mountDir, Root(fsys, source.Signals()), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:  "icbfs-test",
			Name:    "icbfs-test",
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

	path := filepath.Join(mountDir, "watched.txt")
	if err := os.WriteFile(path, []byte("v1"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A plain lookup (os.Open does one) is what registers this UUID in
	// the watch registry — see newChild's doc comment.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	entry, _, err := fsys.Lookup(ctx, fsys.RootKey(), "watched.txt")
	if err != nil {
		t.Fatalf("lookup via Filesystem API (to get the real UUID key): %v", err)
	}

	type dispatchResult struct {
		key   string
		errno syscall.Errno
	}
	resultCh := make(chan dispatchResult, 1)
	notifyContentHook = func(key string, errno syscall.Errno) {
		resultCh <- dispatchResult{key: key, errno: errno}
	}
	t.Cleanup(func() { notifyContentHook = nil })

	// The second, independent client: writes directly to the backend
	// object this mount's "watched.txt" resolves to, never going
	// through this mount's FUSE layer at all.
	if _, _, err := fsys.WriteFile(ctx, entry.UUID, []byte("v2 from another client"), ""); err != nil {
		t.Fatalf("second client write: %v", err)
	}

	// Simulate the real backend adapter having observed that
	// write and delivered a signal.
	source.Emit(notify.Signal{Key: entry.UUID})

	select {
	case res := <-resultCh:
		if res.key != entry.UUID {
			t.Fatalf("dispatch targeted key %q, want %q", res.key, entry.UUID)
		}
		if res.errno != 0 {
			t.Fatalf("NotifyContent for %q = errno %d, want 0 (success)", res.key, res.errno)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the signal to be dispatched")
	}
}

// TestMountNotifyRegistryForgets: once the kernel forgets an item's inode
// (here: unlinked and closed), its watch entry goes, so a long-running
// mount's registry doesn't grow with every item it has ever touched.
func TestMountNotifyRegistryForgets(t *testing.T) {
	ctx := context.Background()
	store := newMountTestStore(t)
	fsys := icbfs.New(store, "test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	source := notify.NewFakeSource(4)
	root := Root(fsys, source.Signals())
	mountDir := t.TempDir()
	server, err := Mount(mountDir, root, &fs.Options{
		MountOptions:    fuse.MountOptions{FsName: "icbfs-test", Name: "icbfs-test", Options: []string{"default_permissions"}},
		NullPermissions: true,
	})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() { _ = server.Unmount() })

	base := root.watches.len()
	for i := 0; i < 5; i++ {
		path := filepath.Join(mountDir, fmt.Sprintf("f%d", i))
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}
	if got := root.watches.len(); got < base+5 {
		t.Fatalf("registry has %d entries after touching 5 files, want at least %d", got, base+5)
	}
	for i := 0; i < 5; i++ {
		if err := os.Remove(filepath.Join(mountDir, fmt.Sprintf("f%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for root.watches.len() > base {
		if time.Now().After(deadline) {
			t.Fatalf("registry still has %d entries after the files were removed, want %d", root.watches.len(), base)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMountFcntlSharedLocksCoexistAcrossProcesses, with a second process:
// its F_RDLCK doesn't block this process's F_RDLCK on the same range, but
// does block an F_WRLCK, which succeeds once the helper releases.
func TestMountFcntlSharedLocksCoexistAcrossProcesses(t *testing.T) {
	mnt := mountTestFSWithLocking(t)
	path := filepath.Join(mnt, "fcntl-shared-cross-process.bin")
	if err := os.WriteFile(path, make([]byte, 100), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	signalDir := t.TempDir()
	readyPath := filepath.Join(signalDir, "ready")
	releasePath := filepath.Join(signalDir, "release")

	cmd := exec.Command(os.Args[0], "-test.run=NoSuchTest")
	cmd.Env = append(os.Environ(),
		"ICBFS_FCNTL_HELPER=1",
		"ICBFS_FCNTL_HELPER_PATH="+path,
		"ICBFS_FCNTL_HELPER_READY="+readyPath,
		"ICBFS_FCNTL_HELPER_RELEASE="+releasePath,
		"ICBFS_FCNTL_HELPER_TYPE=F_RDLCK",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = os.WriteFile(releasePath, []byte("release"), 0644)
		_ = cmd.Wait()
	}
	defer release()

	waitForFileOrFail(t, readyPath, 5*time.Second)

	f2, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open f2: %v", err)
	}
	defer f2.Close()

	readLock := syscall.Flock_t{Type: syscall.F_RDLCK, Whence: 0, Start: 0, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_SETLK, &readLock); err != nil {
		t.Fatalf("F_SETLK F_RDLCK [0,10) while helper process holds F_RDLCK = %v, want nil (shared locks from different holders coexist)", err)
	}

	writeLock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_SETLK, &writeLock); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("F_SETLK F_WRLCK [0,10) while helper process holds F_RDLCK = %v, want EAGAIN", err)
	}

	release()

	if err := retryFcntlSetlkUntilNotEagain(t, f2.Fd(), &writeLock, time.Second); err != nil {
		t.Fatalf("F_SETLK F_WRLCK [0,10) after helper process released its F_RDLCK = %v, want nil", err)
	}
}

// TestMountFcntlGetlkReportsSharedType: F_GETLK as F_RDLCK against another
// process's F_RDLCK reports no conflict, and as F_WRLCK reports the
// conflict, as F_RDLCK.
func TestMountFcntlGetlkReportsSharedType(t *testing.T) {
	mnt := mountTestFSWithLocking(t)
	path := filepath.Join(mnt, "fcntl-getlk-shared.bin")
	if err := os.WriteFile(path, make([]byte, 100), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	signalDir := t.TempDir()
	readyPath := filepath.Join(signalDir, "ready")
	releasePath := filepath.Join(signalDir, "release")

	cmd := exec.Command(os.Args[0], "-test.run=NoSuchTest")
	cmd.Env = append(os.Environ(),
		"ICBFS_FCNTL_HELPER=1",
		"ICBFS_FCNTL_HELPER_PATH="+path,
		"ICBFS_FCNTL_HELPER_READY="+readyPath,
		"ICBFS_FCNTL_HELPER_RELEASE="+releasePath,
		"ICBFS_FCNTL_HELPER_TYPE=F_RDLCK",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0644)
		_ = cmd.Wait()
	}()

	waitForFileOrFail(t, readyPath, 5*time.Second)

	f2, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open f2: %v", err)
	}
	defer f2.Close()

	sharedQuery := syscall.Flock_t{Type: syscall.F_RDLCK, Whence: 0, Start: 0, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_GETLK, &sharedQuery); err != nil {
		t.Fatalf("F_GETLK (shared query): %v", err)
	}
	if sharedQuery.Type != syscall.F_UNLCK {
		t.Fatalf("F_GETLK shared query against another process's F_RDLCK reported Type=%d, want F_UNLCK (no conflict)", sharedQuery.Type)
	}

	exclusiveQuery := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 10}
	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_GETLK, &exclusiveQuery); err != nil {
		t.Fatalf("F_GETLK (exclusive query): %v", err)
	}
	if exclusiveQuery.Type != syscall.F_RDLCK {
		t.Fatalf("F_GETLK exclusive query against another process's F_RDLCK reported Type=%d, want F_RDLCK", exclusiveQuery.Type)
	}
}

// TestMountUnflushedWritesAndUnlinkedOpenFiles covers two POSIX rules a
// single mount keeps on its own: fstat (and stat) see a write before it's
// flushed, and an unlinked file stays usable through its open
// descriptors until the last one closes (pjdfstest's open/07.t and
// unlink/14.t).
func TestMountUnflushedWritesAndUnlinkedOpenFiles(t *testing.T) {
	mnt, fsys := mountTestFSWithFsys(t)
	path := filepath.Join(mnt, "f")

	w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if fi, err := w.Stat(); err != nil || fi.Size() != 1 {
		t.Fatalf("fstat after an unflushed write: size %v, %v; want 1", fi.Size(), err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 1 {
		t.Fatalf("stat by path after an unflushed write: size %v, %v; want 1", fi.Size(), err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Unlink while open: the name goes, the descriptor keeps working.
	rw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(rw.Fd()), &st); err != nil {
		t.Fatalf("fstat: %v", err)
	}
	entries, err := fsys.ReadDir(context.Background(), fsys.RootKey())
	if err != nil || len(entries) != 1 {
		t.Fatalf("readdir: %v, %v", entries, err)
	}
	key := entries[0].UUID

	if _, err := rw.WriteAt([]byte("Hello"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after unlink: %v, want not exist", err)
	}
	if err := syscall.Fstat(int(rw.Fd()), &st); err != nil || st.Nlink != 0 || st.Size != 5 {
		t.Fatalf("fstat of the unlinked open file: nlink %d size %d, %v; want nlink 0 size 5", st.Nlink, st.Size, err)
	}
	buf := make([]byte, 5)
	if n, err := rw.ReadAt(buf, 0); err != nil || string(buf[:n]) != "Hello" {
		t.Fatalf("read of the unlinked open file: %q, %v", buf[:n], err)
	}
	if _, err := fsys.Stat(context.Background(), key, icbfs.TypeFile); err != nil {
		t.Fatalf("objects gone while the file is still open: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close of the unlinked file: %v", err)
	}
	// Release reaches the filesystem asynchronously after close(2).
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := fsys.Stat(context.Background(), key, icbfs.TypeFile)
		if errors.Is(err, icbfs.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("objects still there after the last close: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMountAsOf mounts an as-of view of a filesystem read-only, the way
// `icbfs mount -as-of` does, and checks it shows the past through real
// syscalls, refuses writes with EROFS, and reports the past's df usage.
func TestMountAsOf(t *testing.T) {
	ctx := context.Background()
	store := newMountTestStore(t)
	live := mountFS(t, store, "past")
	path := filepath.Join(live, "f")
	if err := os.WriteFile(path, []byte("before"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	at, err := store.ServerTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte("after, and longer"), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.WriteFile(filepath.Join(live, "g"), []byte("new"), 0644); err != nil {
		t.Fatalf("write g: %v", err)
	}

	view, err := objstore.NewAsOf(ctx, store, at, t.TempDir())
	if err != nil {
		t.Fatalf("as of: %v", err)
	}
	defer view.Close()
	fsys := icbfs.New(view, "past")
	if err := fsys.BootstrapReadOnly(ctx); err != nil {
		t.Fatalf("bootstrap read-only: %v", err)
	}
	mnt := t.TempDir()
	server, err := Mount(mnt, Root(fsys, nil), &fs.Options{
		MountOptions:    fuse.MountOptions{FsName: "icbfs-asof", Name: "icbfs-asof", Options: []string{"default_permissions", "ro"}},
		NullPermissions: true,
	})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer server.Unmount()

	if data, err := os.ReadFile(filepath.Join(mnt, "f")); err != nil || string(data) != "before" {
		t.Fatalf("read in the past: %q, %v; want %q", data, err, "before")
	}
	if _, err := os.Stat(filepath.Join(mnt, "g")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file created later is visible in the past: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "f"), []byte("x"), 0644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write in the past: %v, want EROFS", err)
	}
	if err := os.Mkdir(filepath.Join(mnt, "d"), 0755); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("mkdir in the past: %v, want EROFS", err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(mnt, &st); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	_, used, err := fsys.StatFS(ctx)
	if err != nil || used == 0 {
		t.Fatalf("df used as of then: %d, %v", used, err)
	}
	if got := (st.Blocks - st.Bfree) * uint64(st.Bsize); got != (used/statfsBlockSize)*statfsBlockSize && got != ((used+statfsBlockSize-1)/statfsBlockSize)*statfsBlockSize {
		t.Fatalf("statfs used %d bytes, want about %d", got, used)
	}
}

// TestInterruptedFlushStillStores: an interrupt cancels the request's
// context (and the Go runtime's preemption signals interrupt FUSE
// requests all the time under load). A flush must finish anyway —
// cancelled, it used to fail with EIO and leave the data unstored.
func TestInterruptedFlushStillStores(t *testing.T) {
	ctx := context.Background()
	fsys := icbfs.New(newMountTestStore(t), "interrupt")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	key, _, etag, err := fsys.Create(ctx, fsys.RootKey(), "f", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := newFileHandle(fsys.NewOpenFile(key, etag, nil))
	defer h.open.Close()
	if _, errno := h.Write(ctx, []byte("kept"), 0); errno != 0 {
		t.Fatalf("write: %v", errno)
	}
	interrupted, cancel := context.WithCancel(ctx)
	cancel()
	if errno := h.Flush(interrupted); errno != 0 {
		t.Fatalf("flush with an interrupted context: %v", errno)
	}
	if data, _, _, err := fsys.ReadFile(ctx, key); err != nil || string(data) != "kept" {
		t.Fatalf("stored content = %q, %v; want %q", data, err, "kept")
	}
	if got := errnoFromErr(context.Canceled); got != syscall.EINTR {
		t.Fatalf("an interrupted blocking wait maps to %v, want EINTR", got)
	}
}

// TestMountDirectoryAsRoot: a drive on a directory mounts that directory
// as the mount's root — only what's under it is visible — and a delete
// through the mount records the item's path in the filesystem, not just
// in the mount, so the trash knows where it was.
func TestMountDirectoryAsRoot(t *testing.T) {
	ctx := context.Background()
	fsys := icbfs.New(newMountTestStore(t), "test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
		t.Fatal(err)
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	team, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "team", 0755, uid, gid)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fsys.Create(ctx, fsys.RootKey(), "outside", 0644, uid, gid); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fsys.Create(ctx, team, "inside", 0644, uid, gid); err != nil {
		t.Fatal(err)
	}

	mnt := t.TempDir()
	server, err := Mount(mnt, Root(fsys, nil, WithRoot(team, "/team")), &fs.Options{
		MountOptions:    fuse.MountOptions{FsName: "icbfs-test", Name: "icbfs-test", Options: []string{"default_permissions"}},
		NullPermissions: true,
	})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() { _ = server.Unmount() })

	entries, err := os.ReadDir(mnt)
	if err != nil || len(entries) != 1 || entries[0].Name() != "inside" {
		t.Fatalf("the mount's root lists %v, %v; want just inside", entries, err)
	}
	if err := os.Remove(filepath.Join(mnt, "inside")); err != nil {
		t.Fatal(err)
	}
	trash, err := fsys.ListTrash(ctx, icbfs.TrashOptions{})
	if err != nil || len(trash) != 1 || trash[0].Path != "/team/inside" {
		t.Fatalf("trash = %+v, %v; want /team/inside", trash, err)
	}
}

// TestMountFtruncateDownAndUpZeroes: ftruncate on an open descriptor
// drops the tail for that descriptor too, so growing it back reads zeros,
// before and after the close (xfstests' generic/029).
func TestMountFtruncateDownAndUpZeroes(t *testing.T) {
	mnt := mountTestFS(t)
	path := filepath.Join(mnt, "trunc.bin")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(slices.Repeat([]byte{'X'}, 5120), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2048); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(5120); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5120)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	for i := 2048; i < 5120; i++ {
		if buf[i] != 0 {
			t.Fatalf("byte %d = %q after truncate down and up", i, buf[i])
		}
	}
	f.Close()
	data, _ := os.ReadFile(path)
	for i := 2048; i < 5120; i++ {
		if data[i] != 0 {
			t.Fatalf("after close: byte %d = %q", i, data[i])
		}
	}
}

// TestMountOpenItemReplacedHasNoLinks: an open file or directory that a
// rename replaces, or that is removed, still answers fstat, with no links
// (xfstests' generic/035).
func TestMountOpenItemReplacedHasNoLinks(t *testing.T) {
	mnt := mountTestFS(t)
	for _, c := range []struct {
		name string
		make func(string) error
		gone func(a, b string) error
	}{
		{"file renamed over", func(p string) error { return os.WriteFile(p, nil, 0644) }, os.Rename},
		{"file removed", func(p string) error { return os.WriteFile(p, nil, 0644) }, func(_, b string) error { return os.Remove(b) }},
		{"dir renamed over", func(p string) error { return os.Mkdir(p, 0755) }, syscall.Rename}, // os.Rename refuses to replace a directory
		{"dir removed", func(p string) error { return os.Mkdir(p, 0755) }, func(_, b string) error { return os.Remove(b) }},
	} {
		dir := filepath.Join(mnt, strings.ReplaceAll(c.name, " ", "-"))
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
		if err := c.make(a); err != nil {
			t.Fatal(err)
		}
		if err := c.make(b); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.gone(a, b); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
			t.Errorf("%s: fstat: %v", c.name, err)
		} else if st.Nlink != 0 {
			t.Errorf("%s: nlink %d, want 0", c.name, st.Nlink)
		}
		f.Close()
	}
}

// TestMountFallocate: fallocate grows a file, punches holes and zeroes
// ranges through the mount, and the kernel refuses the modes FUSE doesn't
// pass on.
func TestMountFallocate(t *testing.T) {
	mnt := mountTestFS(t)
	path := filepath.Join(mnt, "falloc.bin")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(slices.Repeat([]byte{'x'}, 8192)); err != nil {
		t.Fatal(err)
	}
	fd := int(f.Fd())
	if err := syscall.Fallocate(fd, 0, 8192, 4096); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if st, _ := f.Stat(); st.Size() != 12288 {
		t.Fatalf("size after allocating %d, want 12288", st.Size())
	}
	const keepSize, punchHole = 0x01, 0x02
	if err := syscall.Fallocate(fd, keepSize|punchHole, 1000, 2000); err != nil {
		t.Fatalf("punch hole: %v", err)
	}
	if err := syscall.Fallocate(fd, 0x08, 0, 4096); err != syscall.EOPNOTSUPP { // collapse range
		t.Fatalf("collapse range = %v, want EOPNOTSUPP", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(slices.Repeat([]byte{'x'}, 1000), make([]byte, 2000), slices.Repeat([]byte{'x'}, 8192-3000), make([]byte, 4096))
	if !bytes.Equal(got, want) {
		t.Fatalf("content differs: %d bytes, want %d", len(got), len(want))
	}
}

// TestMountCloseToOpenAcrossMounts: a file another mount changed reads
// as changed once it's opened again, though this mount had its pages
// cached (see Mount).
func TestMountCloseToOpenAcrossMounts(t *testing.T) {
	store := newMountTestStore(t)
	mntA := mountFS(t, store, "shared")
	fsysB := icbfs.New(store, "shared")
	if err := fsysB.Attach(context.Background()); err != nil {
		t.Fatal(err)
	}
	mntB := t.TempDir()
	server, err := Mount(mntB, Root(fsysB, nil), &fs.Options{
		MountOptions:    fuse.MountOptions{FsName: "icbfs-test", Name: "icbfs-test", Options: []string{"default_permissions"}},
		NullPermissions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Unmount() })

	if err := os.WriteFile(filepath.Join(mntA, "f"), []byte("version 1"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(mntB, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(f); string(got) != "version 1" {
		t.Fatalf("B read %q", got)
	}
	if err := os.WriteFile(filepath.Join(mntA, "f"), []byte("version 2"), 0644); err != nil {
		t.Fatal(err)
	}
	// B still has f open, with its pages cached; a new open must not use them.
	got, err := os.ReadFile(filepath.Join(mntB, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version 2" {
		t.Fatalf("B reopened and read %q, want version 2", got)
	}
	f.Close()
}

// TestMountFsync: fsync stores a file's writes before the close, so
// another mount sees them, and fsync on a directory succeeds.
func TestMountFsync(t *testing.T) {
	store := newMountTestStore(t)
	mnt := mountFS(t, store, "fs")
	f, err := os.Create(filepath.Join(mnt, "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("synced")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("fsync: %v", err)
	}
	other := icbfs.New(store, "fs")
	ctx := context.Background()
	if err := other.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	entry, _, err := other.Lookup(ctx, other.RootKey(), "f")
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := other.ReadFile(ctx, entry.UUID); err != nil || string(got) != "synced" {
		t.Fatalf("another client read %q (%v) before the close, want synced", got, err)
	}
	d, err := os.Open(mnt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		t.Fatalf("fsync on a directory: %v", err)
	}
}

// TestMountMmapWritesAfterCloseKept: writes through a mapping after its
// file was closed reach the store: the kernel writes the pages back after
// the close, before it releases the handle.
func TestMountMmapWritesAfterCloseKept(t *testing.T) {
	mnt := mountTestFS(t)
	path := filepath.Join(mnt, "mapped.bin")
	if err := os.WriteFile(path, make([]byte, 8192), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := syscall.Mmap(int(f.Fd()), 0, 8192, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	copy(m[4096:], "written after close")
	if err := syscall.Munmap(m); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := os.ReadFile(path)
		if err == nil && bytes.HasPrefix(got[4096:], []byte("written after close")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mapping's write was lost: %q, %v", got[4096:4120], err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
