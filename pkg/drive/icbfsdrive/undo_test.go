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

// noDates refuses every write of a .version record while fail is set: a
// store that took a change's versions and then went away.
type noDates struct {
	objstore.Store
	fail atomic.Bool
}

var errNoDates = errors.New("the store is gone")

func (s *noDates) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if s.fail.Load() && strings.HasSuffix(key, objstore.VersionSuffix) {
		return nil, errNoDates
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func (s *noDates) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	if s.fail.Load() && strings.HasSuffix(key, objstore.VersionSuffix) {
		return nil, errNoDates
	}
	return s.Store.PutIfAbsent(ctx, key, body, md)
}

// TestUndatedChangeUndone: a change in a source's time whose versions
// can't be dated fails, and is undone: the file is as it was, every
// version it has dated.
func TestUndatedChangeUndone(t *testing.T) {
	store := &noDates{Store: miniotest.Store(t)}
	d := newDrive(t, store, icbfs.Settings{Name: "undo", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, "")
	ctx := system()
	it, err := d.Write(at(ctx, 2021), "/doc.txt", strings.NewReader("2021"), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store.fail.Store(true)
	if _, err := d.Write(at(ctx, 2022), "/doc.txt", strings.NewReader("2022"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, errNoDates) {
		t.Fatalf("a change that couldn't be dated: %v; want it to fail", err)
	}
	store.fail.Store(false)
	if got := read(t, ctx, d, drive.ByPath("/doc.txt"), drive.At{}); got != "2021" {
		t.Fatalf("after the undone change: %q, want 2021", got)
	}
	allDated(t, store, it.ID)
}
