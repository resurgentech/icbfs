package objstore

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go/modules/minio"
)

// newTestStore spins up a real MinIO container, creates a versioned bucket
// in it, and returns a Store backed by it. Requires Docker.
func newTestStore(t *testing.T) *S3Store {
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

	const bucket = "icbfs-test"
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	// Snapshotting depends entirely on bucket versioning being enabled;
	// this is the architectural precondition from ARCHITECTURE.md.
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

// TestMetadataUpdateCreatesNewVersion is the core assumption behind
// ARCHITECTURE.md's metadata model: a metadata-only update (chmod/chown)
// must not rewrite the body, and must still create a new version on a
// versioned bucket — the same mechanism a content write uses.
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

// TestListVersionsIsNewestFirstWithTimestamps validates the other load-
// bearing assumption: ARCHITECTURE.md's point-in-time tree walk depends on
// being able to list a key's versions with usable timestamps and pick the
// one current as of some time T.
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

	// Walk logic from ARCHITECTURE.md: the version current as of tBetween
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
// ARCHITECTURE.md commits to: a write conditioned on a stale ETag must be
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

// TestConditionalUpdateMetadataIgnoresUnchangedETag documents a real,
// empirically-discovered limitation (not a bug): ETag on an S3-compatible
// store is a hash of the object's *body*. A metadata-only update
// (CopySourceIfMatch, since source and destination are the same key)
// does not change the body, so it does not change the ETag either — two
// different metadata states can share the exact same ETag. That means
// ETag-based CAS cannot detect a lost race between two metadata-only
// writers, even though it works correctly for content writes (see
// TestConditionalPutRejectsStaleETag). This is why icbfs's nlink hardening
// does not use UpdateMetadata's ifMatch for correctness — see
// icbfs.adjustNlink's doc comment for the actual fix (a dedicated side
// object whose body really does change).
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

// TestConditionalDeleteIgnoresIfMatch documents another empirically-
// discovered limitation: MinIO does not enforce DeleteObject's If-Match
// header at all — a delete conditioned on a deliberately stale ETag
// still succeeds. icbfs's nlink hardening therefore never relies on
// conditional delete; see icbfs.adjustNlink's doc comment for the
// tombstone-via-conditional-Put protocol used instead.
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
