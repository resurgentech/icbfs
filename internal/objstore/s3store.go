package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
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

// ListVersions returns every version of key, newest first.
func (s *S3Store) ListVersions(ctx context.Context, key string) ([]Object, error) {
	out, err := s.client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("list versions %s: %w", key, err)
	}

	var versions []Object
	for _, v := range out.Versions {
		if aws.ToString(v.Key) != key {
			continue
		}
		versions = append(versions, Object{
			Key:          key,
			VersionID:    aws.ToString(v.VersionId),
			ETag:         aws.ToString(v.ETag),
			Size:         aws.ToInt64(v.Size),
			LastModified: aws.ToTime(v.LastModified),
		})
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].LastModified.After(versions[j].LastModified)
	})
	return versions, nil
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
