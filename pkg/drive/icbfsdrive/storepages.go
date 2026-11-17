package icbfsdrive

import (
	"context"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// ListPages and ListPagesAfter pass the store's paged listings through
// (objstore.PageLister, objstore.AfterLister): a listing writes nothing,
// so there is nothing to drop, and without them the filesystem under the
// drive would list a prefix whole, the trash a page at a time among
// them (interloc T84).
func (s *dropOnWrite) ListPages(ctx context.Context, prefix string, fn func([]objstore.Object) error) error {
	return objstore.ListEach(ctx, s.Store, prefix, fn)
}

func (s *dropOnWrite) ListPagesAfter(ctx context.Context, prefix, after string, fn func([]objstore.Object) error) error {
	return objstore.ListEachAfter(ctx, s.Store, prefix, after, fn)
}
