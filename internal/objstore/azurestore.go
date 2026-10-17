package objstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

// AzureStore is Store on an Azure Blob Storage container, in an account
// with blob versioning on (icbfs's past views and trash rest on it). It
// maps S3's model onto Azure's:
//
//   - A delete leaves no delete marker on Azure, and no time of deletion,
//     which past views need. So Delete first writes a marker version (an
//     empty blob with markerKey in its metadata), then deletes the blob: the
//     marker is the newest version, from the time of the delete. A marker
//     that is current (a Delete cut short) reads as no blob.
//   - Removing a delete marker (undeleting) can't make an earlier version
//     current again with its own version ID, as on S3: the version before
//     the marker is copied to a new current version. History keeps both,
//     so a past view between them sees the same content either way.
//   - PutParts stages blocks, uploaded or copied on the server from a range
//     of a pinned version (authorized by a short-lived SAS for that version,
//     as Put Block From URL takes no shared key), and commits them under
//     If-Match.
//
// Metadata keys must be C# identifiers on Azure; a '-' is stored as
// metaDash and turned back on reading.
type AzureStore struct {
	client    *container.Client
	cred      *azblob.SharedKeyCredential
	container string
	keyPrefix string // see WithKeyPrefix
}

var _ Store = (*AzureStore)(nil)

const (
	markerKey = "icbfs_delete_marker"
	metaDash  = "_x2d_"
	// azureBlockSize is the size of the blocks Put streams a body in.
	azureBlockSize = 8 << 20
)

// NewAzureStore is a store on container in the storage account at
// serviceURL (https://ACCOUNT.blob.core.windows.net/, or Azurite's
// http://HOST:PORT/ACCOUNT/), with the account's shared key.
func NewAzureStore(serviceURL, account, key, containerName string) (*AzureStore, error) {
	cred, err := azblob.NewSharedKeyCredential(account, key)
	if err != nil {
		return nil, err
	}
	svc, err := azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	if err != nil {
		return nil, err
	}
	return &AzureStore{client: svc.ServiceClient().NewContainerClient(containerName), cred: cred, container: containerName}, nil
}

// WithKeyPrefix is S3Store.WithKeyPrefix: every key kept under prefix,
// invisibly.
func (s *AzureStore) WithKeyPrefix(prefix string) *AzureStore {
	c := *s
	c.keyPrefix = prefix
	return &c
}

func (s *AzureStore) k(key string) string { return s.keyPrefix + key }

func (s *AzureStore) blob(key string) *blockblob.Client {
	return s.client.NewBlockBlobClient(s.k(key))
}

func (s *AzureStore) version(key, versionID string) (*blockblob.Client, error) {
	return s.blob(key).WithVersionID(versionID)
}

// azureErr maps Azure's not-found and failed-condition errors to the
// sentinels IsNotFound and IsPreconditionFailed recognize.
func azureErr(op, key string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ContainerNotFound):
		return fmt.Errorf("%s %s: %w (%v)", op, key, ErrNotFound, err)
	case bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists, bloberror.TargetConditionNotMet):
		return fmt.Errorf("%s %s: %w (%v)", op, key, ErrPreconditionFailed, err)
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		switch re.StatusCode {
		case 404:
			return fmt.Errorf("%s %s: %w (%v)", op, key, ErrNotFound, err)
		case 412:
			return fmt.Errorf("%s %s: %w (%v)", op, key, ErrPreconditionFailed, err)
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
		out[strings.ReplaceAll(k, "-", metaDash)] = to.Ptr(v)
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
			out[strings.ReplaceAll(strings.ToLower(k), metaDash, "-")] = *v
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

func (s *AzureStore) Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatchETag string) (*Object, error) {
	resp, err := s.blob(key).UploadStream(ctx, body, &blockblob.UploadStreamOptions{
		BlockSize:        azureBlockSize,
		Metadata:         toAzureMeta(metadata),
		AccessConditions: ifMatch(ifMatchETag),
	})
	if err != nil {
		if ifMatchETag != "" && s.lostRace(ctx, key, ifMatchETag, err) {
			return nil, fmt.Errorf("put %s: %w (%v)", key, ErrPreconditionFailed, err)
		}
		return nil, azureErr("put", key, err)
	}
	return s.HeadVersion(ctx, key, deref(resp.VersionID))
}

