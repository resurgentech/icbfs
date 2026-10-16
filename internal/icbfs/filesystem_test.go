package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go/modules/minio"
	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
	"github.com/resurgentech/icbfs/internal/testutil"
)

// newTestStore starts a MinIO container with a versioned bucket and returns
// a Store on it with no filesystem, for tests that need several filesystems
// in one bucket. Requires Docker.
func newTestStore(t *testing.T) objstore.Store {
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

	const bucket = "icbfs-core-test"
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

	return objstore.NewS3Store(client, bucket)
}

// newTestFilesystem spins up a real MinIO container and a versioned
// bucket, and returns a Filesystem backed by it. Requires Docker.
func newTestFilesystem(t *testing.T) (*Filesystem, objstore.Store) {
	t.Helper()
	store := newTestStore(t)
	fsys := New(store, "test")
	if err := fsys.Bootstrap(context.Background(), 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return fsys, store
}

// TestDirectorySplitsAndStaysConsistent creates enough entries to split
// several leaves (testDirBlockEntries is 8), and checks that the directory's
// top block becomes an internal node, that every entry is found by Lookup
// and by ReadDir, and that removal works across both subtrees of a
// top-level split.
func TestDirectorySplitsAndStaysConsistent(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	const n = 40 // several multiples of testDirBlockEntries (8)
	names := make([]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("file-%03d", i)
		names[i] = name
		if _, _, _, err := fsys.Create(ctx, root, name, 0644, 0, 0); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	// The root must actually have become an Internal node — otherwise
	// this test isn't exercising sharding at all.
	body, _, err := store.Get(ctx, root)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	rootBlock, err := block.Decode(data)
	if err != nil {
		t.Fatalf("decode root: %v", err)
	}
	if rootBlock.Kind != block.Internal {
		t.Fatalf("root is still a flat Leaf after creating %d entries (testDirBlockEntries=%d) — sharding did not kick in", n, testDirBlockEntries)
	}
	if len(rootBlock.Children) < 2 {
		t.Fatalf("root has %d children, want at least 2", len(rootBlock.Children))
	}

	// Every entry must be findable via Lookup (point descent)...
	for _, name := range names {
		if _, _, err := fsys.Lookup(ctx, root, name); err != nil {
			t.Fatalf("lookup %q: %v", name, err)
		}
	}

	// ...and ReadDir's full traversal must return the complete,
	// duplicate-free set.
	entries, err := fsys.ReadDir(ctx, root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if seen[e.Name] {
			t.Fatalf("duplicate entry %q in readdir", e.Name)
		}
		seen[e.Name] = true
	}
	if len(seen) != n {
		t.Fatalf("readdir returned %d distinct entries, want %d", len(seen), n)
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("readdir missing %q", name)
		}
	}

	// Remove every other entry (spread across whatever shards they
	// landed in) and confirm the survivors are all still correct.
	for i := 0; i < n; i += 2 {
		if err := fsys.Unlink(ctx, root, names[i]); err != nil {
			t.Fatalf("unlink %q: %v", names[i], err)
		}
	}
	entries, err = fsys.ReadDir(ctx, root)
	if err != nil {
		t.Fatalf("readdir after removals: %v", err)
	}
	if len(entries) != n/2 {
		t.Fatalf("got %d entries after removing half, want %d", len(entries), n/2)
	}
	for i := 1; i < n; i += 2 {
		if _, _, err := fsys.Lookup(ctx, root, names[i]); err != nil {
			t.Fatalf("surviving entry %q: %v", names[i], err)
		}
	}
	for i := 0; i < n; i += 2 {
		if _, _, err := fsys.Lookup(ctx, root, names[i]); err == nil {
			t.Fatalf("removed entry %q should no longer be found", names[i])
		}
	}
}

// TestConcurrentLinkRaceDoesNotLoseUpdates: N concurrent Links to one
// target all count. A plain read-increment-write of the link count would
// lose some; .metadata's compare-and-swap (adjustNlink) mustn't.
func TestConcurrentLinkRaceDoesNotLoseUpdates(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	targetUUID, _, _, err := fsys.Create(ctx, root, "original", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 15
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := fsys.Link(ctx, root, fmt.Sprintf("link-%02d", i), targetUUID, TypeFile)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent link %d: %v", i, err)
		}
	}

	got := mustReadNlinkSideObject(t, ctx, store, targetUUID)
	want := n + 1 // the original create starts the count at 1
	if got != want {
		t.Fatalf("authoritative nlink side object = %d, want %d (lost at least one concurrent Link)", got, want)
	}

	// Now unlink every one of those names concurrently and confirm the
	// count comes back down correctly too, without the original being
	// deleted along the way.
	errs = make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fsys.Unlink(ctx, root, fmt.Sprintf("link-%02d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent unlink %d: %v", i, err)
		}
	}

	got = mustReadNlinkSideObject(t, ctx, store, targetUUID)
	if got != 1 {
		t.Fatalf("authoritative nlink side object = %d after unlinking all links, want 1", got)
	}
	if _, _, _, err := fsys.ReadFile(ctx, targetUUID); err != nil {
		t.Fatalf("original file should still exist: %v", err)
	}
	checkClean(t, fsys)
}

