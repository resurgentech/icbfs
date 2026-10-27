package azurestore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/resurgentech/icbfs/internal/objstore"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
)

// Store is Store on an Azure Blob Storage container, in an account
// with blob versioning on (icbfs's past views and trash rest on it). It
// maps S3's model onto Azure's:
//
//   - A delete leaves no delete marker on Azure, and no time of deletion,
//     which past views need. So Delete first writes a marker version (an
//     empty blob with markerKey in its metadata), then deletes the blob: the
//     marker is the newest version, from the time of the delete. A marker
//     that is current (a Delete cut short) reads as no blob.
//   - Undelete can't make an earlier version current again with its own
//     version ID, as S3 can: the newest version is copied to a new current
//     version, and the marker stays in the history between them.
//   - PutParts stages blocks, uploaded or copied on the server from a range
//     of a pinned version (authorized by a short-lived SAS for that version,
//     as Put Block From URL takes no shared key), and commits them under
//     If-Match.
//
// Azure returns metadata names in its own case: they're lowercased on
// reading, as icbfs's names are.
type Store struct {
	client    *container.Client
	cred      *azblob.SharedKeyCredential
	container string
	keyPrefix string // see WithKeyPrefix
}

var _ objstore.Store = (*Store)(nil)

const (
	markerKey = "icbfs_delete_marker"
	// azureBlockSize is the size of the blocks Put streams a body in.
	azureBlockSize = 8 << 20
)

// New is a store on container in the storage account at
// serviceURL (https://ACCOUNT.blob.core.windows.net/, or Azurite's
// http://HOST:PORT/ACCOUNT/), with the account's shared key.
func New(serviceURL, account, key, containerName string) (*Store, error) {
	cred, err := azblob.NewSharedKeyCredential(account, key)
	if err != nil {
		return nil, err
	}
	svc, err := azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	if err != nil {
		return nil, err
	}
	return &Store{client: svc.ServiceClient().NewContainerClient(containerName), cred: cred, container: containerName}, nil
}

// WithKeyPrefix is S3Store.WithKeyPrefix: every key kept under prefix,
// invisibly.
func (s *Store) WithKeyPrefix(prefix string) *Store {
	c := *s
	c.keyPrefix = prefix
	return &c
}

func (s *Store) k(key string) string { return s.keyPrefix + key }

func (s *Store) blob(key string) *blockblob.Client {
	return s.client.NewBlockBlobClient(s.k(key))
}

func (s *Store) version(key, versionID string) (*blockblob.Client, error) {
	return s.blob(key).WithVersionID(versionID)
}

// azureErr maps Azure's not-found and failed-condition errors to the
// sentinels objstore.IsNotFound and objstore.IsPreconditionFailed recognize.
func azureErr(op, key string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound):
		return fmt.Errorf("%s %s: %w (%v)", op, key, objstore.ErrNotFound, err)
	case bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists, bloberror.TargetConditionNotMet):
		return fmt.Errorf("%s %s: %w (%v)", op, key, objstore.ErrPreconditionFailed, err)
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		switch re.StatusCode {
		case 404:
			return fmt.Errorf("%s %s: %w (%v)", op, key, objstore.ErrNotFound, err)
		case 412:
			return fmt.Errorf("%s %s: %w (%v)", op, key, objstore.ErrPreconditionFailed, err)
		}
	}
	return fmt.Errorf("%s %s: %w", op, key, err)
}

func toAzureMeta(md map[string]string) map[string]*string {
	if len(md) == 0 {
		return nil
	}
	out := make(map[string]*string, len(md))
	for k, v := range md {
		out[k] = to.Ptr(v)
	}
	return out
}

func fromAzureMeta(md map[string]*string) map[string]string {
	out := make(map[string]string, len(md))
	for k, v := range md {
		if strings.EqualFold(k, markerKey) {
			continue
		}
		if v != nil {
			out[strings.ToLower(k)] = *v
		}
	}
	return out
}

func isMarker(md map[string]*string) bool {
	for k := range md {
		if strings.EqualFold(k, markerKey) {
			return true
		}
	}
	return false
}

func ifMatch(etag string) *blob.AccessConditions {
	if etag == "" {
		return nil
	}
	return &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: to.Ptr(azcore.ETag(etag))}}
}

func etagString(e *azcore.ETag) string {
	if e == nil {
		return ""
	}
	return string(*e)
}

func (s *Store) Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatchETag string) (*objstore.Object, error) {
	resp, err := s.blob(key).UploadStream(ctx, body, &blockblob.UploadStreamOptions{
		BlockSize:        azureBlockSize,
		Metadata:         toAzureMeta(metadata),
		AccessConditions: ifMatch(ifMatchETag),
	})
	if err != nil {
		if ifMatchETag != "" && s.lostRace(ctx, key, ifMatchETag, err) {
			return nil, fmt.Errorf("put %s: %w (%v)", key, objstore.ErrPreconditionFailed, err)
		}
		return nil, azureErr("put", key, err)
	}
	return s.HeadVersion(ctx, key, deref(resp.VersionID))
}