func (s *AzureStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*Object, error) {
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
func (s *AzureStore) lostRace(ctx context.Context, key, ifMatch string, err error) bool {
	var re *azcore.ResponseError
	if errors.As(err, &re) || ctx.Err() != nil {
		return false
	}
	head, herr := s.Head(ctx, key)
	return herr == nil && head.ETag != ifMatch
}

// dropCurrentMarker finishes a Delete cut short between its two steps.
func (s *AzureStore) dropCurrentMarker(ctx context.Context, key, etag string) error {
	_, err := s.blob(key).Delete(ctx, &blob.DeleteOptions{AccessConditions: ifMatch(etag)})
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ConditionNotMet) {
		return azureErr("delete", key, err)
	}
	return nil
}

func (s *AzureStore) Get(ctx context.Context, key string) (io.ReadCloser, *Object, error) {
	return s.get(ctx, key, s.blob(key))
}

func (s *AzureStore) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *Object, error) {
	c, err := s.version(key, versionID)
	if err != nil {
		return nil, nil, err
	}
	return s.get(ctx, key, c)
}

func (s *AzureStore) get(ctx context.Context, key string, c *blockblob.Client) (io.ReadCloser, *Object, error) {
	resp, err := c.DownloadStream(ctx, nil)
	if err != nil {
		return nil, nil, azureErr("get", key, err)
	}
	if isMarker(resp.Metadata) {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("get %s: %w (deleted)", key, ErrNotFound)
	}
	return resp.Body, &Object{Key: key, VersionID: deref(resp.VersionID), ETag: etagString(resp.ETag),
		Size: deref(resp.ContentLength), LastModified: deref(resp.LastModified), Metadata: fromAzureMeta(resp.Metadata)}, nil
}

func (s *AzureStore) Head(ctx context.Context, key string) (*Object, error) {
	return s.head(ctx, key, s.blob(key))
}

func (s *AzureStore) HeadVersion(ctx context.Context, key, versionID string) (*Object, error) {
	if versionID == "" {
		return s.Head(ctx, key)
	}
	c, err := s.version(key, versionID)
	if err != nil {
		return nil, err
	}
	return s.head(ctx, key, c)
}

func (s *AzureStore) head(ctx context.Context, key string, c *blockblob.Client) (*Object, error) {
	resp, err := c.GetProperties(ctx, nil)
	if err != nil {
		return nil, azureErr("head", key, err)
	}
	if isMarker(resp.Metadata) {
		return nil, fmt.Errorf("head %s: %w (deleted)", key, ErrNotFound)
	}
	return &Object{Key: key, VersionID: deref(resp.VersionID), ETag: etagString(resp.ETag),
		Size: deref(resp.ContentLength), LastModified: deref(resp.LastModified), Metadata: fromAzureMeta(resp.Metadata)}, nil
}

func (s *AzureStore) UpdateMetadata(ctx context.Context, key string, metadata map[string]string, ifMatchETag string) (*Object, error) {
	if _, err := s.Head(ctx, key); err != nil {
		return nil, err
	}
	resp, err := s.blob(key).SetMetadata(ctx, toAzureMeta(metadata), &blob.SetMetadataOptions{AccessConditions: ifMatch(ifMatchETag)})
	if err != nil {
		return nil, azureErr("update metadata", key, err)
	}
	return s.HeadVersion(ctx, key, deref(resp.VersionID))
}

// listVersionsPageSize is shared with S3Store's listing (a var so tests can
// make pages small).