func mustReadNlinkSideObject(t *testing.T, ctx context.Context, store objstore.Store, uuid string) int {
	t.Helper()
	body, _, err := store.Get(ctx, metadataObjectKey(uuid))
	if err != nil {
		t.Fatalf("get .metadata object: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read .metadata object: %v", err)
	}
	attr, err := decodeFileMetadata(data)
	if err != nil {
		t.Fatalf("decode .metadata object: %v", err)
	}
	return int(attr.Nlink)
}

// TestXattrsRoundTripArbitraryBinaryValues: arbitrary binary xattr values
// round-trip through the Protobuf .metadata object unchanged, written and
// read with the same encode and decode functions Create, Stat and SetAttr
// use.
func TestXattrsRoundTripArbitraryBinaryValues(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "has-xattrs.txt", 0644, 1000, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Not valid UTF-8, with zero and high bytes.
	binaryValue := []byte{0x00, 0xFF, 0x01, 0xFE, 'h', 'i', 0x00, 0x80, 0x7F}

	mk := metadataObjectKey(uuid)
	body, obj, err := store.Get(ctx, mk)
	if err != nil {
		t.Fatalf("get .metadata: %v", err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatalf("read .metadata: %v", err)
	}
	attr, err := decodeFileMetadata(data)
	if err != nil {
		t.Fatalf("decode .metadata: %v", err)
	}
	attr.Xattrs = map[string][]byte{"user.binary-blob": binaryValue}
	newData, err := encodeFileMetadata(attr)
	if err != nil {
		t.Fatalf("encode .metadata: %v", err)
	}
	if _, err := store.Put(ctx, mk, bytes.NewReader(newData), nil, obj.ETag); err != nil {
		t.Fatalf("put .metadata: %v", err)
	}

	_, got, _, err := fsys.ReadFile(ctx, uuid)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	roundTripped, ok := got.Xattrs["user.binary-blob"]
	if !ok {
		t.Fatal("xattr missing after round trip")
	}
	if !bytes.Equal(roundTripped, binaryValue) {
		t.Fatalf("xattr corrupted: got %v, want %v", roundTripped, binaryValue)
	}
	// Confirm mode/uid/gid/nlink weren't disturbed by writing xattrs —
	// the map lives alongside them in the same message, not atop them.
	if got.Mode != 0644 || got.Uid != 1000 || got.Gid != 1000 || got.Nlink != 1 {
		t.Fatalf("other fields disturbed by xattrs: %+v", got)
	}
}

// TestInternalStateIsNotXattrs: an item's Windows state, POSIX ACLs and
// tools' data are kept apart from its xattrs, so Xattrs holds only what a
// user set.
func TestInternalStateIsNotXattrs(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	uuid, _, _, err := fsys.Create(ctx, fsys.RootKey(), "kept-apart.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := fsys.SetXattr(ctx, uuid, TypeFile, "user.tag", []byte("mine"), XattrAny); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.SetWindowsCreateMetadata(ctx, uuid, TypeFile, WindowsAttrHidden, []byte("O:BAG:BAD:(A;;FA;;;WD)")); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.SetAppData(ctx, uuid, TypeFile, AppSyncHash, []byte("abc123")); err != nil {
		t.Fatal(err)
	}
	got, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Xattrs) != 1 || string(got.Xattrs["user.tag"]) != "mine" {
		t.Fatalf("xattrs = %v, want only user.tag", got.Xattrs)
	}
	if sddl, ok := got.WindowsACL(); !ok || string(sddl) != "O:BAG:BAD:(A;;FA;;;WD)" {
		t.Fatalf("Windows ACL = %q, %v", sddl, ok)
	}
	if bits, ok := got.WindowsAttributes(); !ok || bits != WindowsAttrHidden {
		t.Fatalf("Windows attributes = %#x, %v", bits, ok)
	}
	if string(got.App[AppSyncHash]) != "abc123" {
		t.Fatalf("app data = %v", got.App)
	}
}

