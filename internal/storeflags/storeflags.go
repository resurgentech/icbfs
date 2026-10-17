// Package storeflags is how every command takes the store's connection:
// the same flags, each defaulting to an environment variable, so a key
// never has to be on a command line. For an Azure Blob Storage store,
// -endpoint is the service URL, -bucket the container, -access-key the
// account name and -secret-key the account key.
//
//	-store       $ICBFS_STORE       (s3; or azure)
//	-endpoint    $ICBFS_ENDPOINT    (http://127.0.0.1:9000)
//	-bucket      $ICBFS_BUCKET      (icbfs)
//	-region      $ICBFS_REGION      (us-east-1)
//	-access-key  $ICBFS_ACCESS_KEY
//	-secret-key  $ICBFS_SECRET_KEY
package storeflags

import (
	"errors"
	"flag"
	"os"

	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// Add registers the connection flags on fset. The returned value is
// filled in when fset is parsed.
func Add(fset *flag.FlagSet) *pkgicbfs.S3 {
	c := &pkgicbfs.S3{}
	fset.StringVar(&c.Type, "store", env("ICBFS_STORE", "s3"), "store: s3 (AWS S3, MinIO) or azure (Azure Blob Storage) ($ICBFS_STORE)")
	fset.StringVar(&c.Endpoint, "endpoint", env("ICBFS_ENDPOINT", "http://127.0.0.1:9000"), "S3-API endpoint ($ICBFS_ENDPOINT)")
	fset.StringVar(&c.Bucket, "bucket", env("ICBFS_BUCKET", "icbfs"), "bucket, with versioning on ($ICBFS_BUCKET)")
	fset.StringVar(&c.Region, "region", env("ICBFS_REGION", "us-east-1"), "region ($ICBFS_REGION)")
	fset.StringVar(&c.AccessKey, "access-key", env("ICBFS_ACCESS_KEY", ""), "access key ($ICBFS_ACCESS_KEY)")
	fset.StringVar(&c.SecretKey, "secret-key", env("ICBFS_SECRET_KEY", ""), "secret key ($ICBFS_SECRET_KEY)")
	return c
}

// ErrNoKeys is Check's answer when either key is missing.
var ErrNoKeys = errors.New("no store credentials: give -access-key and -secret-key, or set ICBFS_ACCESS_KEY and ICBFS_SECRET_KEY")

// Check reports whether c has both keys.
func Check(c *pkgicbfs.S3) error {
	if c.AccessKey == "" || c.SecretKey == "" {
		return ErrNoKeys
	}
	return nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
