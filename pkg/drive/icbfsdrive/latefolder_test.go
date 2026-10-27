package icbfsdrive

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// failOnce refuses the next write of key, once.
type failOnce struct {
	objstore.Store
	key atomic.Pointer[string]
}

var errFailOnce = errors.New("the store refused that one")

func (s *failOnce) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if k := s.key.Load(); k != nil && *k == key && s.key.CompareAndSwap(k, nil) {
		return nil, errFailOnce
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

// TestLateFolderWithoutItsTime: a file written into a folder behind the
// folder's latest state, whose write of the folder's modified time fails,
// fails (the time is the folder's history), and the folder's history is
// still put right: found late from its dates, not from that write.
func TestLateFolderWithoutItsTime(t *testing.T) {
	store := &failOnce{Store: miniotest.Store(t)}
	d := newDrive(t, store, icbfs.Settings{Name: "latefolder", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, "")
	ctx := system()
	if _, err := d.Write(at(ctx, 2022), "/dir/new.txt", strings.NewReader("new"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
		t.Fatal(err)
	}
	dir, err := d.Stat(ctx, drive.ByPath("/dir"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	meta := dir.ID + ".metadata"
	store.key.Store(&meta)
	if _, err := d.Write(at(ctx, 2021), "/dir/old.txt", strings.NewReader("old"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, errFailOnce) {
		t.Fatalf("a late file whose folder's time couldn't be written: %v; want it to fail", err)
	}
	if got := names(t, d, "/dir", year(2021).AddDate(0, 6, 0)); got != "old.txt" {
		t.Fatalf("/dir as of mid-2021: %q, want old.txt", got)
	}
}