func (s *Store) PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*objstore.Object, error) {
	// A current delete marker is no blob, but If-None-Match: * sees it.
	if obj, err := s.blob(key).GetProperties(ctx, nil); err == nil && isMarker(obj.Metadata) {
		if err := s.dropCurrentMarker(ctx, key, etagString(obj.ETag)); err != nil {
			return nil, err
		}
	}
	resp, err := s.blob(key).UploadStream(ctx, body, &blockblob.UploadStreamOptions{
		BlockSize: azureBlockSize,
		Metadata:  toAzureMeta(metadata),
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{
			IfNoneMatch: to.Ptr(azcore.ETagAny)}},
	})
	if err != nil {
		return nil, azureErr("put if absent", key, err)
	}
	return s.HeadVersion(ctx, key, deref(resp.VersionID))
}

// lostRace is S3Store.lostRace: a conditional write that failed without
// an answer from the server (a connection cut off) lost its race if key no
// longer matches ifMatch.
func (s *Store) lostRace(ctx context.Context, key, ifMatch string, err error) bool {
	var re *azcore.ResponseError
	if errors.As(err, &re) || ctx.Err() != nil {
		return false
	}
	head, herr := s.Head(ctx, key)
	return herr == nil && head.ETag != ifMatch
}

// dropCurrentMarker finishes a Delete cut short between its two steps.
func (s *Store) dropCurrentMarker(ctx context.Context, key, etag string) error {
	_, err := s.blob(key).Delete(ctx, &blob.DeleteOptions{AccessConditions: ifMatch(etag)})
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ConditionNotMet) {
		return azureErr("delete", key, err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	return s.get(ctx, key, s.blob(key))
}

func (s *Store) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *objstore.Object, error) {
	c, err := s.version(key, versionID)
	if err != nil {
		return nil, nil, err
	}
	return s.get(ctx, key, c)
}

func (s *Store) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	body, obj, err := s.getWith(ctx, key, s.blob(key), &blob.DownloadStreamOptions{AccessConditions: &blob.AccessConditions{
		ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETag(etag))}}})
	if errors.Is(err, errNotModified) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	return body, obj, true, nil
}

// errNotModified is a conditional read's 304, which Azure sends with the
// code ConditionNotMet, as it does a failed write's 412.
var errNotModified = errors.New("not modified")

func (s *Store) get(ctx context.Context, key string, c *blockblob.Client) (io.ReadCloser, *objstore.Object, error) {
	return s.getWith(ctx, key, c, nil)
}

func (s *Store) getWith(ctx context.Context, key string, c *blockblob.Client, opts *blob.DownloadStreamOptions) (io.ReadCloser, *objstore.Object, error) {
	resp, err := c.DownloadStream(ctx, opts)
	if re := (*azcore.ResponseError)(nil); errors.As(err, &re) && re.StatusCode == 304 {
		return nil, nil, errNotModified
	}
	if err != nil {
		return nil, nil, azureErr("get", key, err)
	}
	if isMarker(resp.Metadata) {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("get %s: %w (deleted)", key, objstore.ErrNotFound)
	}
	return resp.Body, &objstore.Object{Key: key, VersionID: deref(resp.VersionID), ETag: etagString(resp.ETag),
		Size: deref(resp.ContentLength), LastModified: deref(resp.LastModified), Metadata: fromAzureMeta(resp.Metadata)}, nil
}

func (s *Store) Head(ctx context.Context, key string) (*objstore.Object, error) {
	return s.head(ctx, key, s.blob(key))
}

func (s *Store) HeadVersion(ctx context.Context, key, versionID string) (*objstore.Object, error) {
	if versionID == "" {
		return s.Head(ctx, key)
	}
	c, err := s.version(key, versionID)
	if err != nil {
		return nil, err
	}
	return s.head(ctx, key, c)
}

func (s *Store) head(ctx context.Context, key string, c *blockblob.Client) (*objstore.Object, error) {
	resp, err := c.GetProperties(ctx, nil)
	if err != nil {
		return nil, azureErr("head", key, err)
	}
	if isMarker(resp.Metadata) {
		return nil, fmt.Errorf("head %s: %w (deleted)", key, objstore.ErrNotFound)
	}
	return &objstore.Object{Key: key, VersionID: deref(resp.VersionID), ETag: etagString(resp.ETag),
		Size: deref(resp.ContentLength), LastModified: deref(resp.LastModified), Metadata: fromAzureMeta(resp.Metadata)}, nil
}

