package fuseserver

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	ctx := context.Background()

	fsys := icbfs.New(store, fsName)
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0); err != nil {
		t.Fatalf("bootstrap filesystem: %v", err)
	}

	mountDir := t.TempDir()
	server, err := fs.Mount(mountDir, Root(fsys), &fs.Options{
		MountOptions:    fuse.MountOptions{FsName: "icbfs-test", Name: "icbfs-test"},
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
// fs.Options.NullPermissions is set. See MISSING_FEATURES.md ("Permission
// enforcement").
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
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0); err != nil {
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
