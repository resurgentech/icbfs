package icbfstest

// A hook around the test store's PutParts (T82-10 review 3, brought in by
// round 4), so a test in icbfs-git can hold an upload's assembly inside
// its store copy: cut it there, or let it land late.

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// R3Do performs the real PutParts under the context given.
type R3Do func(ctx context.Context) (*pkgicbfs.Object, error)

// R3Hook, if set, is called for every PutParts of Start's store whose
// metadata carries key "icbfs_upload" (an upload's assembly), in place of
// the PutParts: it decides when, whether and under which context do runs.
// A test that sets it clears it when it ends.
var R3Hook atomic.Pointer[func(ctx context.Context, key string, md map[string]string, do R3Do) (*pkgicbfs.Object, error)]

// r3Store is Start's store with R3Hook around PutParts; it keeps the
// store's paged listings (a wrapper loses them unless it says so).
type r3Store struct{ objstore.Store }

// EnforceIfMatch, if set, has Start's store refuse a conditional PutParts
// whose key has moved past ifMatch by the time it commits, as AWS and
// Azure do (the MinIO the tests run does not check If-Match on a
// multipart commit). Checked just before the store's own PutParts, after
// R3Hook. A test that sets it clears it when it ends.
var EnforceIfMatch atomic.Bool

func (s r3Store) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	do := func(c context.Context) (*objstore.Object, error) {
		if EnforceIfMatch.Load() && ifMatch != "" {
			if head, err := s.Store.Head(c, key); err == nil && head.ETag != ifMatch {
				return nil, fmt.Errorf("put parts %s: now %s, not %s: %w", key, head.ETag, ifMatch, objstore.ErrPreconditionFailed)
			}
		}
		return s.Store.PutParts(c, key, parts, md, ifMatch)
	}
	if h := R3Hook.Load(); h != nil && md["icbfs_upload"] != "" {
		return (*h)(ctx, key, md, do)
	}
	return write(ctx, WritePutParts, key, func() (*objstore.Object, error) { return do(ctx) })
}

func (s r3Store) ListPages(ctx context.Context, prefix string, fn func([]objstore.Object) error) error {
	_, err := write(ctx, ReadList, prefix, func() (struct{}, error) { return struct{}{}, objstore.ListEach(ctx, s.Store, prefix, fn) })
	return err
}

func (s r3Store) VersionsSince(ctx context.Context, key string, since time.Time) ([]objstore.Object, error) {
	return objstore.VersionsSince(ctx, s.Store, key, since)
}

// AbortCopy passes the store's through (objstore.CopyAborter, T82-10
// round 6).
func (s r3Store) AbortCopy(ctx context.Context, key, id string) error {
	_, err := write(ctx, WriteAbortCopy, key, func() (struct{}, error) { return struct{}{}, objstore.AbortCopy(ctx, s.Store, key, id) })
	return err
}
