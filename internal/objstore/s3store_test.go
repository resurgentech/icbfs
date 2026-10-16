package objstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/resurgentech/icbfs/internal/testutil/minioimage"

	"github.com/resurgentech/icbfs/internal/testutil"
)

// newTestStore spins up a real MinIO container, creates a versioned bucket
// in it, and returns a Store backed by it. Requires Docker.
func newTestStore(t *testing.T) *S3Store {
	t.Helper()
	ctx := context.Background()

	container, err := minio.Run(ctx, minioimage.Image())
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

	const bucket = "icbfs-test"
	testutil.RetryUntilReady(t, 10*time.Second, func() error {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		return err
	})
	// Snapshotting depends entirely on bucket versioning being enabled;
	// this is the architectural precondition from docs/architecture.md.
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		t.Fatalf("enable bucket versioning: %v", err)
	}

	return NewS3Store(client, bucket)
}

func TestPutGetRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	obj, err := store.Put(ctx, "file-1", strings.NewReader("hello icbfs"), map[string]string{
		"mode": "0644",
	}, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if obj.VersionID == "" {
		t.Fatal("expected a version id on a versioned bucket")
	}

	body, got, err := store.Get(ctx, "file-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != "hello icbfs" {
		t.Fatalf("got body %q, want %q", data, "hello icbfs")
	}
	if got.Metadata["mode"] != "0644" {
		t.Fatalf("got mode metadata %q, want %q", got.Metadata["mode"], "0644")
	}
}

func TestHeadReturnsMetadataAndSize(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.Put(ctx, "file-1", strings.NewReader("twelve bytes"), map[string]string{
		"uid": "1000",
		"gid": "1000",
	}, ""); err != nil {
		t.Fatalf("put: %v", err)
	}

	head, err := store.Head(ctx, "file-1")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Size != int64(len("twelve bytes")) {
		t.Fatalf("got size %d, want %d", head.Size, len("twelve bytes"))
	}
	if head.Metadata["uid"] != "1000" || head.Metadata["gid"] != "1000" {
		t.Fatalf("got metadata %+v, want uid/gid=1000", head.Metadata)
	}
}

// TestMetadataUpdateCreatesNewVersion: a metadata-only update leaves the
// body alone and still makes a new version.
func TestMetadataUpdateCreatesNewVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	created, err := store.Put(ctx, "file-1", strings.NewReader("unchanged content"), map[string]string{
		"mode": "0644",
	}, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	updated, err := store.UpdateMetadata(ctx, "file-1", map[string]string{
		"mode": "0600",
	}, "")
	if err != nil {
		t.Fatalf("update metadata: %v", err)
	}

	if updated.VersionID == created.VersionID {
		t.Fatal("expected a metadata-only update to create a new version")
	}
	if updated.Metadata["mode"] != "0600" {
		t.Fatalf("got mode %q, want %q", updated.Metadata["mode"], "0600")
	}

	body, _, err := store.Get(ctx, "file-1")
	if err != nil {
		t.Fatalf("get after metadata update: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != "unchanged content" {
		t.Fatalf("metadata-only update changed the body: got %q", data)
	}

	versions, err := store.ListVersions(ctx, "file-1")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2 (one content write, one metadata update)", len(versions))
	}
}

// TestListVersionsIsNewestFirstWithTimestamps: a key's versions list newest
// first with timestamps, which as-of views pick from.
func TestListVersionsIsNewestFirstWithTimestamps(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.Put(ctx, "file-1", strings.NewReader("v1"), nil, ""); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // S3 LastModified has ~1s resolution
	tBetween := time.Now()
	time.Sleep(1100 * time.Millisecond)
	if _, err := store.Put(ctx, "file-1", strings.NewReader("v2"), nil, ""); err != nil {
		t.Fatalf("put v2: %v", err)
	}

	versions, err := store.ListVersions(ctx, "file-1")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}
	if !versions[0].LastModified.After(versions[1].LastModified) {
		t.Fatalf("expected newest-first ordering, got %+v", versions)
	}

	// Walk logic from docs/architecture.md: the version current as of tBetween
	// is the one with the latest LastModified <= tBetween.
	var asOfBetween *Object
	for i := range versions {
		if !versions[i].LastModified.After(tBetween) {
			asOfBetween = &versions[i]
			break
		}
	}
	if asOfBetween == nil {
		t.Fatal("expected to find a version current as of tBetween")
	}

	asOfData := mustGetVersion(t, store, "file-1", asOfBetween.VersionID)
	if string(asOfData) != "v1" {
		t.Fatalf("version resolved as-of tBetween = %q, want %q", asOfData, "v1")
	}
}

func mustGetVersion(t *testing.T, store *S3Store, key, versionID string) []byte {
	t.Helper()
	ctx := context.Background()
	out, err := store.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    aws.String(store.bucket),
		Key:       aws.String(key),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		t.Fatalf("get version %s: %v", versionID, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read version %s: %v", versionID, err)
	}
	return data
}

