package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Store is a Store backed by any S3-API-compatible endpoint (MinIO is the
// target here; AWS S3 itself works the same way).
type S3Store struct {
	client *s3.Client
	bucket string
}

var _ Store = (*S3Store)(nil)

// NewS3Store builds a Store against bucket using client.
func NewS3Store(client *s3.Client, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

func (s *S3Store) Put(ctx context.Context, key string, body io.Reader, metadata map[string]string, ifMatch string) (*Object, error) {
	in := &s3.PutObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		Body:     body,
		Metadata: metadata,
	}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("put %s: %w", key, err)
	}
	return s.headVersion(ctx, key, out.VersionId)
}

// partConcurrency bounds how many parts PutParts sends at once.
const partConcurrency = 4

func (s *S3Store) PutParts(ctx context.Context, key string, parts []Part, metadata map[string]string, ifMatch string) (*Object, error) {
	up, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		Metadata: metadata,
	})
	if err != nil {
		return nil, fmt.Errorf("put parts %s: %w", key, err)
	}
	abort := func() {
		// Best effort, and with its own context: ctx may be why we're
		// aborting. A leftover upload holds storage until a bucket
		// lifecycle rule expires it.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = s.client.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: up.UploadId,
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
		go func(i int, p Part) {
			defer wg.Done()
			defer func() { <-sem }()
			num := aws.Int32(int32(i + 1))
			var etag *string
			if p.Body != nil {
				out, err := s.client.UploadPart(pctx, &s3.UploadPartInput{
					Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: up.UploadId,
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
					Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: up.UploadId,
					PartNumber:      num,
					CopySource:      aws.String(s.bucket + "/" + key + "?versionId=" + p.CopyVersion),
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
		return nil, fmt.Errorf("put parts %s: %w", key, err)
	}

	in := &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	}
	out, err := s.client.CompleteMultipartUpload(ctx, in)
	if err != nil {
		abort()
		return nil, fmt.Errorf("put parts %s: %w", key, err)
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

// PutIfAbsent implements Store.PutIfAbsent via PutObject's IfNoneMatch:
// "*", confirmed against real MinIO to be honored (412 on conflict),
// not assumed from the S3 API docs alone.
func (s *S3Store) PutIfAbsent(ctx context.Context, key string, body io.Reader, metadata map[string]string) (*Object, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		Metadata:    metadata,
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		return nil, fmt.Errorf("put if absent %s: %w", key, err)
	}
	return s.headVersion(ctx, key, out.VersionId)
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, *Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("get %s: %w", key, err)
	}
	obj := &Object{
		Key:          key,
		VersionID:    aws.ToString(out.VersionId),
		ETag:         aws.ToString(out.ETag),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}
	return out.Body, obj, nil
}

func (s *S3Store) Head(ctx context.Context, key string) (*Object, error) {
	return s.headVersion(ctx, key, nil)
}

// headVersion is Head pinned to a specific version, used internally right
// after a write to report back exactly the version just created.
func (s *S3Store) headVersion(ctx context.Context, key string, versionID *string) (*Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(key),
		VersionId: versionID,
	})
	if err != nil {
		return nil, fmt.Errorf("head %s: %w", key, err)
	}
	return &Object{
		Key:          key,
		VersionID:    aws.ToString(out.VersionId),
		ETag:         aws.ToString(out.ETag),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}, nil
}

// UpdateMetadata replaces key's metadata via a copy-onto-self with the
// REPLACE directive. This does not re-upload the body, and on a versioned
// bucket it creates a new version exactly as a content write would. Since
// source and destination are the same key, CopySourceIfMatch conditions
// the update on the object's current ETag — the same CAS mechanism Put
// uses, applied to a metadata-only write.
func (s *S3Store) UpdateMetadata(ctx context.Context, key string, metadata map[string]string, ifMatch string) (*Object, error) {
	in := &s3.CopyObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		CopySource:        aws.String(s.bucket + "/" + key),
		Metadata:          metadata,
		MetadataDirective: types.MetadataDirectiveReplace,
	}
	if ifMatch != "" {
		in.CopySourceIfMatch = aws.String(ifMatch)
	}
	out, err := s.client.CopyObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("update metadata %s: %w", key, err)
	}
	return s.headVersion(ctx, key, out.VersionId)
}

// listVersionsPageSize is how many entries one ListObjectVersions page
// asks for; a var so tests can force pagination.
var listVersionsPageSize int32 = 1000

