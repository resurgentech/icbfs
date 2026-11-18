package icbfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// contentCounter counts the reads of one key's content (Get and
// GetVersion), and can hold the first until released or put another
// version in between a Head and the read (race).
type contentCounter struct {
	objstore.Store
	key   atomic.Value // string
	reads atomic.Int64
	heads atomic.Int64

	hold     chan struct{} // if set, the first content read waits for it
	holdOnce sync.Once

	race     []byte // if set, the next Head of key is followed by a Put of these bytes
	raceOnce sync.Once
}

func (s *contentCounter) is(key string) bool {
	k, _ := s.key.Load().(string)
	return k != "" && k == key
}

func (s *contentCounter) Head(ctx context.Context, key string) (*objstore.Object, error) {
	obj, err := s.Store.Head(ctx, key)
	if s.is(key) {
		s.heads.Add(1)
		if s.race != nil {
			s.raceOnce.Do(func() {
				_, _ = s.Store.Put(ctx, key, bytes.NewReader(s.race), nil, "")
			})
		}
	}
	return obj, err
}

func (s *contentCounter) read() {
	s.reads.Add(1)
	if s.hold != nil {
		s.holdOnce.Do(func() {
			select {
			case <-s.hold:
			case <-time.After(30 * time.Second):
			}
		})
	}
}

func (s *contentCounter) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	if s.is(key) {
		s.read()
	}
	return s.Store.Get(ctx, key)
}

func (s *contentCounter) GetVersion(ctx context.Context, key, version string) (io.ReadCloser, *objstore.Object, error) {
	if s.is(key) {
		s.read()
	}
	return s.Store.GetVersion(ctx, key, version)
}

// writeFile makes name in fsys's root with data, flushed.
func writeFile(t *testing.T, fsys *Filesystem, name string, data []byte) string {
	t.Helper()
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), name, 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	of, err := fsys.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := of.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := of.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := of.Close(); err != nil {
		t.Fatal(err)
	}
	return key
}

// attached is a new Filesystem on store ("test"), with cacheDir as its
// cache directory if given.
func attached(t *testing.T, store objstore.Store, cacheDir string) *Filesystem {
	t.Helper()
	f := New(store, "test")
	if cacheDir != "" {
		f.SetCacheOptions(cacheDir, 1<<20)
	}
	if err := f.Attach(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func readWhole(t *testing.T, of *OpenFile) []byte {
	t.Helper()
	return of.ReadAt(0, of.Size())
}

// TestCCI1_OpenFetchesTheVersionItsHeadNamed: a write lands between an
// Open's Head (V1) and its fetch: the open reads V1's bytes, the version
// its ETag is, never V2's under V1's name (the study's first defect).
func TestCCI1_OpenFetchesTheVersionItsHeadNamed(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	key := writeFile(t, fsys, "f.txt", []byte("version one"))
	cs := &contentCounter{Store: store, race: []byte("version two!")}
	cs.key.Store(key)
	f := attached(t, cs, "")
	of, err := f.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer of.Close()
	if got := readWhole(t, of); string(got) != "version one" {
		t.Fatalf("the open read %q, want the version its Head named (\"version one\")", got)
	}
	// A new open sees V2 (close-to-open), from its own Head.
	g := attached(t, store, "")
	og, err := g.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer og.Close()
	if got := readWhole(t, og); string(got) != "version two!" {
		t.Fatalf("a later open read %q", got)
	}
}

// TestCCI2_TwoOpensOneFetch: two Opens of a file at once, neither yet
// open nor cached, fetch its content once (the study's second defect).
func TestCCI2_TwoOpensOneFetch(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	key := writeFile(t, fsys, "f.txt", bytes.Repeat([]byte("x"), 1<<16))
	cs := &contentCounter{Store: store, hold: make(chan struct{})}
	cs.key.Store(key)
	f := attached(t, cs, "")
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			of, err := f.Open(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			if len(readWhole(t, of)) != 1<<16 {
				t.Error("short read")
			}
			of.Close()
		}()
	}
	// Both Opens have made their Head before the first fetch goes on.
	deadline := time.Now().Add(20 * time.Second)
	for cs.heads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(cs.hold)
	wg.Wait()
	if n := cs.reads.Load(); n != 1 {
		t.Fatalf("content reads %d for two Opens at once, want 1", n)
	}
}

// TestCCI3_CacheSurvivesARestart: with a cache directory given (icbfs
// mount's -cache-dir), a file read by one Filesystem is not fetched again
// by the next one on the same directory (the index is read at start).
func TestCCI3_CacheSurvivesARestart(t *testing.T) {
	fsys, store := newTestFilesystem(t)
	key := writeFile(t, fsys, "f.txt", []byte("kept across a restart"))
	dir := filepath.Join(t.TempDir(), "cache")
	cs := &contentCounter{Store: store}
	cs.key.Store(key)

	first := attached(t, cs, dir)
	of, err := first.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	readWhole(t, of)
	of.Close()
	first.Close()
	if n := cs.reads.Load(); n != 1 {
		t.Fatalf("first run: %d content reads, want 1", n)
	}

	second := attached(t, cs, dir)
	of, err = second.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer of.Close()
	if got := readWhole(t, of); string(got) != "kept across a restart" {
		t.Fatalf("read %q", got)
	}
	if n := cs.reads.Load(); n != 1 {
		t.Fatalf("after a restart on the same directory: %d content reads in all, want 1 (none more)", n)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the given directory: %v", err)
	}
}

// TestCCI4_FlushPublishesUnderTheNewVersion: what a flush stored is in
// the cache under the store's new version id: an open of it, in the next
// Filesystem on the directory, fetches nothing.
func TestCCI4_FlushPublishesUnderTheNewVersion(t *testing.T) {
	_, store := newTestFilesystem(t)
	dir := filepath.Join(t.TempDir(), "cache")
	cs := &contentCounter{Store: store}
	writer := attached(t, cs, dir)
	key := writeFile(t, writer, "f.txt", []byte("written here"))
	writer.Close()

	cs.key.Store(key)
	reader := attached(t, cs, dir)
	of, err := reader.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer of.Close()
	if got := readWhole(t, of); string(got) != "written here" {
		t.Fatalf("read %q", got)
	}
	if n := cs.reads.Load(); n != 0 {
		t.Fatalf("content reads %d for an open of the version just flushed, want 0", n)
	}
}

// TestCCE15_IcbfsMountUsesAGroupWritableCacheDir: icbfs mount's
// -cache-dir is exempt from the owner-only check: a directory group and
// other can write still serves (cmd/icbfs only warns).
func TestCCE15_IcbfsMountUsesAGroupWritableCacheDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	fsys, store := newTestFilesystem(t)
	key := writeFile(t, fsys, "f.txt", []byte("hello"))
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	f := attached(t, store, dir)
	of, err := f.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("open through a 0777 -cache-dir: %v", err)
	}
	defer of.Close()
	if got := readWhole(t, of); string(got) != "hello" {
		t.Fatalf("read %q", got)
	}
}
