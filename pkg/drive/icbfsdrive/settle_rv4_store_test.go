package icbfsdrive

// Round-4 reviewer's store wrapper (shared by the branch and main copies):
// counts requests and bytes read, lists entries, and can fail every request
// from the n-th on (after counting it: mode "cut" fails it unmade, "lost"
// makes it and loses its answer).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
)

type rvStore struct {
	objstore.Store
	mu        sync.Mutex
	n         int
	bytes     int64
	listed    int64
	byOp      map[string]int
	failAt    int    // 0: never
	mode      string // "cut" or "lost"
	countTime bool
}

var errRVCut = errors.New("rv: the store is cut")

func newRVStore(s objstore.Store) *rvStore { return &rvStore{Store: s, byOp: map[string]int{}} }

// req counts one request; reports whether to fail it, and whether to make it
// first (lost).
func (s *rvStore) req(op string) (fail, make bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op == "TIME" && !s.countTime {
		return false, true
	}
	s.n++
	s.byOp[op]++
	if s.failAt > 0 && s.n >= s.failAt {
		return true, s.mode == "lost" && s.n == s.failAt
	}
	return false, true
}

func (s *rvStore) take() (int, int64, int64, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, b, l, m := s.n, s.bytes, s.listed, s.byOp
	s.n, s.bytes, s.listed, s.byOp = 0, 0, 0, map[string]int{}
	return n, b, l, m
}

func (s *rvStore) setFail(at int, mode string) {
	s.mu.Lock()
	s.failAt, s.mode, s.n = at, mode, 0
	s.mu.Unlock()
}

type rvCounted struct {
	io.ReadCloser
	s *rvStore
}

func (c rvCounted) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.s.mu.Lock()
	c.s.bytes += int64(n)
	c.s.mu.Unlock()
	return n, err
}

func rvDo[T any](s *rvStore, op string, f func() (T, error)) (T, error) {
	var zero T
	fail, make := s.req(op)
	if fail && !make {
		return zero, fmt.Errorf("%s: %w", op, errRVCut)
	}
	v, err := f()
	if fail {
		return zero, fmt.Errorf("%s (answer lost): %w", op, errRVCut)
	}
	return v, err
}

func (s *rvStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	return rvDo(s, "PUT", func() (*objstore.Object, error) { return s.Store.Put(ctx, key, body, md, ifMatch) })
}
func (s *rvStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	return rvDo(s, "PUT", func() (*objstore.Object, error) { return s.Store.PutIfAbsent(ctx, key, body, md) })
}
func (s *rvStore) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	return rvDo(s, "PUT", func() (*objstore.Object, error) { return s.Store.PutParts(ctx, key, parts, md, ifMatch) })
}
func (s *rvStore) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	type r struct {
		b io.ReadCloser
		o *objstore.Object
	}
	v, err := rvDo(s, "GET", func() (r, error) { b, o, err := s.Store.Get(ctx, key); return r{b, o}, err })
	if err != nil {
		return nil, nil, err
	}
	return rvCounted{v.b, s}, v.o, nil
}
func (s *rvStore) Head(ctx context.Context, key string) (*objstore.Object, error) {
	return rvDo(s, "HEAD", func() (*objstore.Object, error) { return s.Store.Head(ctx, key) })
}
func (s *rvStore) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	type r struct {
		b  io.ReadCloser
		o  *objstore.Object
		ch bool
	}
	v, err := rvDo(s, "GET", func() (r, error) { b, o, ch, err := s.Store.GetIfChanged(ctx, key, etag); return r{b, o, ch}, err })
	if err != nil {
		return nil, nil, false, err
	}
	if v.b != nil {
		v.b = rvCounted{v.b, s}
	}
	return v.b, v.o, v.ch, nil
}
func (s *rvStore) GetVersion(ctx context.Context, key, id string) (io.ReadCloser, *objstore.Object, error) {
	type r struct {
		b io.ReadCloser
		o *objstore.Object
	}
	v, err := rvDo(s, "GETV", func() (r, error) { b, o, err := s.Store.GetVersion(ctx, key, id); return r{b, o}, err })
	if err != nil {
		return nil, nil, err
	}
	return rvCounted{v.b, s}, v.o, nil
}
func (s *rvStore) HeadVersion(ctx context.Context, key, id string) (*objstore.Object, error) {
	return rvDo(s, "HEAD", func() (*objstore.Object, error) { return s.Store.HeadVersion(ctx, key, id) })
}
func (s *rvStore) Delete(ctx context.Context, key string) error {
	_, err := rvDo(s, "DEL", func() (int, error) { return 0, s.Store.Delete(ctx, key) })
	return err
}
func (s *rvStore) Undelete(ctx context.Context, key string) error {
	_, err := rvDo(s, "UNDEL", func() (int, error) { return 0, s.Store.Undelete(ctx, key) })
	return err
}
func (s *rvStore) History(ctx context.Context, key string) ([]objstore.Object, error) {
	v, err := rvDo(s, "LIST", func() ([]objstore.Object, error) { return s.Store.History(ctx, key) })
	s.mu.Lock()
	s.listed += int64(len(v))
	s.mu.Unlock()
	return v, err
}
func (s *rvStore) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	v, err := rvDo(s, "LIST", func() ([]objstore.Object, error) { return s.Store.HistoryUnder(ctx, prefix) })
	s.mu.Lock()
	s.listed += int64(len(v))
	s.mu.Unlock()
	return v, err
}
func (s *rvStore) Erase(ctx context.Context, key, id string) error {
	_, err := rvDo(s, "ERASE", func() (int, error) { return 0, s.Store.Erase(ctx, key, id) })
	return err
}
func (s *rvStore) EraseAll(ctx context.Context, prefix string) error {
	_, err := rvDo(s, "ERASE", func() (int, error) { return 0, s.Store.EraseAll(ctx, prefix) })
	return err
}
func (s *rvStore) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	return rvDo(s, "LIST", func() ([]objstore.Object, error) { return s.Store.List(ctx, prefix) })
}
func (s *rvStore) ServerTime(ctx context.Context) (time.Time, error) {
	return rvDo(s, "TIME", func() (time.Time, error) { return s.Store.ServerTime(ctx) })
}

// rvExternal is a versioned bucket in the MinIO at $RV4_S3 (made if new).
func rvExternal(t *testing.T, bucket string) objstore.Store {
	t.Helper()
	ep := os.Getenv("RV4_S3")
	if ep == "" {
		t.Skip("RV4_S3 unset")
	}
	client := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(ep), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider("rvadmin", "rvadmin-secret", "")})
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil &&
		!strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") && !strings.Contains(err.Error(), "BucketAlreadyExists") {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	return s3store.New(client, bucket)
}
