// Package icbfs is what code outside this module — a host application —
// uses of the icbfs engine: opening a bucket, and administering the
// filesystems in it (the storage endpoints and icbfs-cli fs). Drives on
// them are package drive/icbfsdrive's.
package icbfs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	core "github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/azurestore"
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
	"github.com/resurgentech/icbfs/internal/storeconfig"
)

// Store is a versioned bucket.
type Store = objstore.Store

// Settings, Info and NamedVersion are the engine's.
type (
	Settings     = core.Settings
	Info         = core.Info
	NamedVersion = core.NamedVersion
)

// Errors the administration calls return.
var (
	ErrNotFound = core.ErrNotFound
	ErrExists   = core.ErrExists
)

// DefaultMaxFileSize is a new filesystem's max file size unless given.
const DefaultMaxFileSize = core.DefaultMaxFileSize

// FormatVersion is the newest object format this build reads and writes:
// a filesystem of a newer one is refused.
const FormatVersion = core.FormatVersion

// StoreConfig says which object store and how to reach it: Kind (KindS3,
// the default, or KindAzure) and that kind's section. Its JSON is
// {"kind": ..., "s3": {...}} or {"kind": "azure", "azure": {...}}; an S3
// section's fields at the top, as configurations had them before, read as
// an S3 store.
type StoreConfig = storeconfig.Config

// S3 is a bucket over the S3 API (AWS S3, MinIO), with versioning on.
type S3 = storeconfig.S3

// Azure is an Azure Blob Storage container, in an account with blob
// versioning on.
type Azure = storeconfig.Azure

// ErrNoStoreKeys is StoreConfig.Check's answer when the store's
// credentials are missing.
var ErrNoStoreKeys = storeconfig.ErrNoKeys

const (
	KindS3    = storeconfig.KindS3
	KindAzure = storeconfig.KindAzure
)

// Open opens c's store.
func Open(c StoreConfig) (Store, error) {
	switch c.Kind {
	case "", KindS3:
		return OpenS3(c.S3), nil
	case KindAzure:
		return OpenAzure(c.Azure)
	}
	return nil, fmt.Errorf("store kind %q: %s or %s", c.Kind, KindS3, KindAzure)
}

// OpenAzure opens an Azure Blob Storage container with the account's key.
func OpenAzure(c Azure) (Store, error) {
	return azurestore.New(c.ServiceURL(), c.Account, c.Key, c.Container)
}

// OpenS3 opens a bucket over the S3 API.
func OpenS3(c S3) Store {
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(c.Endpoint),
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
	})
	return s3store.New(client, c.Bucket)
}

// CreateFilesystem creates a filesystem with every setting given.
func CreateFilesystem(ctx context.Context, store Store, s Settings) (Info, error) {
	return core.CreateFilesystem(ctx, store, s)
}

// DefaultSettings is a filesystem called name with the defaults icbfs-cli
// fs create uses: primary-POSIX, 100 GiB declared, the root mode 0755
// owned by uid and gid 0 (icbfs-cli makes those the caller's). Change
// what you need, then CreateFilesystem.
func DefaultSettings(name string) Settings {
	return core.DefaultSettings(name)
}

// ListFilesystems lists a bucket's filesystems.
func ListFilesystems(ctx context.Context, store Store) ([]Info, error) {
	return core.ListFilesystems(ctx, store)
}

// DescribeFilesystem is one filesystem's settings, with its disk usage.
func DescribeFilesystem(ctx context.Context, store Store, name string) (Info, int64, error) {
	info, err := core.DescribeFilesystem(ctx, store, name)
	if err != nil {
		return Info{}, 0, err
	}
	f := core.New(store, name)
	if err := f.Attach(ctx); err != nil {
		return Info{}, 0, err
	}
	used, err := f.DiskUsage(ctx)
	return info, used, err
}

// Resize changes a filesystem's declared size.
func Resize(ctx context.Context, store Store, name string, size uint64) error {
	return core.Resize(ctx, store, name, size)
}

// Archive makes a filesystem read-only from now on.
func Archive(ctx context.Context, store Store, name string) error {
	return core.Archive(ctx, store, name)
}

// Prune erases an archived filesystem, every version of everything in it.
func Prune(ctx context.Context, store Store, name string) error {
	info, err := core.DescribeFilesystem(ctx, store, name)
	if err != nil {
		return err
	}
	if !info.Archived {
		return ErrNotArchived
	}
	return core.Prune(ctx, store, name)
}

// ErrNotArchived: prune needs an archived filesystem.
var ErrNotArchived = errNotArchived{}

type errNotArchived struct{}

func (errNotArchived) Error() string { return "the filesystem isn't archived" }
