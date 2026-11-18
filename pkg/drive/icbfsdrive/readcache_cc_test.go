package icbfsdrive

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
	"github.com/resurgentech/icbfs/pkg/readcache"
)

// keyReads counts the reads (Get, GetVersion) of one key: a file's
// content.
type keyReads struct {
	objstore.Store
	key atomic.Value // string
	n   atomic.Int64
}

func (s *keyReads) count(key string) {
	if k, _ := s.key.Load().(string); k == key {
		s.n.Add(1)
	}
}

func (s *keyReads) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.count(key)
	return s.Store.Get(ctx, key)
}

func (s *keyReads) GetVersion(ctx context.Context, key, v string) (io.ReadCloser, *objstore.Object, error) {
	s.count(key)
	return s.Store.GetVersion(ctx, key, v)
}

// contentGets is how many reads of the file's content were made since
// the last call.
func contentGets(s *keyReads) int64 { return s.n.Swap(0) }

// TestCCI5_DriveReadsThroughTheCache: a drive given Config.ReadCache
// reads a version once: a second Read of the unchanged file, now and as
// of a time, makes no content GET; a new version is fetched; a range
// reads the right bytes from the entry; a second drive on the same cache
// fetches nothing either.
func TestCCI5_DriveReadsThroughTheCache(t *testing.T) {
	store := &keyReads{Store: miniotest.Store(t)}
	rc, err := readcache.Open(filepath.Join(t.TempDir(), "rc"), readcache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	s := icbfs.Settings{Name: "readcache", Size: 1 << 30, RootMode: 0o777}
	d := newDriveWith(t, store, s, Config{ReadCache: rc})
	ctx := system()
	it := write(t, ctx, d, "/a/f.txt", "version one", drive.Condition{})
	store.key.Store(it.ID)
	time.Sleep(1100 * time.Millisecond) // the as-of view below is a second after the write, on the store's clock
	mid := time.Now()
	time.Sleep(1100 * time.Millisecond)
	write(t, ctx, d, "/a/f.txt", "version two!", drive.Condition{})

	readText := func(at drive.At, rng *drive.Range) string {
		t.Helper()
		body, _, err := d.Read(ctx, drive.ByPath("/a/f.txt"), at, rng)
		if err != nil {
			t.Fatal(err)
		}
		defer body.Close()
		b, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	contentGets(store)
	if got := readText(drive.At{}, nil); got != "version two!" {
		t.Fatalf("now: %q", got)
	}
	first := contentGets(store)
	if got := readText(drive.At{}, nil); got != "version two!" {
		t.Fatalf("now again: %q", got)
	}
	if n := contentGets(store); n != 0 {
		t.Fatalf("second Read now: %d content GETs, want 0 (first took %d)", n, first)
	}
	if got := readText(drive.At{}, &drive.Range{Offset: 8, Length: 3}); got != "two" {
		t.Fatalf("range: %q", got)
	}
	if got := readText(drive.At{Time: mid}, nil); got != "version one" {
		t.Fatalf("as of the middle: %q", got)
	}
	if n := contentGets(store); n != 1 {
		t.Fatalf("first Read of the older version: %d content GETs, want 1", n)
	}
	if got := readText(drive.At{Time: mid}, nil); got != "version one" {
		t.Fatalf("as of again: %q", got)
	}
	if n := contentGets(store); n != 0 {
		t.Fatalf("second Read as of a time: %d content GETs, want 0", n)
	}
	// Another drive of the same filesystem, on the same cache.
	other := newDriveWith(t, store, s, Config{ReadCache: rc})
	contentGets(store)
	body, _, err := other.Read(ctx, drive.ByPath("/a/f.txt"), drive.At{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(body)
	body.Close()
	if !strings.HasPrefix(string(b), "version two") {
		t.Fatalf("other drive: %q", b)
	}
	if n := contentGets(store); n != 0 {
		t.Fatalf("another drive on the same cache: %d content GETs, want 0", n)
	}
}
