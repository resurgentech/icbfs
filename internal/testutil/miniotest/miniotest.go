// Package miniotest starts a throwaway MinIO (in Docker) with a
// versioned bucket, for tests outside package icbfs.
package miniotest

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/resurgentech/icbfs/internal/testutil/minioimage"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
	"github.com/resurgentech/icbfs/internal/testutil"
)

// Server is how to reach the MinIO Start started.
type Server struct {
	Endpoint, AccessKey, SecretKey, Bucket string
}

// Store is a versioned bucket in a MinIO that lives as long as t.
func Store(t *testing.T) objstore.Store {
	t.Helper()
	store, _ := Start(t)
	return store
}

// Start is Store, also saying how to reach it.
func Start(t *testing.T) (objstore.Store, Server) {
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
		Credentials:  awscreds.NewStaticCredentialsProvider(container.Username, container.Password, ""),
	})
	const bucket = "icbfs-test"
	testutil.RetryUntilReady(t, 10*time.Second, func() error {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		return err
	})
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("enable bucket versioning: %v", err)
	}
	return s3store.New(client, bucket), Server{Endpoint: "http://" + endpoint, AccessKey: container.Username, SecretKey: container.Password, Bucket: bucket}
}
