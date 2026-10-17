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

// S3 is how to reach a bucket over the S3 API (AWS S3, MinIO), or with
// Type "azure" an Azure Blob Storage container: Endpoint the service URL
// (https://ACCOUNT.blob.core.windows.net/), Bucket the container,
// AccessKey the account name and SecretKey the account key.
type S3 struct {
	Type      string `json:"type,omitempty"` // "" or "s3", or "azure"
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// Open opens c's store, of either type (see S3).
func Open(c S3) (Store, error) {
	switch c.Type {
	case "", "s3":
		return OpenS3(c), nil
	case "azure":
		return objstore.NewAzureStore(c.Endpoint, c.AccessKey, c.SecretKey, c.Bucket)
	}
	return nil, fmt.Errorf("store type %q: s3 or azure", c.Type)
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
	return objstore.NewS3Store(client, c.Bucket)
}

// CreateFilesystem creates a filesystem with every setting given.
func CreateFilesystem(ctx context.Context, store Store, s Settings) (Info, error) {
	return core.CreateFilesystem(ctx, store, s)
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
