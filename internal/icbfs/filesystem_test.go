package icbfs

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// newTestFilesystem spins up a real MinIO container and a versioned
// bucket, and returns a Filesystem backed by it. Requires Docker.
func newTestFilesystem(t *testing.T) (*Filesystem, objstore.Store) {
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

	store := objstore.NewS3Store(client, bucket)
	fsys := New(store, "test")
	if err := fsys.Bootstrap(ctx, 0755, 0, 0); err != nil {
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
		if _, _, err := fsys.Create(ctx, root, name, 0644, 0, 0); err != nil {
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

	targetUUID, _, err := fsys.Create(ctx, root, "original", 0644, 0, 0)
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
	if _, _, err := fsys.ReadFile(ctx, targetUUID); err != nil {
		t.Fatalf("original file should still exist: %v", err)
	}
}

func mustReadNlinkSideObject(t *testing.T, ctx context.Context, store objstore.Store, uuid string) int {
	t.Helper()
	body, _, err := store.Get(ctx, nlinkKey(uuid))
	if err != nil {
		t.Fatalf("get nlink side object: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read nlink side object: %v", err)
	}
	var n int
	fmt.Sscanf(string(data), "%d", &n)
	return n
}
