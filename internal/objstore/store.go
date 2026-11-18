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
// CopyOffset+Size) of version CopyVersion of CopyKey, or of the object
// being written when CopyKey is empty. A copy from a key or version that
// isn't there fails the whole write with an error IsNotFound recognizes.
type Part struct {
	Size        int64
	Body        io.ReadSeeker
	CopyKey     string
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
	// server from a range of an earlier version of key itself, or of
	// another key (Part.CopyKey). It is
	// conditional on ifMatch as Put is, checked when the parts are
	// assembled, and all or nothing. Parts keep to PartLimits.
	PutParts(ctx context.Context, key string, parts []Part, metadata map[string]string, ifMatch string) (*Object, error)

	// PartLimits are this store's limits on PutParts.
	PartLimits() PartLimits

	// Get returns key's current content and metadata, and Head just the
	// metadata. A deleted key is ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, *Object, error)
	Head(ctx context.Context, key string) (*Object, error)

	// GetIfChanged is Get unless key's current version still has ETag
	// etag: then changed is false and nothing else comes back (the
	// store's 304), so a caller holding a copy checks it in one request
	// that carries no body.
	GetIfChanged(ctx context.Context, key, etag string) (body io.ReadCloser, obj *Object, changed bool, err error)

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
	// operations that lose anything. EraseAll promises no order among the
	// keys or their versions, and a version written while it runs may
	// survive it: a caller that needs one key's history gone before
	// another's erases that key first.
	Erase(ctx context.Context, key, versionID string) error
	EraseAll(ctx context.Context, prefix string) error

	// List returns every key starting with prefix that has a current
	// version, with only Key and Size filled in (and LastModified, by
	// S3Store and AzureStore): what df's "Used" needs.
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

// PageLister is a Store that lists by prefix a page at a time: ListPages
// calls fn with each page of what List returns, in order, no page longer
// than HistoryPageSize, and stops at fn's first error, returning it. A
// caller that only folds the listing (a sum) holds one page, not all of
// it. S3Store and AzureStore have it; a wrapper of a Store does not
// unless it says so.
type PageLister interface {
	ListPages(ctx context.Context, prefix string, fn func([]Object) error) error
}

// VersionsSincer is a Store that reads one key's versions back only to a
// time: VersionsSince returns key's versions (not its deletions) dated at
// or after since, newest first, with no Metadata filled in. It lists a page
// (HistoryPageSize) at a time, holds only what it returns, and ends the
// listing as soon as the store's order says no later page holds more:
// S3, at the first version of key older than since or the first entry of
// another key (key's companions, key.metadata and the like, sort after
// it); Azure, whose versions of one name are not listed newest first, at
// the first entry of another name. S3Store and AzureStore have it; a
// wrapper of a Store does not unless it says so (T82-10 round 4).
type VersionsSincer interface {
	VersionsSince(ctx context.Context, key string, since time.Time) ([]Object, error)
}

// HistorySampler is a Store that reads the first entries of a prefix's
// history in one request: HistoryUnderFirst returns up to n (at most
// HistoryPageSize) of the versions and deletions HistoryUnder lists, from
// the first keys in the store's order, with no Metadata filled in and no
// order promised among one key's entries. It answers "is anything here, or
// was it ever" without listing all of it. S3Store and AzureStore have it; a
// wrapper of a Store does not unless it says so.
type HistorySampler interface {
	HistoryUnderFirst(ctx context.Context, prefix string, n int32) ([]Object, error)
}

// HistoryUnderFirst is s's HistoryUnderFirst when it has one, and the first
// n entries of HistoryUnder when it does not (which lists all of it).
func HistoryUnderFirst(ctx context.Context, s Store, prefix string, n int32) ([]Object, error) {
	n = max(1, min(n, HistoryPageSize))
	if hs, ok := s.(HistorySampler); ok {
		return hs.HistoryUnderFirst(ctx, prefix, n)
	}
	all, err := s.HistoryUnder(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if int32(len(all)) > n {
		all = all[:n]
	}
	return all, nil
}

// VersionsSince is s's VersionsSince when it has one, and History's
// versions dated at or after since when it does not.
func VersionsSince(ctx context.Context, s Store, key string, since time.Time) ([]Object, error) {
	if vs, ok := s.(VersionsSincer); ok {
		return vs.VersionsSince(ctx, key, since)
	}
	hist, err := s.History(ctx, key)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, v := range hist {
		if v.Deleted {
			continue
		}
		if v.LastModified.Before(since) {
			break // newest first: the rest are older
		}
		out = append(out, v)
	}
	return out, nil
}

// ListEach is List a page at a time when s is a PageLister, and List's
// whole answer as one page when it is not. fn may keep the slice it is
// given.
func ListEach(ctx context.Context, s Store, prefix string, fn func([]Object) error) error {
	if pl, ok := s.(PageLister); ok {
		return pl.ListPages(ctx, prefix, fn)
	}
	objs, err := s.List(ctx, prefix)
	if err != nil || len(objs) == 0 {
		return err
	}
	return fn(objs)
}
