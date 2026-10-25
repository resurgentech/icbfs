// Package objstore is the object store as icbfs uses it: whole objects
// written and read, conditionally on their current version, every key's
// history kept (versions and deletions), and listing by prefix. Everything
// else in icbfs is written against Store. S3Store (AWS S3, MinIO) and
// AzureStore (Azure Blob Storage) implement it, each with its store's own
// means; AsOf is a read-only view of a Store at a past time.
package objstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound and ErrPreconditionFailed are what a store's errors wrap for
// "no such object" and "the condition didn't hold"; IsNotFound and
// IsPreconditionFailed check for them.
var (
	ErrNotFound           = errors.New("not found")
	ErrPreconditionFailed = errors.New("precondition failed")
)

// IsNotFound reports whether err says there is no such object (or
// version).
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsPreconditionFailed reports whether err is a conditional write's
// refusal: the object wasn't the version the write was conditional on.
// It's the signal a compare-and-swap retry loop watches for.
func IsPreconditionFailed(err error) bool { return errors.Is(err, ErrPreconditionFailed) }

// Object is one entry of a key's history: a version, or a deletion.
type Object struct {
	Key string
	// VersionID names the entry in the key's history. ETag names its
	// content: a write conditional on an ETag lands only while the key's
	// current version has it.
	VersionID    string
	ETag         string
	Size         int64
	LastModified time.Time
	Metadata     map[string]string

	// Deleted marks an entry recording a deletion: the key didn't exist
	// from LastModified until its next version. It has no content.
	Deleted bool
}

type declaredKey struct{}

// WithDeclaredTime says the versions written under ctx came to be at t,
// not when the store writes them: a mirror copying what happened at its
// source at t. The store itself doesn't record it; a Recording store
// notes each version written (Recorder), and the caller dates them in
// the item's .version object (AddDates), which views of the past go by.
func WithDeclaredTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, declaredKey{}, t)
}

// WithoutDeclaredTime is ctx with no declared time: for a write under it
// that happens now (a mark that something changed, a .version record).
func WithoutDeclaredTime(ctx context.Context) context.Context {
	return context.WithValue(ctx, declaredKey{}, nil)
}

// DeclaredTime is ctx's declared time, if it has one.
func DeclaredTime(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(declaredKey{}).(time.Time)
	return t, ok
}

// Part is one part of a PutParts write: Size bytes, either uploaded from
// Body or, when Body is nil, copied on the server from [CopyOffset,
// CopyOffset+Size) of version CopyVersion of the object being written.
type Part struct {
	Size        int64
	Body        io.ReadSeeker
	CopyVersion string
	CopyOffset  int64
}

// PartLimits are a store's limits on a PutParts write: every part but the
// last is Min to Max bytes, and there are at most Count parts.
type PartLimits struct {
	Min, Max int64
	Count    int64
}

// Store is the object-store primitive set icbfs's core is built on.
//
// Metadata names are lowercase letters, digits and underscores, which
// every store takes as they are.
type Store interface {
	// Put writes body as the object under key, replacing its content and
	// metadata, as a new version. A non-empty ifMatch makes the write
	// conditional on the current version's ETag (compare-and-swap).
	Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatch string) (*Object, error)

	// PutIfAbsent is Put only if key has no current version. If it has
	// one, it returns an error IsPreconditionFailed recognizes, and writes
	// nothing.
	PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*Object, error)

	// PutParts is Put of an object assembled from parts, in order: for an
	// object too big for one write, or to rewrite a big object without
	// resending what didn't change, since a part can be copied on the
	// server from a range of an earlier version of key itself. It is
	// conditional on ifMatch as Put is, checked when the parts are
	// assembled, and all or nothing. Parts keep to PartLimits.
	PutParts(ctx context.Context, key string, parts []Part, metadata map[string]string, ifMatch string) (*Object, error)

	// PartLimits are this store's limits on PutParts.
	PartLimits() PartLimits

	// Get returns key's current content and metadata, and Head just the
	// metadata. A deleted key is ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, *Object, error)
	Head(ctx context.Context, key string) (*Object, error)

	// GetVersion and HeadVersion are Get and Head for one version of key,
	// current or not.
	GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *Object, error)
	HeadVersion(ctx context.Context, key, versionID string) (*Object, error)

	// Delete ends key's current version, recording the deletion, with its
	// time, in key's history. Nothing is lost: Undelete brings it back.
	Delete(ctx context.Context, key string) error

	// Undelete makes key's newest version current again, if key is deleted;
	// otherwise it does nothing. The deletion stays in the history or goes,
	// as the store does it; either way a view of any past time is the same.
	Undelete(ctx context.Context, key string) error

	// History returns key's history, newest first: its versions and its
	// deletions. HistoryUnder is History for every key starting with
	// prefix, keys in order, with no Metadata filled in.
	History(ctx context.Context, key string) ([]Object, error)
	HistoryUnder(ctx context.Context, prefix string) ([]Object, error)

	// Erase permanently removes one entry of key's history, a version or a
	// deletion, that isn't key's current version. EraseAll permanently
	// removes every key starting with prefix, and all its history: the only
	// operations that lose anything.
	Erase(ctx context.Context, key, versionID string) error
	EraseAll(ctx context.Context, prefix string) error

	// List returns every key starting with prefix that has a current
	// version, with only Key and Size filled in: what df's "Used" needs.
	// It costs one listing request per thousand or so objects; no store
	// has anything faster.
	List(ctx context.Context, prefix string) ([]Object, error)

	// ServerTime is the store's clock now (whole seconds), from a
	// response's Date header. Lock leases are anchored to it rather than to
	// the caller's clock, which could be skewed against other clients'.
	ServerTime(ctx context.Context) (time.Time, error)
}

// HistoryPageSize is how many entries a history or list request asks for
// at a time (a var so tests can make pages small).
var HistoryPageSize int32 = 1000
