package s3store

import (
	"context"
	"errors"
	"fmt"
	"github.com/resurgentech/icbfs/internal/objstore"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Store is a Store backed by any S3-API-compatible endpoint (MinIO is the
// target here; AWS S3 itself works the same way).
type Store struct {
	client    *s3.Client
	bucket    string
	keyPrefix string // see WithKeyPrefix
}

var _ objstore.Store = (*Store)(nil)

// New builds a Store against bucket using client.
func New(client *s3.Client, bucket string) *Store {
	return &Store{client: client, bucket: bucket}
}

// WithKeyPrefix returns a store on the same bucket that keeps every key
// under prefix, invisibly to its callers: several stores can share one
// bucket, as test runs against a real S3 bucket do.
func (s *Store) WithKeyPrefix(prefix string) *Store {
	c := *s
	c.keyPrefix = prefix
	return &c
}

// k is key as stored: under keyPrefix.
func (s *Store) k(key string) string { return s.keyPrefix + key }

func (s *Store) Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatch string) (*objstore.Object, error) {
	in := &s3.PutObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.k(key)),
		Body:     body,
		Metadata: metadata,
	}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		if ifMatch != "" && s.lostRace(ctx, key, ifMatch, err) {
			return nil, fmt.Errorf("put %s: %w (%v)", key, objstore.ErrPreconditionFailed, err)
		}
		return nil, s3Error(fmt.Errorf("put %s: %w", key, err))
	}
	return s.headVersion(ctx, key, out.VersionId)
}

// lostRace reports whether a conditional write that failed without an
// answer (err isn't one from the server) failed because key no longer
// matches ifMatch. A server may answer a failed precondition before reading
// the body and close the connection; over a slow link the client is still
// sending and sees only a reset. Treating that as a lost race lets the
// caller rebase instead of failing every retry the same way.
func (s *Store) lostRace(ctx context.Context, key, ifMatch string, err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) || ctx.Err() != nil {
		return false
	}
	head, herr := s.Head(ctx, key)
	return herr == nil && head.ETag != ifMatch
}

// partConcurrency bounds how many parts PutParts sends at once.
const partConcurrency = 4

func (s *Store) PutParts(ctx context.Context, key string, parts []objstore.Part, metadata map[string]string, ifMatch string) (*objstore.Object, error) {
	up, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.k(key)),
		Metadata: metadata,
	})
	if err != nil {
		return nil, s3Error(fmt.Errorf("put parts %s: %w", key, err))
	}
	abort := func() {
		// Best effort, and with its own context: ctx may be why we're
		// aborting. A leftover upload holds storage until a bucket
		// lifecycle rule expires it.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = s.client.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(s.k(key)), UploadId: up.UploadId,
		})
	}

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make([]types.CompletedPart, len(parts))
	errs := make([]error, len(parts))
	sem := make(chan struct{}, partConcurrency)
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p objstore.Part) {
			defer wg.Done()
			defer func() { <-sem }()
			num := aws.Int32(int32(i + 1))
			var etag *string
			if p.Body != nil {
				out, err := s.client.UploadPart(pctx, &s3.UploadPartInput{
					Bucket: aws.String(s.bucket), Key: aws.String(s.k(key)), UploadId: up.UploadId,
					PartNumber: num, Body: p.Body, ContentLength: aws.Int64(p.Size),
				})
				if err != nil {
					errs[i] = err
					cancel()
					return
				}
				etag = out.ETag
			} else {
				out, err := s.client.UploadPartCopy(pctx, &s3.UploadPartCopyInput{
					Bucket: aws.String(s.bucket), Key: aws.String(s.k(key)), UploadId: up.UploadId,
					PartNumber:      num,
					CopySource:      aws.String(s.bucket + "/" + s.k(key) + "?versionId=" + p.CopyVersion),
					CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", p.CopyOffset, p.CopyOffset+p.Size-1)),
				})
				if err != nil {
					errs[i] = err
					cancel()
					return
				}
				etag = out.CopyPartResult.ETag
			}
			completed[i] = types.CompletedPart{PartNumber: num, ETag: etag}
		}(i, p)
	}
	wg.Wait()
	if err := firstRealError(errs); err != nil {
		abort()
		return nil, s3Error(fmt.Errorf("put parts %s: %w", key, err))
	}

	in := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.k(key)), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	}
	out, err := s.client.CompleteMultipartUpload(ctx, in)
	if err != nil {
		abort()
		return nil, s3Error(fmt.Errorf("put parts %s: %w", key, err))
	}
	return s.headVersion(ctx, key, out.VersionId)
}

// firstRealError returns the first error in errs that isn't just a part
// cancelled because another one failed first.
func firstRealError(errs []error) error {
	var cancelled error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if errors.Is(err, context.Canceled) {
			if cancelled == nil {
				cancelled = err
			}
			continue
		}
		return err
	}
	return cancelled
}