// TestLegacyXattrLayoutIsSplitOnRead: a .metadata record written before
// the split (layout 0) kept icbfs's state in xattrs under fixed names;
// reading it puts each in its own field, leaving only users' xattrs, and
// the next write stores the new layout.
func TestLegacyXattrLayoutIsSplitOnRead(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	uuid, _, _, err := fsys.Create(ctx, fsys.RootKey(), "legacy.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	legacy, err := proto.Marshal(&pb.Metadata{Mode: 0644, Nlink: 1, Xattrs: map[string][]byte{
		"user.tag":                []byte("mine"),
		"windows.acl":             []byte("O:BAG:BA"),
		"windows.attributes":      {byte(WindowsAttrReadOnly)},
		"windows.reparse":         []byte("\x01\x02\x03\x04"),
		"system.posix_acl_access": []byte("acl"),
		"user.sync.sha256":        []byte("abc123"),
		"user.drive.contentType":  []byte("text/plain"),
		"user.side.base":          []byte("v1"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, metadataObjectKey(uuid), bytes.NewReader(legacy), nil, ""); err != nil {
		t.Fatal(err)
	}

	check := func(what string) {
		t.Helper()
		got, err := fsys.Stat(ctx, uuid, TypeFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Xattrs) != 1 || string(got.Xattrs["user.tag"]) != "mine" {
			t.Fatalf("%s: xattrs = %v, want only user.tag", what, got.Xattrs)
		}
		if sddl, _ := got.WindowsACL(); string(sddl) != "O:BAG:BA" {
			t.Fatalf("%s: Windows ACL = %q", what, sddl)
		}
		if bits, ok := got.WindowsAttributes(); !ok || bits != WindowsAttrReadOnly {
			t.Fatalf("%s: Windows attributes = %#x, %v", what, bits, ok)
		}
		if buf, _ := got.GenericReparse(); len(buf) != 4 {
			t.Fatalf("%s: reparse = %v", what, buf)
		}
		if string(got.PosixACLAccess) != "acl" {
			t.Fatalf("%s: POSIX ACL = %q", what, got.PosixACLAccess)
		}
		if string(got.App[AppSyncHash]) != "abc123" || string(got.App[AppDriveContentType]) != "text/plain" || string(got.App[AppSidePrefix+"base"]) != "v1" {
			t.Fatalf("%s: app data = %v", what, got.App)
		}
	}
	check("read")
	// A write stores the new layout; reading it gives the same.
	if _, err := fsys.SetXattr(ctx, uuid, TypeFile, "user.more", []byte("x"), XattrAny); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.RemoveXattr(ctx, uuid, TypeFile, "user.more"); err != nil {
		t.Fatal(err)
	}
	check("after a write")
	body, _, err := store.Get(ctx, metadataObjectKey(uuid))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	var m pb.Metadata
	if err := proto.Unmarshal(data, &m); err != nil || m.Layout != metadataLayout {
		t.Fatalf("stored layout = %d, %v", m.Layout, err)
	}
}

// TestSetAttrRejectsNegativeSizeInsteadOfCrashing: a negative size is
// EINVAL, as truncate(2) says, not a crash (pjdfstest's truncate tests pass
// one).
func TestSetAttrRejectsNegativeSizeInsteadOfCrashing(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "truncate-target.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := fsys.WriteFile(ctx, uuid, []byte("hello"), ""); err != nil {
		t.Fatalf("write: %v", err)
	}

	negative := int64(-1)
	if _, err := fsys.SetAttr(ctx, uuid, TypeFile, nil, nil, nil, &negative, nil, nil, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("SetAttr with negative size = %v, want ErrInvalidArgument", err)
	}

	// Content must be untouched by the rejected call.
	data, _, _, err := fsys.ReadFile(ctx, uuid)
	if err != nil {
		t.Fatalf("read after rejected SetAttr: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("content disturbed by rejected SetAttr: got %q, want %q", data, "hello")
	}
}

// TestSetAttrRejectsHugeSizeInsteadOfCrashing: pjdfstest's truncate/12.t
// and ftruncate/12.t truncate to 999999999999999 bytes and accept EFBIG or
// EINVAL; past the max file size, it's EFBIG.
func TestSetAttrRejectsHugeSizeInsteadOfCrashing(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "huge-truncate-target.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := fsys.WriteFile(ctx, uuid, []byte("hello"), ""); err != nil {
		t.Fatalf("write: %v", err)
	}

	huge := int64(999999999999999)
	if _, err := fsys.SetAttr(ctx, uuid, TypeFile, nil, nil, nil, &huge, nil, nil, nil); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("SetAttr with huge size = %v, want ErrFileTooLarge", err)
	}

	data, _, _, err := fsys.ReadFile(ctx, uuid)
	if err != nil {
		t.Fatalf("read after rejected SetAttr: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("content disturbed by rejected SetAttr: got %q, want %q", data, "hello")
	}
}

