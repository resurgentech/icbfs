package fuseserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/resurgentech/icbfs/internal/objstore"
)

// newMountTestStore spins up a real MinIO container and a versioned
// bucket, returning a Store backed by it with no filesystem bootstrapped
// yet — mountTestFS's single-filesystem callers go through mountTestFS
// itself; callers that need to act on the store before mounting (e.g.
// archiving a filesystem first, to prove mount-time enforcement) use
// this directly.
func newMountTestStore(t *testing.T) objstore.Store {
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
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		t.Fatalf("enable bucket versioning: %v", err)
	}

	return objstore.NewS3Store(client, bucket)
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
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
		t.Fatalf("bootstrap filesystem: %v", err)
	}
	fsys.EnableLocking(locking)

	mountDir := t.TempDir()
	server, err := fs.Mount(mountDir, Root(fsys), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:  "icbfs-test",
			Name:    "icbfs-test",
			Options: []string{"default_permissions"}, // task C1
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
// (ROADMAP.md tasks B2-B8) turned on.
func mountTestFSWithLocking(t *testing.T) string {
	t.Helper()
	return mountFSWithLocking(t, newMountTestStore(t), "test", true)
}

// mountTestFSWithFsys is mountTestFS but also returns the underlying
// *icbfs.Filesystem — for tests (task C2) that need to create an
// object directly through the Filesystem API with an owner uid/gid
// that doesn't match the test process's own, something no real
// application could do through the mounted path itself but is exactly
// what's needed to exercise "other"/group-bit enforcement without
// real privilege escalation (setuid/running as a second real user).
func mountTestFSWithFsys(t *testing.T) (string, *icbfs.Filesystem) {
	t.Helper()
	ctx := context.Background()
	store := newMountTestStore(t)
	fsys := icbfs.New(store, "test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
		t.Fatalf("bootstrap filesystem: %v", err)
	}

	mountDir := t.TempDir()
	server, err := fs.Mount(mountDir, Root(fsys), &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName:  "icbfs-test",
			Name:    "icbfs-test",
			Options: []string{"default_permissions"}, // task C1
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

// TestMountChmodToZeroIsReportedAccurately is a regression test for a real
// bug found by testing, not foreseen: go-fuse silently rewrites a
// genuinely-stored "0000" mode back to 0644/0755 on every Getattr unless
// fs.Options.NullPermissions is set.
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

// TestMountDefaultPermissionsEnforcesOwnerModeBits covers ROADMAP.md's
// task C1 "Done when": a chmod 000 file cannot be read or written even
// by its own owner, and a chmod 444 file can be read but a write
// attempt fails with a permission error — real kernel-level
// enforcement via default_permissions, not just mode bits being
// correctly stored/reported (TestMountChmodToZeroIsReportedAccurately
// already covers the reporting side; this covers actual enforcement).
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

// TestMountDefaultPermissionsEnforcesOtherBits is ROADMAP.md's task
// C2's decision, made and exercised rather than silently skipped: a
// genuine differing-real-uid test would need actual privilege
// escalation (running part of the test as a second real user, which
// needs root or an equivalent privilege to setuid) — not available to
// this automated suite, and not worth adding just for this. Instead,
// this test creates a file directly through the Filesystem API
// (fsys.Create) with an owner uid/gid that is deliberately *not* this
// test process's own — something no real application could do through
// the mounted path itself (a real create(2) always records the real
// calling uid), but the kernel's default_permissions enforcement
// doesn't care how an inode's reported uid/gid/mode got that way, only
// what Getattr reports at access time. So accessing that file through
// the real, kernel-enforced mount still genuinely exercises the
// "other" bits code path (this test process is neither the owner nor
// a member of the fabricated group), not just the owner-bits path
// task C1's test already covers.
//
// What this does NOT cover: the "group" bits specifically (vs.
// "other"), and the ROADMAP-called-out nuance of the kernel checking a
// caller's *full* supplementary group list, not just a primary gid —
// confirming those still needs a genuine second real identity. Covered
// here only to the extent that "other" bits are real kernel
// enforcement, not something this codebase has to implement itself.
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

// TestMountStatfsReflectsDeclaredSizeAndUsage covers D4's FUSE wiring
// end to end through a real kernel mount (mountTestFS bootstraps with
// a 1<<30 declared size — see newTestFilesystem's Bootstrap call): df
// ("Total") should reflect the declared size, and writing a file should
// move "Used" (via statfsBlockSize-rounded free space shrinking)
// without the mount reporting bogus/zeroed values, which is what a
// mis-wired NodeStatfser would actually look like (per that interface's
// own doc comment: "If not defined, the `out` argument will [be]
// zeroed with an OK result").
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

// TestMountArchivedFilesystemRejectsWritesButAllowsReads is D5's FUSE-
// level counterpart to internal/icbfs's TestArchivedFilesystemRejects-
// WritesButAllowsReads: proves errnoFromErr's ErrArchived->EROFS mapping
// (added in node.go) is actually reachable through a real kernel mount,
// and exercises the documented mount-time (not continuous) enforcement
// point — the filesystem is archived, then mounted fresh, so this
// mount's Bootstrap is the one that observes archived=true.
func TestMountArchivedFilesystemRejectsWritesButAllowsReads(t *testing.T) {
	store := newMountTestStore(t)
	ctx := context.Background()

	fsys := icbfs.New(store, "archived-mount-test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
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

// TestMountFlockWholeFileExclusive covers ROADMAP.md's task B8 "Done
// when" for flock(2): real kernel flock syscalls against a real mount,
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


// TestMain intercepts the test binary's own invocation when re-exec'd
// as a cross-process fcntl lock holder (see runFcntlLockHolderHelper)
// — a real, separate OS process is required to exercise fcntl(2)'s
// actual ownership semantics: fcntl byte-range locks are owned by
// (process, inode), not by file descriptor, so two fds opened by the
// *same* process (confirmed by testing directly: the naive two-fd,
// one-process version of this test always "succeeded" without
// conflict, because the kernel reports the same lock owner for both,
// and this codebase's holder model correctly treats a holder's own
// re-acquisition as a non-conflicting replace, exactly like real
// fcntl semantics do) never actually conflict with each other,
// regardless of what this driver does.
func TestMain(m *testing.M) {
	if os.Getenv("ICBFS_FCNTL_HELPER") == "1" {
		runFcntlLockHolderHelper()
		return
	}
	os.Exit(m.Run())
}

// runFcntlLockHolderHelper opens ICBFS_FCNTL_HELPER_PATH (a path under
// an already-mounted icbfs FUSE mount, inherited from the parent
// process — no testcontainers/mount setup of its own needed, since it
// operates on the mount the same way any other real process accessing
// an already-mounted filesystem would), takes an F_SETLK on [0,10),
// signals readiness by creating ICBFS_FCNTL_HELPER_READY, then waits
// for ICBFS_FCNTL_HELPER_RELEASE to appear before exiting (releasing
// the lock as a side effect of process exit, same as a real
// application would).
func runFcntlLockHolderHelper() {
	path := os.Getenv("ICBFS_FCNTL_HELPER_PATH")
	readyPath := os.Getenv("ICBFS_FCNTL_HELPER_READY")
	releasePath := os.Getenv("ICBFS_FCNTL_HELPER_RELEASE")

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open:", err)
		os.Exit(1)
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 10}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lk); err != nil {
		fmt.Fprintln(os.Stderr, "helper: F_SETLK [0,10):", err)
		os.Exit(1)
	}
	if err := os.WriteFile(readyPath, []byte("ready"), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "helper: signal ready:", err)
		os.Exit(1)
	}

	deadline := time.Now().Add(10 * time.Second)
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

// TestMountFcntlByteRangeLocksAcrossProcesses covers ROADMAP.md's task
// B8 "Done when" for fcntl(2) byte-range locks through real kernel
// syscalls against a real mount, using a genuine second OS process
// (see TestMain/runFcntlLockHolderHelper's doc comments on why that's
// required, not optional, for fcntl specifically): an overlapping
// F_SETLK from the other process is refused while held, a genuinely
// non-overlapping range succeeds while that lock is still held, a
// conflicting F_GETLK reports it back rather than L_UNLCK, and the
// overlapping range succeeds once the holder process exits (releasing
// it).
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

	if err := syscall.FcntlFlock(f2.Fd(), syscall.F_SETLK, &overlapping); err != nil {
		t.Fatalf("F_SETLK f2 [5,15) after helper process exited (and released) = %v, want nil", err)
	}
}
