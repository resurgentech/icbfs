// Package objstore wraps the two backends icbfs targets (MinIO's S3 API and
// Azure Blob Storage) behind one interface, exposing only the primitives the
// rest of icbfs is built on: whole-object put/get, metadata-only updates,
// conditional writes, and version listing.
package objstore

import (
	"context"
	"io"
	"time"
)

// Object describes one version of a stored blob.
type Object struct {
	Key          string
	VersionID    string
	ETag         string
	Size         int64
	LastModified time.Time
	Metadata     map[string]string
}

// Store is the object-store primitive set icbfs's core is built on.
type Store interface {
	// Put writes body as the object body under key, replacing any existing
	// content and metadata. If ifMatch is non-empty, the write is
	// conditioned on the current object's ETag matching it (compare-and-swap);
	// an empty ifMatch means an unconditional write.
	Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatch string) (*Object, error)

	// Get returns the current body and metadata for key.
	Get(ctx context.Context, key string) (io.ReadCloser, *Object, error)

	// Head returns metadata for key without fetching the body.
	Head(ctx context.Context, key string) (*Object, error)

	// UpdateMetadata replaces an object's metadata without rewriting its
	// body. On a versioned bucket this creates a new version, same as a
	// content write would. If ifMatch is non-empty, the update is
	// conditioned on the current object's ETag matching it, same as Put.
	UpdateMetadata(ctx context.Context, key string, metadata map[string]string, ifMatch string) (*Object, error)

	// ListVersions returns every version of key, newest first.
	ListVersions(ctx context.Context, key string) ([]Object, error)

	// Delete removes key (creating a delete marker on a versioned bucket).
	// If ifMatch is non-empty, the delete is conditioned on the current
	// object's ETag matching it.
	Delete(ctx context.Context, key string, ifMatch string) error
}