func (s *Store) History(ctx context.Context, key string) ([]objstore.Object, error) {
	all, err := s.listVersions(ctx, key)
	if err != nil {
		return nil, err
	}
	var out []objstore.Object
	for _, o := range all {
		if o.Key == key {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *Store) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	return s.listVersions(ctx, prefix)
}

// listVersions lists every version under prefix: per key, newest first
// (version IDs are timestamps, so their order is the versions'), with
// markers as delete markers, and a key with no current version whose
// newest version isn't a marker (deleted by something other than Delete)
// given one, dated as that version.
func (s *Store) listVersions(ctx context.Context, prefix string) ([]objstore.Object, error) {
	type item struct {
		obj     objstore.Object
		current bool
	}
	byKey := map[string][]item{}
	var keys []string
	pager := s.client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     to.Ptr(s.k(prefix)),
		Include:    container.ListBlobsInclude{Versions: true, Metadata: true},
		MaxResults: to.Ptr(objstore.HistoryPageSize),
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureErr("list versions", prefix, err)
		}
		for _, b := range page.Segment.BlobItems {
			key := strings.TrimPrefix(deref(b.Name), s.keyPrefix)
			o := objstore.Object{Key: key, VersionID: deref(b.VersionID), Deleted: isMarker(b.Metadata)}
			if b.Properties != nil {
				o.ETag = etagString(b.Properties.ETag)
				o.Size = deref(b.Properties.ContentLength)
				o.LastModified = deref(b.Properties.LastModified)
			}
			if o.Deleted {
				o.ETag, o.Size = "", 0
			}
			if _, seen := byKey[key]; !seen {
				keys = append(keys, key)
			}
			byKey[key] = append(byKey[key], item{o, deref(b.IsCurrentVersion)})
		}
	}
	sort.Strings(keys)
	var out []objstore.Object
	for _, key := range keys {
		items := byKey[key]
		sort.Slice(items, func(i, j int) bool { return items[i].obj.VersionID > items[j].obj.VersionID })
		anyCurrent := false
		for _, it := range items {
			anyCurrent = anyCurrent || it.current
		}
		if !anyCurrent && len(items) > 0 && !items[0].obj.Deleted {
			out = append(out, objstore.Object{Key: key, Deleted: true, LastModified: items[0].obj.LastModified})
		}
		for _, it := range items {
			out = append(out, it.obj)
		}
	}
	return out, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	// The marker records when; deleting the blob then makes it, the newest
	// version, no longer current. If another write lands between the two,
	// the blob isn't deleted: the write came after the delete.
	resp, err := s.blob(key).Upload(ctx, nopSeekCloser{bytes.NewReader(nil)}, &blockblob.UploadOptions{
		Metadata: map[string]*string{markerKey: to.Ptr("1")},
	})
	if err != nil {
		return azureErr("delete", key, err)
	}
	return s.dropCurrentMarker(ctx, key, etagString(resp.ETag))
}

// Undelete copies key's newest version back as its current version, if
// key is deleted. Azure can't make an earlier version current with its
// own version ID, as S3 can; the marker stays in the history, so a view of
// the time between it and the copy shows the deletion it was.
func (s *Store) Undelete(ctx context.Context, key string) error {
	history, err := s.History(ctx, key)
	if err != nil {
		return err
	}
	if len(history) == 0 || !history[0].Deleted {
		return nil
	}
	for _, v := range history[1:] {
		if !v.Deleted {
			return s.promote(ctx, key, v.VersionID)
		}
	}
	return nil // nothing before the deletion to bring back
}

// Erase deletes one version of key by its ID: a content version or a
// deletion's marker. Azure deletes only previous versions by ID; the
// current one is Delete's.
func (s *Store) Erase(ctx context.Context, key, versionID string) error {
	if versionID == "" {
		return nil // a deletion Azure dated itself has no entry of its own
	}
	if cur, err := s.blob(key).GetProperties(ctx, nil); err == nil && deref(cur.VersionID) == versionID && !isMarker(cur.Metadata) {
		return fmt.Errorf("erase %s: version %s is current", key, versionID)
	}
	c, err := s.version(key, versionID)
	if err != nil {
		return err
	}
	if _, err := c.Delete(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return azureErr("erase", key, err)
	}
	return nil
}

