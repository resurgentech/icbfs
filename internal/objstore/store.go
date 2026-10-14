// Package objstore is the object store as icbfs uses it: whole-object put
// and get, conditional writes, version listing, and prefix listing.
// Everything else in icbfs is written against Store; S3Store (AWS S3 and
// MinIO) is the one implementation, and AsOf is a read-only view of a Store
// at a past time.
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

	// DeleteMarker is set on a version listing's entry for a delete: the
	// key didn't exist from LastModified until its next version.
	DeleteMarker bool
}

// Part is one part of a PutParts write: Size bytes, either uploaded from
// Body or, when Body is nil, copied from [CopyOffset, CopyOffset+Size) of
// version CopyVersion of the object being written.
type Part struct {
	Size        int64
	Body        io.ReadSeeker
	CopyVersion string
	CopyOffset  int64
}

// Multipart limits, S3's (MinIO's are the same): every part but the
// last is MinPartSize to MaxPartSize bytes, and an object has at most
// MaxParts parts.
const (
	MinPartSize = 5 << 20
	MaxPartSize = 5 << 30
	MaxParts    = 10000
)

// Store is the object-store primitive set icbfs's core is built on.
type Store interface {
	// Put writes body as the object under key, replacing its content and
	// metadata. A non-empty ifMatch makes the write conditional on the
	// current ETag (compare-and-swap); empty means unconditional.
	Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatch string) (*Object, error)

	// Get returns key's current body and metadata.
	Get(ctx context.Context, key string) (io.ReadCloser, *Object, error)

	// Head returns key's metadata without its body.
	Head(ctx context.Context, key string) (*Object, error)

	// UpdateMetadata replaces an object's metadata without rewriting its
	// body (a new version on a versioned bucket), conditional on ifMatch as
	// Put is.
	UpdateMetadata(ctx context.Context, key string, metadata map[string]string, ifMatch string) (*Object, error)

	// ListVersions returns every version of key, newest first, delete
	// markers included (DeleteMarker set). Newest first is the store's own
	// order, not a re-sort by LastModified, which would scramble versions
	// written within the same timestamp.
	ListVersions(ctx context.Context, key string) ([]Object, error)

	// ListAllVersions is ListVersions for every key starting with prefix:
	// each key's history, newest first, delete markers included, keys in
	// listing order. Only listing fields are filled in (no Metadata).
	ListAllVersions(ctx context.Context, prefix string) ([]Object, error)

	// GetVersion and HeadVersion are Get and Head for one specific
	// version of key, current or not.
	GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *Object, error)
	HeadVersion(ctx context.Context, key, versionID string) (*Object, error)

	// Delete removes key, leaving a delete marker on a versioned bucket. A
	// non-empty ifMatch makes it conditional, but MinIO doesn't enforce
	// If-Match on deletes, so nothing relies on that.
	Delete(ctx context.Context, key string, ifMatch string) error

	// DeleteVersion permanently removes one version of key — the only
	// truly destructive operation here: Delete leaves history alone and
	// adds a delete marker, while this erases a version, or, given a
	// delete marker's version ID, removes the marker (undoing a Delete:
	// the version before it is current again).
	DeleteVersion(ctx context.Context, key, versionID string) error

	// PutIfAbsent writes body under key only if nothing is there (If-None-
	// Match: *). If something is, it returns the error a failed If-Match
	// does (IsPreconditionFailed), and writes nothing.
	PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*Object, error)

	// PutParts writes key as one object assembled from parts, in order: a
	// multipart upload, for objects too big for one Put, or to rewrite a
	// big object without resending what didn't change, since a part can be
	// copied on the server from a byte range of an earlier version of key
	// itself.
	//
	// It is conditional on ifMatch as Put is, checked when the parts are
	// assembled, and all or nothing: on any error the upload is aborted and
	// key is unchanged. Every part but the last is MinPartSize to
	// MaxPartSize bytes, and there are at most MaxParts. A copied range
	// comes from the pinned version even if key has moved on since.
	PutParts(ctx context.Context, key string, parts []Part, metadata map[string]string, ifMatch string) (*Object, error)

	// ListByPrefix returns every current object whose key starts with
	// prefix, with only Key and Size filled in: what df's "Used" and Prune
	// need. It costs one listing page per thousand objects; no supported
	// store has anything faster.
	ListByPrefix(ctx context.Context, prefix string) ([]Object, error)

	// ServerTime is the store's clock now, from a response's HTTP Date
	// header (whole seconds). Lock leases are anchored to it rather than to
	// the caller's clock, which could be skewed against other clients'.
	ServerTime(ctx context.Context) (time.Time, error)
}
