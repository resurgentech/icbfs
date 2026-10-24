// Package storeflags is how every command takes the store's connection:
// the same flags, each defaulting to an environment variable, so a key
// never has to be on a command line.
//
//	-store            $ICBFS_STORE            (s3; or azure)
//
// An S3-API store (AWS S3, MinIO):
//
//	-endpoint         $ICBFS_ENDPOINT         (http://127.0.0.1:9000)
//	-bucket           $ICBFS_BUCKET           (icbfs)
//	-region           $ICBFS_REGION           (us-east-1)
//	-access-key       $ICBFS_ACCESS_KEY
//	-secret-key       $ICBFS_SECRET_KEY
//
// An Azure Blob Storage container:
//
//	-azure-account    $ICBFS_AZURE_ACCOUNT
//	-azure-container  $ICBFS_AZURE_CONTAINER  (icbfs)
//	-azure-key        $ICBFS_AZURE_KEY
//	-azure-endpoint   $ICBFS_AZURE_ENDPOINT   (https://ACCOUNT.blob.core.windows.net/)
package storeflags

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// Add registers the connection flags on fset. The returned value is
// filled in when fset is parsed.
func Add(fset *flag.FlagSet) *pkgicbfs.StoreConfig {
	c := &pkgicbfs.StoreConfig{}
	fset.StringVar(&c.Kind, "store", env("ICBFS_STORE", pkgicbfs.KindS3), "store: s3 (AWS S3, MinIO) or azure (Azure Blob Storage) ($ICBFS_STORE)")
	fset.StringVar(&c.S3.Endpoint, "endpoint", env("ICBFS_ENDPOINT", "http://127.0.0.1:9000"), "S3: endpoint ($ICBFS_ENDPOINT)")
	fset.StringVar(&c.S3.Bucket, "bucket", env("ICBFS_BUCKET", "icbfs"), "S3: bucket, with versioning on ($ICBFS_BUCKET)")
	fset.StringVar(&c.S3.Region, "region", env("ICBFS_REGION", "us-east-1"), "S3: region ($ICBFS_REGION)")
	fset.StringVar(&c.S3.AccessKey, "access-key", env("ICBFS_ACCESS_KEY", ""), "S3: access key ($ICBFS_ACCESS_KEY)")
	fset.StringVar(&c.S3.SecretKey, "secret-key", env("ICBFS_SECRET_KEY", ""), "S3: secret key ($ICBFS_SECRET_KEY)")
	fset.StringVar(&c.Azure.Account, "azure-account", env("ICBFS_AZURE_ACCOUNT", ""), "Azure: storage account, with blob versioning on ($ICBFS_AZURE_ACCOUNT)")
	fset.StringVar(&c.Azure.Container, "azure-container", env("ICBFS_AZURE_CONTAINER", "icbfs"), "Azure: container ($ICBFS_AZURE_CONTAINER)")
	fset.StringVar(&c.Azure.Key, "azure-key", env("ICBFS_AZURE_KEY", ""), "Azure: the account's key ($ICBFS_AZURE_KEY)")
	fset.StringVar(&c.Azure.Endpoint, "azure-endpoint", env("ICBFS_AZURE_ENDPOINT", ""), "Azure: blob service URL, if not https://ACCOUNT.blob.core.windows.net/ ($ICBFS_AZURE_ENDPOINT)")
	return c
}

// ErrNoKeys is Check's answer when the store's credentials are missing.
var ErrNoKeys = pkgicbfs.ErrNoStoreKeys

// Check reports whether c names a store and has its credentials, saying
// which flags to give if not.
func Check(c *pkgicbfs.StoreConfig) error {
	err := c.Check()
	if errors.Is(err, ErrNoKeys) {
		if c.Kind == pkgicbfs.KindAzure {
			return fmt.Errorf("%w: give -azure-key, or set ICBFS_AZURE_KEY", err)
		}
		return fmt.Errorf("%w: give -access-key and -secret-key, or set ICBFS_ACCESS_KEY and ICBFS_SECRET_KEY", err)
	}
	return err
}

// History is where as-of views keep the store's history, and for how
// long: the -history-cache-dir and -history-ttl flags.
type History struct {
	Dir string
	TTL time.Duration
}

// AddHistory registers the history cache flags on fset.
func AddHistory(fset *flag.FlagSet) *History {
	h := &History{}
	fset.StringVar(&h.Dir, "history-cache-dir", objstore.DefaultHistoryCacheDir(), "as-of mounts: directory of the history caches, one per store and credential, kept between mounts and shared with every other as-of mount and program reading history")
	fset.DurationVar(&h.TTL, "history-ttl", objstore.DefaultHistoryTTL, "as-of mounts: how long the history cache keeps what it fetched (a purged file's name and times stay in it until then)")
	return h
}

// Open opens the history cache of the store c says how to reach. If it
// can't (another user's directory, say), it logs why and opens a
// temporary one instead, which Close removes: the past is readable
// either way, only not shared.
func (h *History) Open(ctx context.Context, store objstore.Store, c *pkgicbfs.StoreConfig) (*objstore.HistoryCache, error) {
	who := c.S3.AccessKey
	if c.Kind == pkgicbfs.KindAzure {
		who = c.Azure.Account
	}
	hist, err := objstore.OpenHistoryCache(ctx, store, objstore.HistoryCachePath(h.Dir, c.String()+" as "+who), h.TTL)
	if err == nil {
		return hist, nil
	}
	log.Printf("%v; using a temporary history cache, for this mount only", err)
	return objstore.OpenTempHistoryCache(ctx, store, "")
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