// PutIfAbsent implements Store.PutIfAbsent with PutObject's
// IfNoneMatch "*" (MinIO answers 412 when the key exists).
func (s *Store) PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*objstore.Object, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.k(key)),
		Body:        body,
		Metadata:    metadata,
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		return nil, s3Error(fmt.Errorf("put if absent %s: %w", key, err))
	}
	return s.headVersion(ctx, key, out.VersionId)
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.k(key)),
	})
	if err != nil {
		return nil, nil, s3Error(fmt.Errorf("get %s: %w", key, err))
	}
	obj := &objstore.Object{
		Key:          key,
		VersionID:    aws.ToString(out.VersionId),
		ETag:         aws.ToString(out.ETag),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}
	return out.Body, obj, nil
}

func (s *Store) Head(ctx context.Context, key string) (*objstore.Object, error) {
	return s.headVersion(ctx, key, nil)
}

// headVersion is Head pinned to a specific version, used internally right
// after a write to report back exactly the version just created.
func (s *Store) headVersion(ctx context.Context, key string, versionID *string) (*objstore.Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(s.k(key)),
		VersionId: versionID,
	})
	if err != nil {
		return nil, s3Error(fmt.Errorf("head %s: %w", key, err))
	}
	return &objstore.Object{
		Key:          key,
		VersionID:    aws.ToString(out.VersionId),
		ETag:         aws.ToString(out.ETag),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}, nil
}

func (s *Store) History(ctx context.Context, key string) ([]objstore.Object, error) {
	all, err := s.listVersions(ctx, key)
	if err != nil {
		return nil, s3Error(fmt.Errorf("list versions %s: %w", key, err))
	}
	versions := all[:0]
	for _, v := range all {
		if v.Key == key {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

func (s *Store) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	all, err := s.listVersions(ctx, prefix)
	if err != nil {
		return nil, s3Error(fmt.Errorf("list versions under %q: %w", prefix, err))
	}
	return all, nil
}

// listVersions pages through every version and delete marker under
// prefix. S3 reports a page's versions and delete markers as two separate
// lists, each newest first per key; they're merged back into one history
// per key here, by LastModified, with the store's IsLatest flag and then
// each list's own order breaking ties.
func (s *Store) listVersions(ctx context.Context, prefix string) ([]objstore.Object, error) {
	type entry struct {
		obj    objstore.Object
		latest bool
		seq    int
	}
	byKey := map[string][]entry{}
	var keys []string
	add := func(e entry) {
		if _, ok := byKey[e.obj.Key]; !ok {
			keys = append(keys, e.obj.Key)
		}
		byKey[e.obj.Key] = append(byKey[e.obj.Key], e)
	}
	seq := 0
	var keyMarker, versionMarker *string
	for {
		out, err := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
			Bucket:          aws.String(s.bucket),
			Prefix:          aws.String(s.k(prefix)),
			KeyMarker:       keyMarker,
			VersionIdMarker: versionMarker,
			MaxKeys:         aws.Int32(objstore.HistoryPageSize),
		})
		if err != nil {
			return nil, err
		}
		for _, v := range out.Versions {
			seq++
			add(entry{obj: objstore.Object{
				Key:          strings.TrimPrefix(aws.ToString(v.Key), s.keyPrefix),
				VersionID:    aws.ToString(v.VersionId),
				ETag:         aws.ToString(v.ETag),
				Size:         aws.ToInt64(v.Size),
				LastModified: aws.ToTime(v.LastModified),
			}, latest: aws.ToBool(v.IsLatest), seq: seq})
		}
		for _, m := range out.DeleteMarkers {
			seq++
			add(entry{obj: objstore.Object{
				Key:          strings.TrimPrefix(aws.ToString(m.Key), s.keyPrefix),
				VersionID:    aws.ToString(m.VersionId),
				LastModified: aws.ToTime(m.LastModified),
				Deleted:      true,
			}, latest: aws.ToBool(m.IsLatest), seq: seq + 1<<30}) // after versions on a tie
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		keyMarker, versionMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}

	var all []objstore.Object
	for _, k := range keys {
		es := byKey[k]
		sort.SliceStable(es, func(i, j int) bool {
			a, b := es[i], es[j]
			if !a.obj.LastModified.Equal(b.obj.LastModified) {
				return a.obj.LastModified.After(b.obj.LastModified)
			}
			if a.latest != b.latest {
				return a.latest
			}
			return a.seq < b.seq
		})
		for _, e := range es {
			all = append(all, e.obj)
		}
	}
	return all, nil
}

func (s *Store) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *objstore.Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(s.k(key)),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		return nil, nil, s3Error(fmt.Errorf("get %s version %s: %w", key, versionID, err))
	}
	return out.Body, &objstore.Object{
		Key:          key,
		VersionID:    aws.ToString(out.VersionId),
		ETag:         aws.ToString(out.ETag),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}, nil
}

