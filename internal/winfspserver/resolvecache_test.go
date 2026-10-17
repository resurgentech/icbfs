package winfspserver

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/namecache"
	"github.com/resurgentech/icbfs/internal/objstore"
)

type countingStore struct {
	objstore.Store
	reads atomic.Int64
}

func (s *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.reads.Add(1)
	return s.Store.Get(ctx, key)
}

func (s *countingStore) Head(ctx context.Context, key string) (*objstore.Object, error) {
	s.reads.Add(1)
	return s.Store.Head(ctx, key)
}

// TestCachedPathWalks: with a name cache, walking a path a second time
// asks the store nothing, case-insensitively too; a walk whose last
// component must be fresh (an open's) sees another client's delete at
// once, where a cached walk may lag it until the cache expires.
func TestCachedPathWalks(t *testing.T) {
	base := newTestFilesystem(t)
	ctx := context.Background()
	counter := &countingStore{Store: base.Store()}
	fsys := icbfs.New(counter, "test")
	if err := fsys.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	dir := fsys.RootKey()
	for _, name := range []string{"a", "b", "c"} {
		key, _, err := fsys.Mkdir(ctx, dir, name, 0755, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		dir = key
	}
	if _, _, _, err := fsys.Create(ctx, dir, "Report.docx", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}

	res := resolver{fsys: fsys, ci: true, names: namecache.New(time.Hour, 1<<20, block.FoldKey)}
	if _, err := resolvePath(ctx, res, `\a\b\c\Report.docx`); err != nil {
		t.Fatal(err)
	}
	before := counter.reads.Load()
	for _, p := range []string{`\a\b\c\Report.docx`, `\A\B\C\REPORT.DOCX`, `\`} {
		r, err := resolvePath(ctx, res, p)
		if err != nil || r.Path != `\a\b\c\Report.docx` && p != `\` {
			t.Fatalf("resolve %s: %+v, %v", p, r, err)
		}
	}
	if n := counter.reads.Load() - before; n != 2 {
		// The root was never walked before, so it is stat'ed once: its
		// .metadata and its block.
		t.Fatalf("cached walks made %d store reads, want 2 (the root's stat)", n)
	}

	// Another client removes the file.
	other := icbfs.New(base.Store(), "test")
	if err := other.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	if err := other.Unlink(ctx, dir, "Report.docx"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePath(ctx, res, `\a\b\c\Report.docx`); err != nil {
		t.Fatalf("a cached walk lagging the delete is allowed (until it expires): %v", err)
	}
	fresh := res
	fresh.freshLast = true
	before = counter.reads.Load()
	if _, err := resolvePath(ctx, fresh, `\a\b\c\Report.docx`); !errors.Is(err, icbfs.ErrNotFound) {
		t.Fatalf("an open's walk after the delete: %v, want not found", err)
	}
	if n := counter.reads.Load() - before; n > 3 {
		t.Fatalf("an open's walk made %d store reads: only its last component should be read", n)
	}
	// And the fresh answer replaced the cached one.
	if _, err := resolvePath(ctx, res, `\a\b\c\Report.docx`); !errors.Is(err, icbfs.ErrNotFound) {
		t.Fatalf("a cached walk after the fresh one: %v, want not found", err)
	}
}

// TestUncachedResolverAsksEveryTime: without a cache (-meta-cache 0) every
// walk reads the store, as before there was one.
func TestUncachedResolverAsksEveryTime(t *testing.T) {
	base := newTestFilesystem(t)
	ctx := context.Background()
	counter := &countingStore{Store: base.Store()}
	fsys := icbfs.New(counter, "test")
	if err := fsys.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, res := range []resolver{{fsys: fsys}, {fsys: fsys, names: namecache.New(0, 1<<20, nil)}} {
		before := counter.reads.Load()
		for range 3 {
			if _, err := resolvePath(ctx, res, `\f`); err != nil {
				t.Fatal(err)
			}
		}
		if n := counter.reads.Load() - before; n < 3 {
			t.Fatalf("three uncached walks made %d store reads", n)
		}
	}
}