// TestConditionalPutRejectsStaleETag validates the concurrency mechanism
// docs/architecture.md commits to: a write conditioned on a stale ETag must be
// rejected, not silently overwrite a concurrent change.
func TestConditionalPutRejectsStaleETag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	v1, err := store.Put(ctx, "dir-block", strings.NewReader("entry-a"), nil, "")
	if err != nil {
		t.Fatalf("initial put: %v", err)
	}

	// A second writer updates the object without knowing about a third
	// writer, moving the ETag forward.
	if _, err := store.Put(ctx, "dir-block", strings.NewReader("entry-a,entry-b"), nil, v1.ETag); err != nil {
		t.Fatalf("second writer's conditional put should have succeeded: %v", err)
	}

	// A first writer, still holding the stale v1 ETag, tries to write —
	// this must be rejected rather than silently clobbering entry-b.
	_, err = store.Put(ctx, "dir-block", strings.NewReader("entry-a,entry-c"), nil, v1.ETag)
	if err == nil {
		t.Fatal("expected conditional put with a stale ETag to fail")
	}
	if !IsPreconditionFailed(err) {
		t.Fatalf("expected a precondition-failed error, got: %v", err)
	}

	body, _, err := store.Get(ctx, "dir-block")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "entry-a,entry-b" {
		t.Fatalf("stale write must not have applied: got %q", data)
	}
}

// TestConditionalUpdateMetadataIgnoresUnchangedETag records a limit: an
// ETag is a hash of the body, so a metadata-only update leaves it as it
// was, and a conditional write can't catch two metadata-only writers racing
// (content writes are caught: TestConditionalPutRejectsStaleETag). That's
// why attributes live in .metadata objects, whose bodies change.
func TestConditionalUpdateMetadataIgnoresUnchangedETag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	v1, err := store.Put(ctx, "file-1", strings.NewReader("unchanging body"), map[string]string{
		"nlink": "1",
	}, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	v2, err := store.UpdateMetadata(ctx, "file-1", map[string]string{"nlink": "2"}, v1.ETag)
	if err != nil {
		t.Fatalf("first conditional metadata update should have succeeded: %v", err)
	}

	if v2.ETag != v1.ETag {
		t.Fatalf("got etag %q after a metadata-only update, want it unchanged at %q — if this ever fails, the backend's ETag semantics changed and the nlink side-object workaround may no longer be needed", v2.ETag, v1.ETag)
	}

	// Because the ETag never moved, a "stale" writer's conditional update
	// does not actually get rejected — this is the crux of the finding.
	v3, err := store.UpdateMetadata(ctx, "file-1", map[string]string{"nlink": "99"}, v1.ETag)
	if err != nil {
		t.Fatalf("expected this update to succeed (unprotected) given unchanged ETag, got: %v", err)
	}
	if v3.Metadata["nlink"] != "99" {
		t.Fatalf("got nlink %q, want %q", v3.Metadata["nlink"], "99")
	}
}

// TestConditionalDeleteIgnoresIfMatch records another: MinIO ignores
// If-Match on DeleteObject, so icbfs never relies on a conditional delete
// (adjustNlink writes a tombstone with a conditional Put instead).
func TestConditionalDeleteIgnoresIfMatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	v1, err := store.Put(ctx, "file-1", strings.NewReader("content"), nil, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// Someone else updates the object after we observed v1, which (per
	// the finding above) may not even move the ETag if it's metadata-only
	// — so use a real content change to guarantee a different ETag.
	if _, err := store.Put(ctx, "file-1", strings.NewReader("different content"), nil, ""); err != nil {
		t.Fatalf("concurrent update: %v", err)
	}

	err = store.Delete(ctx, "file-1", v1.ETag)
	if err != nil {
		t.Fatalf("expected delete with a stale If-Match to succeed unprotected (MinIO does not enforce it), got: %v", err)
	}

	if _, err := store.Head(ctx, "file-1"); !IsNotFound(err) {
		t.Fatalf("expected object to be gone after the unprotected delete, head error: %v", err)
	}
}

// TestServerTimeReturnsRealClockFromDateHeader: MinIO returns a Date
// header, and it parses to about now, within a budget covering its
// whole-second resolution.
func TestServerTimeReturnsRealClockFromDateHeader(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	before := time.Now()
	got, err := store.ServerTime(ctx)
	after := time.Now()
	if err != nil {
		t.Fatalf("server time: %v", err)
	}

	// The header reads in whole seconds, so it can trail the request by
	// almost a second with no skew at all.
	const skewBudget = 2 * time.Second
	if got.Before(before.Add(-skewBudget)) || got.After(after.Add(skewBudget)) {
		t.Fatalf("server time %v is outside [%v, %v] (request window ± %v) — looks like real clock skew, not just Date's whole-second rounding", got, before.Add(-skewBudget), after.Add(skewBudget), skewBudget)
	}
}

