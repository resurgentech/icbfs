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
	})
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
