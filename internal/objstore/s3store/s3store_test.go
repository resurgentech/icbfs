package s3store_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
	"github.com/resurgentech/icbfs/internal/objstore/storetest"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
)

// TestConformance runs the store suite on a MinIO container, each test's
// keys under a prefix of its own; or, with ICBFS_TEST_S3_BUCKET set, on
// that real bucket (see externalStore).
func TestConformance(t *testing.T) {
	var base *s3store.Store
	if bucket := os.Getenv("ICBFS_TEST_S3_BUCKET"); bucket != "" {
		base = externalStore(bucket)
	} else {
		store, _ := miniotest.Start(t)
		base = store.(*s3store.Store)
	}
	run := time.Now().UTC().Format("20060102T150405.000")
	storetest.Run(t, func(t *testing.T) objstore.Store {
		return base.WithKeyPrefix(fmt.Sprintf("objstore-test/%s/%s/", run, t.Name()))
	})
}

// externalStore is a real S3 bucket: ICBFS_TEST_S3_BUCKET names it
// (versioned), ICBFS_TEST_S3_REGION its region (default us-east-1),
// ICBFS_TEST_S3_ENDPOINT its endpoint (default AWS's for the region), and
// the AWS_* variables the credentials. icbfs addresses buckets
// path-style, so this does too.
func externalStore(bucket string) *s3store.Store {
	region := os.Getenv("ICBFS_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := os.Getenv("ICBFS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(
			os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN"),
		),
	})
	return s3store.New(client, bucket)
}