func (s *Store) HeadVersion(ctx context.Context, key, versionID string) (*objstore.Object, error) {
	return s.headVersion(ctx, key, aws.String(versionID))
}

// Erase deletes one version (or delete marker) of key by its ID: S3 erases
// it for good.
func (s *Store) Erase(ctx context.Context, key, versionID string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(s.k(key)),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		return s3Error(fmt.Errorf("delete %s version %s: %w", key, versionID, err))
	}
	return nil
}

// Delete leaves a delete marker, S3's record of the deletion.
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.k(key)),
	})
	if err != nil {
		return s3Error(fmt.Errorf("delete %s: %w", key, err))
	}
	return nil
}

// Undelete removes key's newest delete marker, if it has one: the version
// before it is current again, as it was before the delete, with its own
// version ID.
func (s *Store) Undelete(ctx context.Context, key string) error {
	history, err := s.History(ctx, key)
	if err != nil {
		return err
	}
	if len(history) == 0 || !history[0].Deleted {
		return nil
	}
	return s.Erase(ctx, key, history[0].VersionID)
}

// EraseAll erases every version and delete marker under prefix.
func (s *Store) EraseAll(ctx context.Context, prefix string) error {
	history, err := s.HistoryUnder(ctx, prefix)
	if err != nil {
		return err
	}
	for _, v := range history {
		if err := s.Erase(ctx, v.Key, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// PartLimits are S3's multipart limits (MinIO's are the same).
func (s *Store) PartLimits() objstore.PartLimits {
	return objstore.PartLimits{Min: 5 << 20, Max: 5 << 30, Count: 10000}
}

// List returns every current object under prefix, paginating internally
// via ListObjectsV2's ContinuationToken.
func (s *Store) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	var all []objstore.Object
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(s.k(prefix)),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, s3Error(fmt.Errorf("list by prefix %q: %w", prefix, err))
		}
		for _, o := range out.Contents {
			all = append(all, objstore.Object{
				Key:  strings.TrimPrefix(aws.ToString(o.Key), s.keyPrefix),
				Size: aws.ToInt64(o.Size),
			})
		}
		if !aws.ToBool(out.IsTruncated) {
			return all, nil
		}
		token = out.NextContinuationToken
	}
}

// ServerTime is the store's clock now, from the HTTP Date header of a
// HeadBucket, captured by deserialize middleware: the typed S3 API doesn't
// expose it. Lock leases are anchored to it (internal/icbfs's lock.go),
// rather than to the caller's clock.
func (s *Store) ServerTime(ctx context.Context) (time.Time, error) {
	var serverTime time.Time
	var found bool
	capture := middleware.DeserializeMiddlewareFunc("icbfsCaptureDateHeader", func(
		ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler,
	) (middleware.DeserializeOutput, middleware.Metadata, error) {
		out, metadata, err := next.HandleDeserialize(ctx, in)
		if resp, ok := out.RawResponse.(*smithyhttp.Response); ok {
			if raw := resp.Header.Get("Date"); raw != "" {
				if t, perr := time.Parse(time.RFC1123, raw); perr == nil {
					serverTime = t
					found = true
				}
			}
		}
		return out, metadata, err
	})

	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)},
		s3.WithAPIOptions(func(stack *middleware.Stack) error {
			return stack.Deserialize.Add(capture, middleware.After)
		}),
	)
	if err != nil {
		return time.Time{}, s3Error(fmt.Errorf("server time: %w", err))
	}
	if !found {
		return time.Time{}, errors.New("server time: response had no Date header")
	}
	return serverTime, nil
}

// s3Error adds to err the shared sentinel for what S3 said: objstore.ErrNotFound
// for "no such key" (GetObject reports a typed NoSuchKey; HeadObject and
// CopyObject a bare 404), objstore.ErrPreconditionFailed for a 412.
func s3Error(err error) error {
	if err == nil {
		return nil
	}
	var respErr *smithyhttp.ResponseError
	status := 0
	if errors.As(err, &respErr) {
		status = respErr.HTTPStatusCode()
	}
	var noSuchKey *types.NoSuchKey
	var apiErr smithy.APIError
	notFound := status == 404 || errors.As(err, &noSuchKey)
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NoSuchVersion", "NotFound", "404":
			notFound = true
		}
	}
	switch {
	case notFound:
		return fmt.Errorf("%w: %w", objstore.ErrNotFound, err)
	case status == 412:
		return fmt.Errorf("%w: %w", objstore.ErrPreconditionFailed, err)
	}
	return err
}