// TestPutPartsCopiesAndUploadsUnderIfMatch: a multipart write mixing
// server-side copies of an older version's ranges with uploaded parts
// assembles exactly those bytes, honors If-Match, and leaves the object
// alone when it fails.
func TestPutPartsCopiesAndUploadsUnderIfMatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const mib = 1 << 20

	base := bytes.Repeat([]byte("a"), 12*mib)
	v1, err := store.Put(ctx, "big", bytes.NewReader(base), nil, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	middle := bytes.Repeat([]byte("M"), 5*mib)
	parts := func() []Part {
		return []Part{
			{Size: 5 * mib, CopyVersion: v1.VersionID, CopyOffset: 0},
			{Size: 5 * mib, Body: bytes.NewReader(middle)},
			{Size: 2 * mib, CopyVersion: v1.VersionID, CopyOffset: 10 * mib},
		}
	}

	if _, err := store.PutParts(ctx, "big", parts(), nil, `"stale-etag"`); !IsPreconditionFailed(err) {
		t.Fatalf("PutParts with a stale If-Match: %v, want precondition failed", err)
	}
	v2, err := store.PutParts(ctx, "big", parts(), map[string]string{"k": "v"}, v1.ETag)
	if err != nil {
		t.Fatalf("PutParts: %v", err)
	}
	want := append(append(bytes.Repeat([]byte("a"), 5*mib), middle...), bytes.Repeat([]byte("a"), 2*mib)...)
	body, obj, err := store.Get(ctx, "big")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(got, want) || obj.Size != 12*mib || obj.ETag != v2.ETag || obj.Metadata["k"] != "v" {
		t.Fatalf("assembled object: size %d etag %s (want %s) metadata %v, content equal %v",
			obj.Size, obj.ETag, v2.ETag, obj.Metadata, bytes.Equal(got, want))
	}

	// A part below the minimum fails the whole write; nothing changes.
	bad := []Part{{Size: 1, Body: bytes.NewReader([]byte("x"))}, {Size: 1, Body: bytes.NewReader([]byte("y"))}}
	if _, err := store.PutParts(ctx, "big", bad, nil, v2.ETag); err == nil {
		t.Fatal("PutParts with an undersized first part succeeded")
	}
	if head, err := store.Head(ctx, "big"); err != nil || head.ETag != v2.ETag {
		t.Fatalf("object after a failed PutParts: %v, %v; want unchanged %s", head, err, v2.ETag)
	}
}

// TestDeleteVersion: removing a delete marker undoes a delete, and
// removing a version erases it from history.
func TestDeleteVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	v1, err := store.Put(ctx, "k", strings.NewReader("one"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "k", strings.NewReader("two"), nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "k", ""); err != nil {
		t.Fatal(err)
	}
	versions, err := store.ListVersions(ctx, "k")
	if err != nil || len(versions) != 3 || !versions[0].DeleteMarker {
		t.Fatalf("after delete: %+v, %v", versions, err)
	}
	if err := store.DeleteVersion(ctx, "k", versions[0].VersionID); err != nil {
		t.Fatalf("remove the delete marker: %v", err)
	}
	body, _, err := store.Get(ctx, "k")
	if err != nil {
		t.Fatalf("get after undelete: %v", err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "two" {
		t.Fatalf("after undelete: %q, want two", data)
	}
	if err := store.DeleteVersion(ctx, "k", v1.VersionID); err != nil {
		t.Fatalf("erase version one: %v", err)
	}
	versions, _ = store.ListVersions(ctx, "k")
	if len(versions) != 1 || versions[0].VersionID == v1.VersionID {
		t.Fatalf("history after erasing one version: %+v", versions)
	}
}

// failingReader fails after its first n bytes, as a connection the server
// reset would.
type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	n := min(len(p), r.n)
	clear(p[:n])
	r.n -= n
	return n, nil
}

// TestPutCutOffIsLostRaceOnlyIfChanged: a conditional Put that fails without
// a server answer is a lost race if the object no longer matches, and an
// ordinary failure if it still does.
func TestPutCutOffIsLostRaceOnlyIfChanged(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	obj, err := s.Put(ctx, "k", strings.NewReader("v1"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, "k", &failingReader{n: 1 << 10}, nil, `"stale"`)
	if !IsPreconditionFailed(err) {
		t.Fatalf("cut off, object changed: %v, want a precondition failure", err)
	}
	_, err = s.Put(ctx, "k", &failingReader{n: 1 << 10}, nil, obj.ETag)
	if err == nil || IsPreconditionFailed(err) {
		t.Fatalf("cut off, object unchanged: %v, want a plain failure", err)
	}
}
