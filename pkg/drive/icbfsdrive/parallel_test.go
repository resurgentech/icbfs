package icbfsdrive

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// blockRace holds the first n writes of the folder blocks it watches
// until all n have arrived, so they race as truly simultaneous creates
// would, and counts the ones the store refuses (a lost compare-and-swap).
type blockRace struct {
	objstore.Store
	mu      sync.Mutex
	watched map[string]bool
	n       int
	gate    chan struct{}
	lost    int
}

func (s *blockRace) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	s.mu.Lock()
	watched := s.watched[key]
	gate := s.gate
	if watched && s.n > 0 {
		if s.n--; s.n == 0 {
			close(s.gate)
		}
	}
	s.mu.Unlock()
	if !watched {
		return s.Store.Put(ctx, key, body, md, ifMatch)
	}
	select {
	case <-gate:
	case <-time.After(10 * time.Second): // fewer arrived: let them go
	}
	obj, err := s.Store.Put(ctx, key, body, md, ifMatch)
	if objstore.IsPreconditionFailed(err) {
		s.mu.Lock()
		s.lost++
		s.mu.Unlock()
	}
	return obj, err
}

// TestCreatesInParallel: files made at once under one folder contend on
// its block (each create is a compare-and-swap of it; all but one lose
// the first round and go again), and all land; under different folders
// they don't contend at all. What docs/api.md tells a caller sizing how
// many creates to run at once.
func TestCreatesInParallel(t *testing.T) {
	const n = 6
	for _, shared := range []bool{true, false} {
		name := "one folder"
		if !shared {
			name = "a folder each"
		}
		t.Run(name, func(t *testing.T) {
			store := &blockRace{Store: miniotest.Store(t), watched: map[string]bool{}}
			d := newDrive(t, store, icbfs.Settings{Name: "parallel", Size: 1 << 30, RootMode: 0o777}, "")
			ctx := system()
			paths := make([]string, n)
			for i := range paths {
				dir := "/shared"
				if !shared {
					dir = fmt.Sprintf("/dir%d", i)
				}
				it, err := d.Mkdir(ctx, dir, true)
				if err != nil {
					t.Fatal(err)
				}
				store.watched[it.ID] = true
				paths[i] = fmt.Sprintf("%s/f%d.txt", dir, i)
			}
			store.n, store.gate = n, make(chan struct{})

			var wg sync.WaitGroup
			errs := make([]error, n)
			for i, p := range paths {
				wg.Go(func() {
					_, errs[i] = d.Write(ctx, p, strings.NewReader("x"), drive.Condition{IfNoneMatch: true}, drive.WriteOptions{})
				})
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("create %s: %v", paths[i], err)
				}
			}
			for _, p := range paths {
				if _, err := d.Stat(ctx, drive.ByPath(p), drive.At{}); err != nil {
					t.Fatalf("%s isn't there: %v", p, err)
				}
			}
			switch {
			case shared && store.lost < n-1:
				t.Fatalf("%d creates at once in one folder lost %d compare-and-swaps, want at least %d", n, store.lost, n-1)
			case !shared && store.lost != 0:
				t.Fatalf("%d creates at once in different folders lost %d compare-and-swaps, want none", n, store.lost)
			}
		})
	}
}