func (s *AzureStore) ListVersions(ctx context.Context, key string) ([]Object, error) {
	all, err := s.listVersions(ctx, key)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, o := range all {
		if o.Key == key {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *AzureStore) ListAllVersions(ctx context.Context, prefix string) ([]Object, error) {
	return s.listVersions(ctx, prefix)
}

// listVersions lists every version under prefix: per key, newest first
// (version IDs are timestamps, so their order is the versions'), with
// markers as delete markers, and a key with no current version whose
// newest version isn't a marker (deleted by something other than Delete)
// given one, dated as that version.
func (s *AzureStore) listVersions(ctx context.Context, prefix string) ([]Object, error) {
	type item struct {
		obj     Object
		current bool
	}
	byKey := map[string][]item{}
	var keys []string
	pager := s.client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:     to.Ptr(s.k(prefix)),
		Include:    container.ListBlobsInclude{Versions: true, Metadata: true},
		MaxResults: to.Ptr(listVersionsPageSize),
	})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureErr("list versions", prefix, err)
		}
		for _, b := range page.Segment.BlobItems {
			key := strings.TrimPrefix(deref(b.Name), s.keyPrefix)
			o := Object{Key: key, VersionID: deref(b.VersionID), DeleteMarker: isMarker(b.Metadata)}
			if b.Properties != nil {
				o.ETag = etagString(b.Properties.ETag)
				o.Size = deref(b.Properties.ContentLength)
				o.LastModified = deref(b.Properties.LastModified)
			}
			if o.DeleteMarker {
				o.ETag, o.Size = "", 0
			}
			if _, seen := byKey[key]; !seen {
				keys = append(keys, key)
			}
			byKey[key] = append(byKey[key], item{o, deref(b.IsCurrentVersion)})
		}
	}
	sort.Strings(keys)
	var out []Object
	for _, key := range keys {
		items := byKey[key]
		sort.Slice(items, func(i, j int) bool { return items[i].obj.VersionID > items[j].obj.VersionID })
		anyCurrent := false
		for _, it := range items {
			anyCurrent = anyCurrent || it.current
		}
		if !anyCurrent && len(items) > 0 && !items[0].obj.DeleteMarker {
			out = append(out, Object{Key: key, DeleteMarker: true, LastModified: items[0].obj.LastModified})
		}
		for _, it := range items {
			out = append(out, it.obj)
		}
	}
	return out, nil
}

func (s *AzureStore) Delete(ctx context.Context, key string, ifMatchETag string) error {
	// The marker records when; deleting the blob then makes it, the newest
	// version, no longer current. If another write lands between the two,
	// the blob isn't deleted: the write came after the delete.
	resp, err := s.blob(key).Upload(ctx, nopSeekCloser{bytes.NewReader(nil)}, &blockblob.UploadOptions{
		Metadata:         map[string]*string{markerKey: to.Ptr("1")},
		AccessConditions: ifMatch(ifMatchETag),
	})
	if err != nil {
		return azureErr("delete", key, err)
	}
	return s.dropCurrentMarker(ctx, key, etagString(resp.ETag))
}

func (s *AzureStore) DeleteVersion(ctx context.Context, key, versionID string) error {
	versions, err := s.ListVersions(ctx, key)
	if err != nil {
		return err
	}
	i := -1
	for j, v := range versions {
		if v.VersionID == versionID {
			i = j
		}
	}
	if i < 0 {
		return nil // already gone, as S3 answers
	}
	v := versions[i]
	current, _ := s.blob(key).GetProperties(ctx, nil)
	isCurrent := deref(current.VersionID) == versionID && !isMarker(current.Metadata)
	if v.DeleteMarker && i == 0 && versionID != "" {
		// Undelete: the version before the marker becomes current again.
		if i+1 < len(versions) && !versions[i+1].DeleteMarker {
			if err := s.promote(ctx, key, versions[i+1].VersionID); err != nil {
				return err
			}
		}
	}
	if isCurrent {
		// Azure deletes only previous versions by ID: make it one.
		if _, err := s.blob(key).Delete(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
			return azureErr("delete version", key, err)
		}
	}
	if versionID == "" {
		return nil // a marker given for a deletion Azure dated itself
	}
	c, err := s.version(key, versionID)
	if err != nil {
		return err
	}
	if _, err := c.Delete(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return azureErr("delete version", key, err)
	}
	return nil
}

// promote makes version versionID of key current again, as a new version
// with its content and metadata.
func (s *AzureStore) promote(ctx context.Context, key, versionID string) error {
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

func (s *AzureStore) PutParts(ctx context.Context, key string, parts []Part, metadata map[string]string, ifMatchETag string) (*Object, error) {
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
func (s *AzureStore) versionURL(key, versionID string) (string, error) {
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

func (s *AzureStore) ListByPrefix(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
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
			o := Object{Key: strings.TrimPrefix(deref(b.Name), s.keyPrefix)}
			if b.Properties != nil {
				o.Size = deref(b.Properties.ContentLength)
			}
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *AzureStore) ServerTime(ctx context.Context) (time.Time, error) {
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
