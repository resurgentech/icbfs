// Package icbfstest is for tests of programs built on icbfs: a throwaway
// MinIO, in Docker, with a versioned bucket, that lives as long as the
// test.
package icbfstest

import (
	"testing"

	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// Store is a versioned bucket in a MinIO that lives as long as t.
func Store(t *testing.T) pkgicbfs.Store {
	t.Helper()
	return miniotest.Store(t)
}

// Start is Store, also saying how to reach it, as a store config (for
// what opens the store itself, such as pkg/history).
func Start(t *testing.T) (pkgicbfs.Store, pkgicbfs.StoreConfig) {
	t.Helper()
	store, srv := miniotest.Start(t)
	return store, pkgicbfs.StoreConfig{Kind: pkgicbfs.KindS3, S3: pkgicbfs.S3{Endpoint: srv.Endpoint, Bucket: srv.Bucket,
		AccessKey: srv.AccessKey, SecretKey: srv.SecretKey}}
}
