package icbfs

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
)

// listCounter counts the listings a caller makes under a prefix.
type listCounter struct {
	objstore.Store
	prefix string
	n      atomic.Int64
}

func (c *listCounter) under(prefix string) bool {
	return strings.HasPrefix(prefix, c.prefix) || strings.HasPrefix(c.prefix, prefix)
}

func (c *listCounter) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	if c.under(prefix) {
		c.n.Add(1)
	}
	return c.Store.List(ctx, prefix)
}

func (c *listCounter) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	if c.under(prefix) {
		c.n.Add(1)
	}
	return c.Store.HistoryUnder(ctx, prefix)
}

// FilesystemInfo reads the filesystem's settings, its creation date
// among them, from the master block alone: nothing is listed under the
// filesystem's prefix. DescribeFilesystem still gives the usage.
func TestFilesystemInfoReadsOnlyTheMasterBlock(t *testing.T) {
	base := miniotest.Store(t)
	ctx := context.Background()
	created := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	s := DefaultSettings("lib")
	s.Created = created
	made, err := CreateFilesystem(ctx, base, s)
	if err != nil {
		t.Fatal(err)
	}
	store := &listCounter{Store: base, prefix: made.ID + "-"}

	info, err := FilesystemInfo(ctx, store, "lib")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Created.Equal(created) || info.Name != "lib" || info.ID != made.ID {
		t.Fatalf("FilesystemInfo %+v, want lib created %v", info, created)
	}
	if n := store.n.Load(); n != 0 {
		t.Fatalf("FilesystemInfo listed under the filesystem %d times, want none", n)
	}
	if _, err := FilesystemInfo(ctx, store, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FilesystemInfo of a filesystem not there: %v, want ErrNotFound", err)
	}

	if _, used, err := DescribeFilesystem(ctx, store, "lib"); err != nil || used <= 0 {
		t.Fatalf("DescribeFilesystem: used %d, %v; want the root's bytes", used, err)
	}
	if store.n.Load() == 0 {
		t.Fatal("DescribeFilesystem listed nothing: want its usage")
	}
}