// EraseAll ends every current blob under prefix, which makes it a previous
// version, then deletes every version.
func (s *Store) EraseAll(ctx context.Context, prefix string) error {
	type entry struct{ name, version string }
	var current, versions []entry
	pager := s.client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     to.Ptr(s.k(prefix)),
		Include:    container.ListBlobsInclude{Versions: true},
		MaxResults: to.Ptr(objstore.HistoryPageSize),
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return azureErr("erase all", prefix, err)
		}
		for _, b := range page.Segment.BlobItems {
			e := entry{deref(b.Name), deref(b.VersionID)}
			if deref(b.IsCurrentVersion) {
				current = append(current, e)
			}
			versions = append(versions, e)
		}
	}
	for _, e := range current {
		if _, err := s.client.NewBlobClient(e.name).Delete(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
			return azureErr("erase all", e.name, err)
		}
	}
	for _, e := range versions {
		c, err := s.client.NewBlobClient(e.name).WithVersionID(e.version)
		if err != nil {
			return err
		}
		if _, err := c.Delete(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
			return azureErr("erase all", e.name, err)
		}
	}
	return nil
}

// PartLimits are Azure's block limits: up to 4000 MiB a block, 50,000
// blocks, and no minimum.
func (s *Store) PartLimits() objstore.PartLimits {
	return objstore.PartLimits{Min: 1, Max: 4000 << 20, Count: 50000}
}

// promote makes version versionID of key current again, as a new version
// with its content and metadata.
func (s *Store) promote(ctx context.Context, key, versionID string) error {
	src, err := s.version(key, versionID)
	if err != nil {
		return err
	}
	resp, err := s.blob(key).StartCopyFromURL(ctx, src.URL(), nil)
	if err != nil {
		return azureErr("undelete", key, err)
	}
	for status := resp.CopyStatus; status != nil && *status == blob.CopyStatusTypePending; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		p, err := s.blob(key).GetProperties(ctx, nil)
		if err != nil {
			return azureErr("undelete", key, err)
		}
		status = p.CopyStatus
	}
	return nil
}

func (s *Store) PutParts(ctx context.Context, key string, parts []objstore.Part, metadata map[string]string, ifMatchETag string) (*objstore.Object, error) {
	bb := s.blob(key)
	ids := make([]string, len(parts))
	for i, p := range parts {
		ids[i] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("icbfs-part-%06d", i)))
		if p.Body != nil {
			if _, err := bb.StageBlock(ctx, ids[i], nopSeekCloser{p.Body}, nil); err != nil {
				return nil, azureErr("put parts", key, err)
			}
			continue
		}
		src, err := s.versionURL(key, p.CopyVersion)
		if err != nil {
			return nil, err
		}
		if _, err := bb.StageBlockFromURL(ctx, ids[i], src, &blockblob.StageBlockFromURLOptions{
			Range: blob.HTTPRange{Offset: p.CopyOffset, Count: p.Size},
		}); err != nil {
			return nil, azureErr("put parts", key, err)
		}
	}
	resp, err := bb.CommitBlockList(ctx, ids, &blockblob.CommitBlockListOptions{
		Metadata:         toAzureMeta(metadata),
		AccessConditions: ifMatch(ifMatchETag),
	})
	if err != nil {
		return nil, azureErr("put parts", key, err)
	}
	return s.HeadVersion(ctx, key, deref(resp.VersionID))
}

// versionURL is a URL for reading one version of key, with a SAS good for
// an hour: Put Block From URL authorizes its source by the URL alone.
func (s *Store) versionURL(key, versionID string) (string, error) {
	q, err := sas.BlobSignatureValues{
		Protocol:      sas.ProtocolHTTPSandHTTP,
		ExpiryTime:    time.Now().UTC().Add(time.Hour),
		Permissions:   (&sas.BlobPermissions{Read: true}).String(),
		ContainerName: s.container,
		BlobName:      s.k(key),
		BlobVersion:   versionID,
	}.SignWithSharedKey(s.cred)
	if err != nil {
		return "", err
	}
	c, err := s.version(key, versionID)
	if err != nil {
		return "", err
	}
	return c.URL() + "&" + q.Encode(), nil
}

func (s *Store) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	var out []objstore.Object
	pager := s.client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:  to.Ptr(s.k(prefix)),
		Include: container.ListBlobsInclude{Metadata: true},
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureErr("list", prefix, err)
		}
		for _, b := range page.Segment.BlobItems {
			if isMarker(b.Metadata) {
				continue
			}
			o := objstore.Object{Key: strings.TrimPrefix(deref(b.Name), s.keyPrefix)}
			if b.Properties != nil {
				o.Size = deref(b.Properties.ContentLength)
			}
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *Store) ServerTime(ctx context.Context) (time.Time, error) {
	resp, err := s.client.GetProperties(ctx, nil)
	if err != nil {
		return time.Time{}, azureErr("server time", s.container, err)
	}
	if resp.Date == nil {
		return time.Time{}, errors.New("server time: no Date in the response")
	}
	return resp.Date.UTC(), nil
}

// nopSeekCloser gives an io.ReadSeeker the Close the SDK's uploads take.
type nopSeekCloser struct{ io.ReadSeeker }

func (nopSeekCloser) Close() error { return nil }

// deref is *p, or the zero value for nil.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