// TestMknodCreatesSpecialFileEntries: a FIFO, a Unix socket, and character
// and block devices can be created, ReadDir reports their types, and
// devices keep their device numbers. The kernel serves their I/O; the type
// and metadata are all this filesystem handles.
func TestMknodCreatesSpecialFileEntries(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	cases := []struct {
		name string
		typ  EntryType
		rdev uint32
	}{
		{"a-fifo", TypeFifo, 0},
		{"a-socket", TypeSocket, 0},
		{"a-char-device", TypeCharDevice, 0x0105}, // major 1, minor 5 — arbitrary, just non-zero and distinguishable
		{"a-block-device", TypeBlockDevice, 0x0800},
	}
	for _, c := range cases {
		uuid, attr, err := fsys.Mknod(ctx, root, c.name, 0644, c.typ, c.rdev, 1000, 1000)
		if err != nil {
			t.Fatalf("Mknod %s: %v", c.name, err)
		}
		if attr.Rdev != c.rdev {
			t.Fatalf("%s: Mknod's returned Rdev = %#x, want %#x", c.name, attr.Rdev, c.rdev)
		}
		stated, err := fsys.Stat(ctx, uuid, c.typ)
		if err != nil {
			t.Fatalf("Stat %s: %v", c.name, err)
		}
		if stated.Rdev != c.rdev {
			t.Fatalf("%s: Stat's Rdev = %#x, want %#x", c.name, stated.Rdev, c.rdev)
		}
		if stated.Uid != 1000 || stated.Mode != 0644 {
			t.Fatalf("%s: Stat = uid %d mode %o, want uid 1000 mode 0644", c.name, stated.Uid, stated.Mode)
		}
	}

	entries, err := fsys.ReadDir(ctx, root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	gotTypes := make(map[string]EntryType, len(entries))
	for _, e := range entries {
		gotTypes[e.Name] = e.Type
	}
	for _, c := range cases {
		if gotTypes[c.name] != c.typ {
			t.Fatalf("ReadDir reported %s as type %v, want %v", c.name, gotTypes[c.name], c.typ)
		}
	}
}

// TestTouchAtimeBumpsFileAtime: a new file has no access time, and one
// TouchAtime (what a read triggers) records a time after its creation.
func TestTouchAtimeBumpsFileAtime(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "atime-target.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	before, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat before touch: %v", err)
	}
	if !before.Atime.IsZero() {
		t.Fatalf("freshly created file's Atime = %v, want zero (never accessed)", before.Atime)
	}

	touchedAt := time.Now()
	if err := fsys.TouchAtime(ctx, uuid, TypeFile); err != nil {
		t.Fatalf("TouchAtime: %v", err)
	}

	after, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat after touch: %v", err)
	}
	if after.Atime.Before(touchedAt.Add(-time.Second)) {
		t.Fatalf("Atime after TouchAtime = %v, want at/after %v", after.Atime, touchedAt)
	}
}

