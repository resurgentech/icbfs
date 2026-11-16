package icbfs

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
)

// pageCounter is an S3 store that counts how DiskUsage lists.
type pageCounter struct {
	*s3store.Store
	lists, pages int
}

func (c *pageCounter) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	c.lists++
	return c.Store.List(ctx, prefix)
}

func (c *pageCounter) ListPages(ctx context.Context, prefix string, fn func([]objstore.Object) error) error {
	return c.Store.ListPages(ctx, prefix, func(p []objstore.Object) error {
		c.pages++
		return fn(p)
	})
}

// DiskUsage sums a page at a time what List would return all at once:
// the same total, never the whole listing held.
func TestDiskUsageSumsPageByPage(t *testing.T) {
	base := newTestStore(t).(*s3store.Store)
	store := &pageCounter{Store: base}
	ctx := context.Background()
	f := New(store, "paged")
	if err := f.Bootstrap(ctx, 1<<30, 0o755, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		ino, _, _, err := f.Create(ctx, f.RootKey(), fmt.Sprintf("f%d", i), 0o644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteFile(ctx, ino, bytes.Repeat([]byte("z"), 100*(i+1)), ""); err != nil {
			t.Fatal(err)
		}
	}
	all, err := base.List(ctx, f.Prefix())
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, o := range all {
		want += o.Size
	}

	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 2
	store.lists, store.pages = 0, 0
	got, err := f.DiskUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("DiskUsage %d, the listing sums to %d", got, want)
	}
	if store.lists != 0 || store.pages < len(all)/2 {
		t.Fatalf("DiskUsage made %d List calls and %d pages of %d objects; want no List, pages of 2", store.lists, store.pages, len(all))
	}
}