func (s *S3Store) ListVersions(ctx context.Context, key string) ([]Object, error) {
	all, err := s.listVersions(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("list versions %s: %w", key, err)
	}
	versions := all[:0]
	for _, v := range all {
		if v.Key == key {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

func (s *S3Store) ListAllVersions(ctx context.Context, prefix string) ([]Object, error) {
	all, err := s.listVersions(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list versions under %q: %w", prefix, err)
	}
	return all, nil
}

// listVersions pages through every version and delete marker under
// prefix. S3 reports a page's versions and delete markers as two separate
// lists, each newest first per key; they're merged back into one history
// per key here, by LastModified, with the store's IsLatest flag and then
// each list's own order breaking ties.
func (s *S3Store) listVersions(ctx context.Context, prefix string) ([]Object, error) {
	type entry struct {
		obj    Object
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
			Prefix:          aws.String(prefix),
			KeyMarker:       keyMarker,
			VersionIdMarker: versionMarker,
			MaxKeys:         aws.Int32(listVersionsPageSize),
		})
		if err != nil {
			return nil, err
		}
		for _, v := range out.Versions {
			seq++
			add(entry{obj: Object{
				Key:          aws.ToString(v.Key),
				VersionID:    aws.ToString(v.VersionId),
				ETag:         aws.ToString(v.ETag),
				Size:         aws.ToInt64(v.Size),
				LastModified: aws.ToTime(v.LastModified),
			}, latest: aws.ToBool(v.IsLatest), seq: seq})
		}
		for _, m := range out.DeleteMarkers {
			seq++
			add(entry{obj: Object{
				Key:          aws.ToString(m.Key),
				VersionID:    aws.ToString(m.VersionId),
				LastModified: aws.ToTime(m.LastModified),
				DeleteMarker: true,
			}, latest: aws.ToBool(m.IsLatest), seq: seq + 1<<30}) // after versions on a tie
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		keyMarker, versionMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}

	var all []Object
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

func (s *S3Store) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    aws.String(s.bucket),
		Key:       aws.String(key),
		VersionId: aws.String(versionID),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("get %s version %s: %w", key, versionID, err)
	}
	return out.Body, &Object{
		Key:          key,
		VersionID:    aws.ToString(out.VersionId),
		ETag:         aws.ToString(out.ETag),
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
		Metadata:     out.Metadata,
	}, nil
}

func (s *S3Store) HeadVersion(ctx context.Context, key, versionID string) (*Object, error) {
	return s.headVersion(ctx, key, aws.String(versionID))
}

func (s *S3Store) Delete(ctx context.Context, key string, ifMatch string) error {
	in := &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if ifMatch != "" {
		in.IfMatch = aws.String(ifMatch)
	}
	_, err := s.client.DeleteObject(ctx, in)
	if err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// ListByPrefix returns every current object under prefix, paginating
// internally via ListObjectsV2's ContinuationToken.
func (s *S3Store) ListByPrefix(ctx context.Context, prefix string) ([]Object, error) {
	var all []Object
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list by prefix %q: %w", prefix, err)
		}
		for _, o := range out.Contents {
			all = append(all, Object{
				Key:  aws.ToString(o.Key),
				Size: aws.ToInt64(o.Size),
			})
		}
		if !aws.ToBool(out.IsTruncated) {
			return all, nil
		}
		token = out.NextContinuationToken
	}
}

// ServerTime returns the object store's current clock, observed via
// the raw HTTP "Date" response header of a real request — not a typed
// SDK field like an Object's LastModified, which is "when this
// specific object was last written," not "what time is it right now."
// Confirmed against real MinIO (not assumed): the typed S3 API doesn't
// expose this header anywhere, so it's captured directly via a
// deserialize-step middleware on a single lightweight HeadBucket call.
//
// This exists so expires_at computations (ARCHITECTURE.md's Locking
// section) can be anchored to the object store's clock instead of the
// calling client's, which could be skewed relative to other clients —
// see internal/icbfs/lock.go's use of this.
func (s *S3Store) ServerTime(ctx context.Context) (time.Time, error) {
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
		return time.Time{}, fmt.Errorf("server time: %w", err)
	}
	if !found {
		return time.Time{}, errors.New("server time: response had no Date header")
	}
	return serverTime, nil
}

// IsPreconditionFailed reports whether err is the rejection from a failed
// conditional write (If-Match mismatch) — the signal a CAS retry loop
// should watch for.
func IsPreconditionFailed(err error) bool {
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode() == 412
	}
	return false
}

// IsNotFound reports whether err is the backend's "no such key" response,
// covering both GetObject (which reports a typed NoSuchKey) and
// HeadObject/CopyObject (which report a generic 404 with no distinguishing
// body).
func IsNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode() == 404
	}
	return false
}
