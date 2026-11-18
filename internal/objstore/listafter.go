package objstore

import (
	"context"
	"sort"
)

// AfterLister is a PageLister that can start after a key: ListPagesAfter
// is ListPages over only the keys under prefix that sort after after
// ("" : all of them), so a caller that carries on a listing where it
// stopped does not list again what it has passed. S3Store (StartAfter)
// and AzureStore (StartFrom) have it; a wrapper of a Store does not
// unless it says so.
type AfterLister interface {
	ListPagesAfter(ctx context.Context, prefix, after string, fn func([]Object) error) error
}

// ListEachAfter is ListEach over only the keys under prefix that sort
// after after, in key order, a page at a time: one listing that starts
// after after when s is an AfterLister, else the whole listing with what
// comes before after left out (each page sorted when s lists all at
// once). Every key fn is given sorts after after, whatever the store
// answered. fn may keep the slice it is given.
func ListEachAfter(ctx context.Context, s Store, prefix, after string, fn func([]Object) error) error {
	keep := func(page []Object) error {
		if after != "" {
			i := 0
			for i < len(page) && page[i].Key <= after {
				i++
			}
			page = page[i:]
		}
		if len(page) == 0 {
			return nil
		}
		return fn(page)
	}
	if al, ok := s.(AfterLister); ok {
		return al.ListPagesAfter(ctx, prefix, after, keep)
	}
	_, paged := s.(PageLister)
	return ListEach(ctx, s, prefix, func(page []Object) error {
		if !paged {
			sort.Slice(page, func(i, j int) bool { return page[i].Key < page[j].Key })
		}
		return keep(page)
	})
}

// ListPagesAfter starts after a key if the store under it can: a listing
// writes nothing, so there is nothing to record.
func (r *recording) ListPagesAfter(ctx context.Context, prefix, after string, fn func([]Object) error) error {
	return ListEachAfter(ctx, r.Store, prefix, after, fn)
}
