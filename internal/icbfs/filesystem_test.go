package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil"
)

// newTestStore spins up a real MinIO container and a versioned bucket,
// and returns a Store backed by it, with no filesystem bootstrapped yet
// — callers that need more than one named filesystem in the same
// bucket (the master-block tests) start here instead of
// newTestFilesystem. Requires Docker.
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

// TestDirectorySplitsAndStaysConsistent creates enough entries to force
// several leaf splits (maxEntriesPerBlock is 8), and checks that the
// directory's own root block actually becomes an Internal node, that
// every entry is still findable both via Lookup and via ReadDir's full
// traversal, and that removal (including a root split's two separate
// child subtrees) still works correctly afterward.
func TestDirectorySplitsAndStaysConsistent(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	const n = 40 // several multiples of maxEntriesPerBlock (8)
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
		t.Fatalf("root is still a flat Leaf after creating %d entries (maxEntriesPerBlock=%d) — sharding did not kick in", n, maxEntriesPerBlock)
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

// TestConcurrentLinkRaceDoesNotLoseUpdates is the hardening proof for
// nlink: without the CAS-protected side object (see adjustNlink's doc
// comment), concurrent Link calls on the same target would race on a
// plain read-increment-write of metadata and lose updates. Here N
// goroutines link the same target concurrently; the authoritative side
// object (not the best-effort metadata cache) must reflect every one of
// them.
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

// TestXattrsRoundTripArbitraryBinaryValues proves the ROADMAP-called-for
// property directly against real MinIO: xattrs is a generic
// map[string][]byte, and arbitrary binary content — not just printable
// text — round-trips through the real Protobuf-encoded .metadata object
// without corruption or truncation. There's no public Filesystem API for
// setting xattrs yet (nothing consumes it — no FUSE getxattr/setxattr
// wiring exists), so this writes directly to the real .metadata object
// via the same encodeFileMetadata/decodeFileMetadata functions Create/
// Stat/SetAttr already use internally, proving the wire format itself
// is correct ahead of any caller needing it.
func TestXattrsRoundTripArbitraryBinaryValues(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "has-xattrs.txt", 0644, 1000, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Arbitrary binary content: not valid UTF-8, includes null bytes and
	// high bytes — exactly the kind of thing base64-in-JSON would have
	// needed encoding for, and Protobuf's native bytes type shouldn't
	// care about at all.
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

// TestXattrsPosixAndWindowsACLsCoexist proves the union-map design from
// ARCHITECTURE.md directly: a POSIX ACL entry (the real well-known key
// Linux's own getfacl/setfacl use) and a Windows ACL entry (the reserved
// key this project chose) live in the same xattrs map without
// interfering with each other — no special-casing at the storage layer,
// exactly as designed.
func TestXattrsPosixAndWindowsACLsCoexist(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "has-acls.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	posixACL := []byte{0x02, 0x00, 0x00, 0x00 /* a plausible posix_acl binary header shape */}
	windowsACL := []byte("\x01\x00\x04\x80fake-security-descriptor-bytes")

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
	attr.Xattrs = map[string][]byte{
		"system.posix_acl_access": posixACL,
		"windows.acl":             windowsACL,
		"user.unrelated-tag":      []byte("just a tag"),
	}
	newData, err := encodeFileMetadata(attr)
	if err != nil {
		t.Fatalf("encode .metadata: %v", err)
	}
	if _, err := store.Put(ctx, mk, bytes.NewReader(newData), nil, obj.ETag); err != nil {
		t.Fatalf("put .metadata: %v", err)
	}

	got, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !bytes.Equal(got.Xattrs["system.posix_acl_access"], posixACL) {
		t.Fatalf("posix ACL corrupted or missing: %v", got.Xattrs["system.posix_acl_access"])
	}
	if !bytes.Equal(got.Xattrs["windows.acl"], windowsACL) {
		t.Fatalf("windows ACL corrupted or missing: %v", got.Xattrs["windows.acl"])
	}
	if string(got.Xattrs["user.unrelated-tag"]) != "just a tag" {
		t.Fatalf("unrelated tag disturbed: %v", got.Xattrs["user.unrelated-tag"])
	}
	if len(got.Xattrs) != 3 {
		t.Fatalf("got %d xattrs entries, want exactly 3 (no cross-contamination): %v", len(got.Xattrs), got.Xattrs)
	}
}

// TestSetAttrRejectsNegativeSizeInsteadOfCrashing is a real regression
// test: a negative *size used to reach make([]byte, *size) directly
// and panic the whole process with "makeslice: len out of range" —
// found running pjdfstest's real conformance suite against a real
// FUSE mount (its truncate/unlink/utimensat groups exercise exactly
// this), which crashed the mount outright, failing every subsequent
// test in the same run for an unrelated reason. ftruncate(2)/
// truncate(2) themselves reject a negative length with EINVAL, so
// this must too, as a real error, not a crash.
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
	if _, err := fsys.SetAttr(ctx, uuid, TypeFile, nil, nil, nil, &negative); !errors.Is(err, ErrInvalidArgument) {
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

// TestSetAttrRejectsHugeSizeInsteadOfCrashing is
// TestSetAttrRejectsNegativeSizeInsteadOfCrashing's sibling: the exact
// same "makeslice: len out of range" panic, found via the exact same
// real pjdfstest run, for a different reason — pjdfstest's own
// truncate/12.t and ftruncate/12.t specifically probe
// `truncate(path, 999999999999999)` (~909TB) and accept either EFBIG
// or EINVAL as correct, since this implementation has no sparse-file
// support (a resize always materializes the whole new size as one
// in-memory []byte — see maxSetAttrSize's own doc comment).
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
	if _, err := fsys.SetAttr(ctx, uuid, TypeFile, nil, nil, nil, &huge); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("SetAttr with huge size = %v, want ErrInvalidArgument", err)
	}

	data, _, _, err := fsys.ReadFile(ctx, uuid)
	if err != nil {
		t.Fatalf("read after rejected SetAttr: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("content disturbed by rejected SetAttr: got %q, want %q", data, "hello")
	}
}

// TestSetWindowsAttributesRoundTripsAndMasksUnrelatedBits covers task
// F7's storage half: SetWindowsAttributes/WindowsAttributesFromXattrs,
// the real public API (unlike the two tests above, which predate it
// and poke .metadata directly). Confirms a round trip through real
// storage, that unrelated Win32 bits (e.g. FILE_ATTRIBUTE_NORMAL,
// FILE_ATTRIBUTE_DIRECTORY) passed in by a careless caller are masked
// out rather than stored verbatim, and that mode/uid/gid/nlink are
// left untouched by the call — same invariant
// TestXattrsRoundTripArbitraryBinaryValues already established for
// plain xattrs.
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

	attr, err := fsys.SetWindowsAttributes(ctx, uuid, in)
	if err != nil {
		t.Fatalf("SetWindowsAttributes: %v", err)
	}
	want := WindowsAttrHidden | WindowsAttrArchive
	bits, ok := WindowsAttributesFromXattrs(attr.Xattrs)
	if !ok {
		t.Fatal("WindowsAttributesFromXattrs: ok = false immediately after SetWindowsAttributes")
	}
	if bits != want {
		t.Fatalf("stored bits = %#x, want %#x (Directory/Normal should be masked out)", bits, want)
	}
	// Check the raw stored byte directly, bypassing
	// WindowsAttributesFromXattrs's own read-side mask — proves
	// SetWindowsAttributes itself masks at write time, not just that
	// the reader happens to hide an unmasked value.
	if raw := attr.Xattrs[WindowsAttributesXattrKey]; len(raw) != 1 || raw[0] != byte(want) {
		t.Fatalf("raw stored byte = %v, want [%#x] (Directory/Normal must not reach storage at all)", raw, byte(want))
	}

	// Re-read via Stat, not just the return value, to prove this is
	// really persisted, not just echoed back.
	got, err := fsys.Stat(ctx, uuid, TypeFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	bits, ok = WindowsAttributesFromXattrs(got.Xattrs)
	if !ok {
		t.Fatal("WindowsAttributesFromXattrs: ok = false after Stat")
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
	if _, ok := WindowsAttributesFromXattrs(otherAttr.Xattrs); ok {
		t.Fatalf("WindowsAttributesFromXattrs: ok = true for an entry never touched, xattrs = %v", otherAttr.Xattrs)
	}
}

// TestSetWindowsACLRoundTrips covers task F8's storage half:
// SetWindowsACL/WindowsACLFromXattrs. The value itself is treated as
// an opaque SDDL string here — parsing/merging it into a real
// *windows.SECURITY_DESCRIPTOR is winfspserver's job (Windows-only,
// verified separately against a real mount, per ROADMAP.md's F8
// entry) — this test only has to prove the storage round trip is
// correct and that an untouched entry reports ok = false.
func TestSetWindowsACLRoundTrips(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	uuid, _, _, err := fsys.Create(ctx, root, "has-win-acl.txt", 0644, 1000, 1000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sddl := []byte("O:S-1-5-21-1-2-3-1000G:S-1-5-21-1-2-3-1001D:(A;;FA;;;S-1-5-21-1-2-3-1000)")
	attr, err := fsys.SetWindowsACL(ctx, uuid, sddl)
	if err != nil {
		t.Fatalf("SetWindowsACL: %v", err)
	}
	got, ok := WindowsACLFromXattrs(attr.Xattrs)
	if !ok {
		t.Fatal("WindowsACLFromXattrs: ok = false immediately after SetWindowsACL")
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
	got, ok = WindowsACLFromXattrs(statAttr.Xattrs)
	if !ok {
		t.Fatal("WindowsACLFromXattrs: ok = false after Stat")
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
	if _, ok := WindowsACLFromXattrs(otherAttr.Xattrs); ok {
		t.Fatalf("WindowsACLFromXattrs: ok = true for an entry never touched, xattrs = %v", otherAttr.Xattrs)
	}
}
