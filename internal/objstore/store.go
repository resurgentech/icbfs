// Package objstore wraps the three backends icbfs targets (AWS S3, Azure
// Blob Storage, and MinIO's S3 API) behind one interface, exposing only
// the primitives the rest of icbfs is built on: whole-object put/get,
// metadata-only updates, conditional writes, version listing, and
// prefix-scoped listing.
//
// Store (below) is already that wrapper layer — S3Store (shared by both
// real AWS S3 and MinIO, which speaks the same API) is the only
// implementation that exists today, but nothing outside this package's
// own S3Store knows that: every call site throughout icbfs (master.go,
// lock.go, filesystem.go, ...) is already written against Store, never
// against s3.Client or any AWS-specific type directly. Adding a real
// AzureStore means writing one new file that implements this same
// interface — it does not mean inventing a new abstraction layer at
// that point, because this one is it.
//
// Each method below carries a comment sketching the Azure Blob Storage
// SDK for Go (`github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`)
// call a real AzureStore would make, for whoever picks that up — but
// unlike every S3-side primitive in this package (each verified against
// real MinIO before being trusted, see ASSUMPTIONS.md throughout), this
// is NOT verified against a real Azure SDK or Storage Account: no
// Azure SDK is installed in this environment and no account exists to
// test against, the same documented-gap treatment ASSUMPTIONS.md's E5
// entry already gives internal/notify/azurecf for the exact same
// reason. Treat these as a starting sketch from training knowledge, to
// be confirmed against the actual installed package source before
// trusting any of it, not as verified fact.
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
	//
	// Sketch for Azure: (*blob.Client).Upload(ctx, body, &blob.UploadOptions{
	// Metadata: metadata, AccessConditions: &blob.AccessConditions{
	// ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: ...}}}),
	// taking the ETag from the returned UploadResponse.
	Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatch string) (*Object, error)

	// Get returns the current body and metadata for key.
	//
	// Sketch for Azure: (*blob.Client).DownloadStream(ctx, nil), reading
	// Body and taking Object fields from the response's ETag/
	// LastModified/Metadata/ContentLength.
	Get(ctx context.Context, key string) (io.ReadCloser, *Object, error)

	// Head returns metadata for key without fetching the body.
	//
	// Sketch for Azure: (*blob.Client).GetProperties(ctx, nil) — Azure's
	// direct equivalent of S3's HeadObject.
	Head(ctx context.Context, key string) (*Object, error)

	// UpdateMetadata replaces an object's metadata without rewriting its
	// body. On a versioned bucket this creates a new version, same as a
	// content write would. If ifMatch is non-empty, the update is
	// conditioned on the current object's ETag matching it, same as Put.
	//
	// Sketch for Azure: (*blob.Client).SetMetadata(ctx, metadata, &blob.
	// SetMetadataOptions{AccessConditions: ...}) — notably a *real*,
	// direct metadata-only operation, unlike S3Store's own
	// implementation of this method (S3 has no such primitive and goes
	// through CopyObject with a REPLACE metadata directive instead; check
	// S3Store.UpdateMetadata's own doc comment for that detail before
	// assuming Azure needs the same workaround — it shouldn't).
	UpdateMetadata(ctx context.Context, key string, metadata map[string]string, ifMatch string) (*Object, error)

	// ListVersions returns every version of key, newest first.
	//
	// Sketch for Azure: (*container.Client).NewListBlobsFlatPager(&container.
	// ListBlobsFlatOptions{Prefix: &key, Include: container.ListBlobsInclude{
	// Versions: true}}), filtered to exact-name matches (a prefix listing
	// scoped to one blob name), each page's Segment.BlobItems giving
	// VersionID/Properties.Etag/LastModified per entry, newest-first
	// ordering unconfirmed (would need checking against this behavior for
	// real, not assumed from the S3 side's already-confirmed ordering).
	ListVersions(ctx context.Context, key string) ([]Object, error)

	// Delete removes key (creating a delete marker on a versioned bucket).
	// If ifMatch is non-empty, the delete is conditioned on the current
	// object's ETag matching it.
	//
	// Sketch for Azure: (*blob.Client).Delete(ctx, &blob.DeleteOptions{
	// AccessConditions: ...}). Flagged specifically, not just sketched:
	// Azure Blob versioning's delete semantics are not confirmed to work
	// the same way as S3's delete-marker model this method's own doc
	// comment describes — Azure may simply demote the current version
	// without leaving an explicit "delete marker" entry the way S3 does.
	// Anything in icbfs that depends on ListVersions/Get being able to
	// tell "deleted" apart from "never existed" after a Delete would need
	// this checked against a real account before trusting it, not
	// inferred from the S3 behavior already verified elsewhere in this
	// package.
	Delete(ctx context.Context, key string, ifMatch string) error

	// PutIfAbsent writes body as the object body under key only if no
	// object currently exists there — a true create-if-absent CAS
	// primitive (unlike Put's ifMatch, which has no way to express
	// "must not exist" when there is no prior ETag to condition on).
	// If an object already exists at key, it returns the error Put
	// returns for a failed If-Match (satisfies IsPreconditionFailed),
	// and body is not written. Confirmed against real MinIO, not
	// assumed: PutObject's IfNoneMatch: "*" is honored and reports 412.
	//
	// Sketch for Azure: same Upload call as Put, with AccessConditions.
	// ModifiedAccessConditions.IfNoneMatch set to azcore.ETagAny (Azure's
	// "*" wildcard) — structurally the same mechanism S3 uses, but this
	// specific combination is unverified here the way PutIfAbsent's own
	// 412-on-conflict behavior was confirmed against real MinIO.
	PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*Object, error)

	// ListByPrefix returns every current object whose key starts with
	// prefix, paginating internally. Only Key and Size are populated —
	// this is the lightweight listing operation ARCHITECTURE.md's
	// Multiple filesystems per bucket section describes for du/df's
	// "Used" and for Prune: no object bodies are fetched, just listing
	// metadata, but it's still O(number of matching objects/pages), not
	// O(1) — there is no faster primitive on any of the three target
	// backends (see that section for why).
	//
	// Sketch for Azure: (*container.Client).NewListBlobsFlatPager(&container.
	// ListBlobsFlatOptions{Prefix: &prefix}), draining every page via
	// .NextPage(ctx)/.More() and taking Key/Size from each BlobItem's
	// Name/Properties.ContentLength.
	ListByPrefix(ctx context.Context, prefix string) ([]Object, error)

	// ServerTime returns the object store's current clock, observed
	// via a real request's raw HTTP response — not a typed field like
	// an Object's LastModified (which only ever says when some
	// specific object was last written, never "what time is it right
	// now"). Used to anchor lease expiry (ARCHITECTURE.md's Locking
	// section) to the store's clock instead of the calling client's,
	// which could be skewed relative to other clients.
	//
	// Sketch for Azure: every Azure Storage REST response also carries a
	// standard HTTP Date header, so the same raw-header-capture approach
	// S3Store.ServerTime uses (smithy-go's deserialize middleware) has an
	// Azure SDK equivalent: azcore's runtime.WithCaptureResponse(ctx,
	// &resp) stashes the raw *http.Response for whatever call wraps it
	// (e.g. a GetProperties against the container), letting this read
	// resp.Header.Get("Date") the same way. Unverified here the way
	// S3Store.ServerTime's approach was confirmed against real MinIO
	// before being trusted (see ASSUMPTIONS.md's B2 entry).
	ServerTime(ctx context.Context) (time.Time, error)
}
