package miniosrc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	testminio "github.com/testcontainers/testcontainers-go/modules/minio"
)

// TestMinIOSourceReceivesRealNotification covers ROADMAP.md's task E4
// "Done when": a real MinIO container test proves an actual change
// produces an actual received notification — the one backend that
// can be tested the way everything else in this project is (unlike
// Azure/AWS S3, tasks E5/E6, which have no real account available in
// this environment).
func TestMinIOSourceReceivesRealNotification(t *testing.T) {
	ctx := context.Background()

	container, err := testminio.Run(ctx, "minio/minio:latest")
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

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(container.Username, container.Password, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("new minio client: %v", err)
	}

	const bucket = "icbfs-notify-test"
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}

	const prefix = "0000-"
	src := New(ctx, client, bucket, prefix)
	t.Cleanup(func() { src.Close() })

	// Give ListenBucketNotification's first long-poll request a moment
	// to actually establish before the write below — there's no readiness
	// signal exposed by the API, so a short, generous wait is the
	// pragmatic choice here (consistent with this being a latency-
	// tolerant feature by design).
	time.Sleep(500 * time.Millisecond)

	// A key under prefix should be delivered (matches the server-side
	// filter); a key outside it, written at the same time, should not.
	const matchingKey = prefix + "some-uuid"
	const nonMatchingKey = "9999-some-other-uuid"
	if _, err := client.PutObject(ctx, bucket, matchingKey, strings.NewReader("v1"), 2, minio.PutObjectOptions{}); err != nil {
		t.Fatalf("put matching object: %v", err)
	}
	if _, err := client.PutObject(ctx, bucket, nonMatchingKey, strings.NewReader("v1"), 2, minio.PutObjectOptions{}); err != nil {
		t.Fatalf("put non-matching object: %v", err)
	}

	select {
	case sig := <-src.Signals():
		if sig.Key != matchingKey {
			t.Fatalf("got signal for key %q, want %q (the prefix-scoped match)", sig.Key, matchingKey)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for a real MinIO bucket notification")
	}

	// The non-matching key must never arrive — confirms the server-side
	// prefix filter, not just that signals arrive at all.
	select {
	case sig := <-src.Signals():
		t.Fatalf("received an unexpected second signal for key %q — prefix filtering isn't working", sig.Key)
	case <-time.After(2 * time.Second):
		// Expected: nothing else should arrive.
	}
}