// TestTouchAtimeBumpsDirAtime is the same for a directory, whose .metadata
// is written through a different path (updateDirMetadata).
func TestTouchAtimeBumpsDirAtime(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	dirUUID, _, err := fsys.Mkdir(ctx, root, "atime-dir", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	before, err := fsys.Stat(ctx, dirUUID, TypeDir)
	if err != nil {
		t.Fatalf("stat before touch: %v", err)
	}
	if !before.Atime.IsZero() {
		t.Fatalf("freshly created dir's Atime = %v, want zero (never accessed)", before.Atime)
	}

	touchedAt := time.Now()
	if err := fsys.TouchAtime(ctx, dirUUID, TypeDir); err != nil {
		t.Fatalf("TouchAtime: %v", err)
	}

	after, err := fsys.Stat(ctx, dirUUID, TypeDir)
	if err != nil {
		t.Fatalf("stat after touch: %v", err)
	}
	if after.Atime.Before(touchedAt.Add(-time.Second)) {
		t.Fatalf("Atime after TouchAtime = %v, want at/after %v", after.Atime, touchedAt)
	}
	// TouchAtime must not disturb mode/uid/gid — it's a full
	// native-metadata rewrite (MetadataDirectiveReplace), so a bug
	// here would silently wipe them, not just fail loudly.
	if after.Mode != 0755 {
		t.Fatalf("mode disturbed by TouchAtime: got %o, want %o", after.Mode, 0755)
	}
}

// TestSetAttrSetsExplicitAtime: utimensat(2)'s explicit atime, a chosen
// time rather than now, round-trips for a file and for a directory, each
// through its own SetAttr branch.
func TestSetAttrSetsExplicitAtime(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()
	explicit := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	fileUUID, _, _, err := fsys.Create(ctx, root, "explicit-atime.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := fsys.SetAttr(ctx, fileUUID, TypeFile, nil, nil, nil, nil, &explicit, nil, nil); err != nil {
		t.Fatalf("SetAttr explicit atime (file): %v", err)
	}
	fileAttr, err := fsys.Stat(ctx, fileUUID, TypeFile)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if !fileAttr.Atime.Equal(explicit) {
		t.Fatalf("file Atime = %v, want %v", fileAttr.Atime, explicit)
	}

	dirUUID, _, err := fsys.Mkdir(ctx, root, "explicit-atime-dir", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := fsys.SetAttr(ctx, dirUUID, TypeDir, nil, nil, nil, nil, &explicit, nil, nil); err != nil {
		t.Fatalf("SetAttr explicit atime (dir): %v", err)
	}
	dirAttr, err := fsys.Stat(ctx, dirUUID, TypeDir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if !dirAttr.Atime.Equal(explicit) {
		t.Fatalf("dir Atime = %v, want %v", dirAttr.Atime, explicit)
	}
}

// TestSetAttrExplicitMtimeBtimeOverrides covers the nullable mtime/btime
// overrides: unset, mtime follows content and is never earlier than the
// creation time; set (even to a date long before creation, as Windows'
// SetFileTime allows), both round-trip exactly and survive an unrelated
// metadata change.
func TestSetAttrExplicitMtimeBtimeOverrides(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "override.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.Btime.Equal(Btime(uuid)) {
		t.Fatalf("Btime without override = %v, want key time %v", before.Btime, Btime(uuid))
	}
	if before.Mtime.Before(before.Btime) {
		t.Fatalf("Mtime %v earlier than Btime %v", before.Mtime, before.Btime)
	}

	mtime := time.Date(1970, 1, 1, 7, 52, 26, 0, time.UTC)
	btime := time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)
	if _, err := fsys.SetAttr(ctx, uuid, TypeFile, nil, nil, nil, nil, nil, &mtime, &btime); err != nil {
		t.Fatalf("SetAttr overrides: %v", err)
	}
	mode := uint32(0600)
	if _, err := fsys.SetAttr(ctx, uuid, TypeFile, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("SetAttr mode: %v", err)
	}
	after, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.Mtime.Equal(mtime) {
		t.Fatalf("Mtime = %v, want override %v", after.Mtime, mtime)
	}
	if !after.Btime.Equal(btime) {
		t.Fatalf("Btime = %v, want override %v", after.Btime, btime)
	}
	if after.Mode != 0600 {
		t.Fatalf("mode = %o, want 600", after.Mode)
	}

	// A real write afterwards: mtime follows content again (the override
	// was tied to the old content version); the btime override stays.
	if _, _, err := fsys.WriteFile(ctx, uuid, []byte("new content"), ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	written, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if written.Mtime.Equal(mtime) || written.Mtime.Before(Btime(uuid)) {
		t.Fatalf("Mtime after write = %v, want content time (not override %v)", written.Mtime, mtime)
	}
	if !written.Btime.Equal(btime) {
		t.Fatalf("Btime after write = %v, want override %v", written.Btime, btime)
	}
}

// TestSetWindowsAttributesRoundTripsAndMasksUnrelatedBits:
// SetWindowsAttributes stores the four attribute bits, drops others a
// caller passes (FILE_ATTRIBUTE_NORMAL, FILE_ATTRIBUTE_DIRECTORY), and
// leaves mode, owner and link count alone.
func TestSetWindowsAttributesRoundTripsAndMasksUnrelatedBits(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "has-win-attrs.txt", 0644, 1000, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Hidden|Archive, plus FILE_ATTRIBUTE_NORMAL (0x80) and
	// FILE_ATTRIBUTE_DIRECTORY (0x10) thrown in to confirm masking —
	// neither belongs in the stored byte at all.
	const (
		fileAttributeDirectory = 0x10
		fileAttributeNormal    = 0x80
	)
	in := WindowsAttrHidden | WindowsAttrArchive | fileAttributeDirectory | fileAttributeNormal

	attr, err := fsys.SetWindowsAttributes(ctx, uuid, TypeFile, in)
	if err != nil {
		t.Fatalf("SetWindowsAttributes: %v", err)
	}
	want := WindowsAttrHidden | WindowsAttrArchive
	bits, ok := attr.WindowsAttributes()
	if !ok {
		t.Fatal("WindowsAttributes: ok = false immediately after SetWindowsAttributes")
	}
	if bits != want {
		t.Fatalf("stored bits = %#x, want %#x (Directory/Normal should be masked out)", bits, want)
	}
	// The raw stored byte, bypassing the reader's mask: masking happens at
	// write time.
	if attr.WindowsAttrs != want {
		t.Fatalf("stored bits = %#x, want %#x (Directory/Normal must not reach storage at all)", attr.WindowsAttrs, want)
	}

	// Re-read via Stat, not just the return value, to prove this is
	// really persisted, not just echoed back.
	got, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	bits, ok = got.WindowsAttributes()
	if !ok {
		t.Fatal("WindowsAttributes: ok = false after Stat")
	}
	if want := WindowsAttrHidden | WindowsAttrArchive; bits != want {
		t.Fatalf("stat-read bits = %#x, want %#x", bits, want)
	}
	if got.Mode != 0644 || got.Uid != 1000 || got.Gid != 1000 || got.Nlink != 1 {
		t.Fatalf("other fields disturbed by SetWindowsAttributes: %+v", got)
	}

	// An entry nothing has ever called SetWindowsAttributes on reports
	// ok = false, not a zero-value false positive.
	other, _, _, err := fsys.Create(ctx, root, "no-win-attrs.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	otherAttr, err := fsys.Stat(ctx, other, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, ok := otherAttr.WindowsAttributes(); ok {
		t.Fatalf("WindowsAttributes: ok = true for an entry never touched, attrs = %+v", otherAttr)
	}
}

// TestSetWindowsACLRoundTrips: SetWindowsACL stores an SDDL string as is
// (parsing it is winfspserver's business, tested on a real mount), and an
// item without one reports ok false.
func TestSetWindowsACLRoundTrips(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "has-win-acl.txt", 0644, 1000, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sddl := []byte("O:S-1-5-21-1-2-3-1000G:S-1-5-21-1-2-3-1001D:(A;;FA;;;S-1-5-21-1-2-3-1000)")
	attr, err := fsys.SetWindowsACL(ctx, uuid, TypeFile, sddl)
	if err != nil {
		t.Fatalf("SetWindowsACL: %v", err)
	}
	got, ok := attr.WindowsACL()
	if !ok {
		t.Fatal("WindowsACL: ok = false immediately after SetWindowsACL")
	}
	if !bytes.Equal(got, sddl) {
		t.Fatalf("stored SDDL = %q, want %q", got, sddl)
	}

	// Re-read via Stat, not just the return value, to prove this is
	// really persisted, not just echoed back.
	statAttr, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	got, ok = statAttr.WindowsACL()
	if !ok {
		t.Fatal("WindowsACL: ok = false after Stat")
	}
	if !bytes.Equal(got, sddl) {
		t.Fatalf("stat-read SDDL = %q, want %q", got, sddl)
	}
	if statAttr.Mode != 0644 || statAttr.Uid != 1000 || statAttr.Gid != 1000 || statAttr.Nlink != 1 {
		t.Fatalf("other fields disturbed by SetWindowsACL: %+v", statAttr)
	}

	other, _, _, err := fsys.Create(ctx, root, "no-win-acl.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	otherAttr, err := fsys.Stat(ctx, other, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, ok := otherAttr.WindowsACL(); ok {
		t.Fatalf("WindowsACL: ok = true for an entry never touched, attrs = %+v", otherAttr)
	}
}

// TestDirectoryMetadataObject covers directories' own .metadata: Mkdir
// creates it, attribute changes and Windows bits/ACLs round-trip through
// it, and Rmdir removes it.
func TestDirectoryMetadataObject(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	if _, err := store.Head(ctx, metadataObjectKey(root)); err != nil {
		t.Fatalf("root .metadata after Bootstrap: %v", err)
	}
	dir, _, err := fsys.Mkdir(ctx, root, "d", 0750, 7, 8)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := store.Head(ctx, metadataObjectKey(dir)); err != nil {
		t.Fatalf("dir .metadata after Mkdir: %v", err)
	}
	attr, err := fsys.Stat(ctx, dir, TypeDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if attr.Mode != 0750 || attr.Uid != 7 || attr.Gid != 8 || attr.Mtime.IsZero() || !attr.Btime.Equal(Btime(dir)) {
		t.Fatalf("dir attr = %+v", attr)
	}

	mode := uint32(0700)
	mtime := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := fsys.SetAttr(ctx, dir, TypeDir, &mode, nil, nil, nil, nil, &mtime, nil); err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	if _, err := fsys.SetWindowsAttributes(ctx, dir, TypeDir, WindowsAttrReadOnly); err != nil {
		t.Fatalf("SetWindowsAttributes: %v", err)
	}
	if _, err := fsys.SetWindowsACL(ctx, dir, TypeDir, []byte("D:P(A;;FA;;;WD)")); err != nil {
		t.Fatalf("SetWindowsACL: %v", err)
	}
	if attr, err = fsys.Stat(ctx, dir, TypeDir); err != nil || !attr.Mtime.Equal(mtime) {
		t.Fatalf("dir mtime after SetAttr = %v, %v; want %v", attr.Mtime, err, mtime)
	}
	// Entries changing must not disturb the directory's own attributes,
	// only move its mtime on, as on any POSIX filesystem or NTFS.
	if _, _, _, err := fsys.Create(ctx, dir, "child", 0644, 0, 0); err != nil {
		t.Fatalf("create child: %v", err)
	}
	attr, err = fsys.Stat(ctx, dir, TypeDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	bits, _ := attr.WindowsAttributes()
	acl, _ := attr.WindowsACL()
	if attr.Mode != 0700 || !attr.Mtime.After(mtime) || bits != WindowsAttrReadOnly || string(acl) != "D:P(A;;FA;;;WD)" {
		t.Fatalf("dir attr after updates = %+v", attr)
	}

	if err := fsys.Unlink(ctx, dir, "child"); err != nil {
		t.Fatalf("unlink child: %v", err)
	}
	if err := fsys.Rmdir(ctx, root, "d"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if _, err := store.Head(ctx, metadataObjectKey(dir)); !objstore.IsNotFound(err) {
		t.Fatalf("dir .metadata after Rmdir: err = %v, want not found", err)
	}
}

// TestLegacyDirectoryMigratesOnFirstWrite covers a directory created
// before directories had .metadata (attributes in its block's native
// metadata): it reads correctly as-is, and its first attribute write
// moves everything into .metadata without losing any of it.
func TestLegacyDirectoryMigratesOnFirstWrite(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	key, err := fsys.newKey()
	if err != nil {
		t.Fatalf("newKey: %v", err)
	}
	empty, err := (&block.Block{Kind: block.Leaf}).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	legacyMtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	native := map[string]string{
		dirMetaMode: "493", dirMetaUid: "11", dirMetaGid: "12", dirMetaNlink: "1",
		dirMetaMtime: fmt.Sprint(legacyMtime.UnixNano()),
	}
	if _, err := store.Put(ctx, key, bytes.NewReader(empty), native, ""); err != nil {
		t.Fatalf("put legacy block: %v", err)
	}
	if err := fsys.insertEntry(ctx, root, "old", key, TypeDir); err != nil {
		t.Fatalf("insert: %v", err)
	}

	attr, err := fsys.Stat(ctx, key, TypeDir)
	if err != nil {
		t.Fatalf("stat legacy: %v", err)
	}
	if attr.Mode != 0755 || attr.Uid != 11 || attr.Gid != 12 || !attr.Mtime.Equal(legacyMtime) {
		t.Fatalf("legacy attr = %+v", attr)
	}

	if err := fsys.TouchAtime(ctx, key, TypeDir); err != nil {
		t.Fatalf("TouchAtime: %v", err)
	}
	if _, err := store.Head(ctx, metadataObjectKey(key)); err != nil {
		t.Fatalf(".metadata after first write: %v", err)
	}
	after, err := fsys.Stat(ctx, key, TypeDir)
	if err != nil {
		t.Fatalf("stat migrated: %v", err)
	}
	if after.Mode != 0755 || after.Uid != 11 || after.Gid != 12 || !after.Mtime.Equal(legacyMtime) || after.Atime.IsZero() {
		t.Fatalf("migrated attr = %+v", after)
	}
}

// TestConcurrentCreatesAllSurvive: many writers creating different names
// in one directory at once, so blocks split under contention, lose none of
// them.
func TestConcurrentCreatesAllSurvive(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	dir, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "busy", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, errs[i] = fsys.Create(ctx, dir, fmt.Sprintf("f-%02d", i), 0o644, 0, 0)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	entries, err := fsys.ReadDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Name]++
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f-%02d", i)
		if _, _, err := fsys.Lookup(ctx, dir, name); err != nil {
			t.Errorf("lookup %s: %v", name, err)
		}
		if seen[name] != 1 {
			t.Errorf("%s listed %d times", name, seen[name])
		}
	}
	if len(entries) != n {
		t.Errorf("listed %d entries, want %d", len(entries), n)
	}
	checkClean(t, fsys)
}

// TestRightLinksAloneReachEverything: with parents never pointed at new
// right halves (as if every linkChild were lost), every name is still
// found, listed once and in order, found case-insensitively on a
// primary-Windows filesystem, and removable.
func TestRightLinksAloneReachEverything(t *testing.T) {
	linkParents = false
	t.Cleanup(func() { linkParents = true })
	ctx := context.Background()
	for _, windows := range []bool{false, true} {
		store := newTestStore(t)
		fsys := New(store, "links")
		if err := fsys.Bootstrap(ctx, 1<<30, 0o755, 0, 0, windows); err != nil {
			t.Fatal(err)
		}
		dir, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "d", 0o755, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		const n = 60
		for i := 0; i < n; i++ {
			if _, _, _, err := fsys.Create(ctx, dir, fmt.Sprintf("File-%03d", i), 0o644, 0, 0); err != nil {
				t.Fatalf("create %d: %v", i, err)
			}
		}
		entries, err := fsys.ReadDir(ctx, dir)
		if err != nil || len(entries) != n {
			t.Fatalf("windows=%v: listed %d, %v", windows, len(entries), err)
		}
		for i, e := range entries {
			if want := fmt.Sprintf("File-%03d", i); e.Name != want {
				t.Fatalf("windows=%v: entry %d = %s, want %s", windows, i, e.Name, want)
			}
		}
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("File-%03d", i)
			if _, _, err := fsys.Lookup(ctx, dir, name); err != nil {
				t.Fatalf("windows=%v: lookup %s: %v", windows, name, err)
			}
			if windows {
				if e, _, err := fsys.LookupFold(ctx, dir, strings.ToUpper(name)); err != nil || e.Name != name {
					t.Fatalf("lookup %s case-insensitively: %v, %v", name, e.Name, err)
				}
			}
		}
		for i := 0; i < n; i++ {
			if err := fsys.Unlink(ctx, dir, fmt.Sprintf("File-%03d", i)); err != nil {
				t.Fatalf("windows=%v: unlink %d: %v", windows, i, err)
			}
		}
		if entries, err := fsys.ReadDir(ctx, dir); err != nil || len(entries) != 0 {
			t.Fatalf("windows=%v: after removing all, %d listed, %v", windows, len(entries), err)
		}
	}
}
