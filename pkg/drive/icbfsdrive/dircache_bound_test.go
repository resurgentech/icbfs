package icbfsdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestDirCacheLRU: the directory cache keeps what it holds under its
// bound in bytes (HeldSize), dropping the least recently used first:
// a directory read again and again (a path's root and ancestors) stays
// while others come and go; one larger than the bound alone is kept,
// alone, until the next.
func TestDirCacheLRU(t *testing.T) {
	big := func(n int) icbfs.Attr {
		return icbfs.Attr{Xattrs: map[string][]byte{"user.pad": make([]byte, n)}}
	}
	one := big(1000).HeldSize() + cachedDirSelf
	bound := 5 * one
	c := newDirCache(0, bound)
	now := time.Now()
	c.put("root", big(1000), now, c.gen("root"))
	for i := range 20 {
		if _, ok := c.get("root"); !ok {
			t.Fatalf("the root went after %d others", i)
		}
		k := fmt.Sprint("d", i)
		c.put(k, big(1000), now, c.gen(k))
		if n, b := c.stats(); b > bound || n > 5 {
			t.Fatalf("after %d puts: %d directories, %d bytes; bound %d", i+1, n, b, bound)
		}
	}
	if _, ok := c.get("d0"); ok {
		t.Fatal("d0, the least recently used, is still there")
	}
	if _, ok := c.get("d19"); !ok {
		t.Fatal("d19, just put, is gone")
	}
	// Over the bound alone: kept alone.
	c.put("huge", big(int(bound)), now, c.gen("huge"))
	if n, _ := c.stats(); n != 1 {
		t.Fatalf("%d directories with one over the bound, want it alone", n)
	}
	if _, ok := c.get("huge"); !ok {
		t.Fatal("one over the bound isn't kept")
	}
	// A put of a key already there replaces it, its bytes too.
	c.put("huge", big(10), now, c.gen("huge"))
	if n, b := c.stats(); n != 1 || b != big(10).HeldSize()+cachedDirSelf {
		t.Fatalf("after replacing: %d, %d bytes", n, b)
	}
	c.drop("huge")
	if n, b := c.stats(); n != 0 || b != 0 {
		t.Fatalf("after the drop: %d, %d bytes", n, b)
	}
}

// dirCacheBound is the bound TestDirCacheBound sets: under what its
// tree's folders take (300 of about 2 KiB each), so the bound is what
// decides.
const dirCacheBound = 256 << 10

// dirCacheMargin is what the heap a dropped cache frees may be over the
// bound by: the bound again. HeldSize is an estimate, under what Go
// holds for a cached folder: measured at 0.6 of it (87 folders cached,
// 261,088 bytes by HeldSize, 434,176 bytes of heap freed by dropping
// them, -race, on a runner), the rest the store client's and the
// allocator's that HeldSize doesn't see. Unbounded, the same rebuild's
// cache freed 753,664 bytes (all 300 folders), over bound and margin.
const dirCacheMargin = dirCacheBound

// TestDirCacheBound (interloc T80-2, the rebuild's growth): after a
// mirror's RebuildHashes over 300 folders three wide x 5 files, the
// drive's directory cache holds no more than its bound by HeldSize, and
// dropping it frees no more heap in use (after a collection) than the
// bound and dirCacheMargin. T80-1 found the cache, bounded only at
// 50,000 folders, holding every folder after a rebuild: 4 to 11 KB
// each, 6.2 MiB at 1,500 folders.
func TestDirCacheBound(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a tree of 1,800 items")
	}
	store := miniotest.Store(t)
	ctx := system()
	s := icbfs.Settings{Name: "dircachebound", Size: 1 << 40, RootMode: 0o777}
	build := newDrive(t, store, s, "")
	fs := build.Filesystem()
	const folders, files = 300, 5
	type dir struct{ key, path string }
	var dirs []dir
	for c := range folders {
		parent := dir{key: fs.RootKey()}
		if c >= 3 {
			parent = dirs[(c-3)/3]
		}
		n := fmt.Sprintf("d%03d-%s", c, strings.Repeat("x", 6))
		key, _, err := fs.Mkdir(ctx, parent.key, n, 0o755, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir{key: key, path: parent.path + "/" + n})
	}
	work := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	for range 32 {
		wg.Go(func() {
			for di := range work {
				dr := dirs[di]
				for f := range files {
					g := di*files + f
					fname := fmt.Sprintf("f%06d-%s.txt", g, strings.Repeat("y", 40))
					data := fmt.Appendf(nil, "content %d\n", g)
					sums, err := icbfs.ChecksumsOf(bytes.NewReader(data))
					var origin []byte
					if err == nil {
						origin, err = json.Marshal(treeOrigin(g, 1, dr.path+"/"+fname))
					}
					if err == nil {
						info := icbfs.ItemInfo{App: map[string][]byte{icbfs.AppDriveOrigin: origin}, Checksums: &sums}
						_, _, err = fs.CreateWith(ctx, dr.key, fname, 0o644, 0, 0, data, nil, info)
					}
					if err != nil {
						mu.Lock()
						failed = fmt.Errorf("%s/%s: %w", dr.path, fname, err)
						mu.Unlock()
						return
					}
				}
			}
		})
	}
	for di := range dirs {
		work <- di
	}
	close(work)
	wg.Wait()
	if failed != nil {
		t.Fatal(failed)
	}

	// A fresh drive, its cache cold, bounded.
	d := newDriveWith(t, store, s, Config{DirCacheBytes: dirCacheBound})
	m := drive.NewMirror(statVersions{d}, srcDrive)
	before := heapInUse()
	if err := m.RebuildHashes(ctx); err != nil {
		t.Fatal(err)
	}
	withCache := heapInUse()
	n, est := d.dirs.stats()
	d.dirs.dropAll()
	without := heapInUse()
	freed := int64(withCache) - int64(without)
	t.Logf("after a rebuild over %d folders x %d files: %d directories cached, %d bytes by HeldSize (bound %d); heap in use before %s, with the cache %s, without %s: the cache held %d bytes (%.0f a folder cached)",
		folders, files, n, est, dirCacheBound, mib(before), mib(withCache), mib(without), freed, float64(freed)/float64(max(n, 1)))
	if entries := m.Hashes().Len(); entries != folders*files {
		t.Fatalf("the map has %d entries, want %d", entries, folders*files)
	}
	if est > dirCacheBound {
		t.Errorf("the cache holds %d bytes by HeldSize, over its bound %d", est, dirCacheBound)
	}
	if freed > dirCacheBound+dirCacheMargin {
		t.Errorf("dropping the cache freed %d bytes of heap, over the bound %d and the margin %d", freed, dirCacheBound, dirCacheMargin)
	}
	runtime.KeepAlive(m)
}

// TestDirCacheRequests: for a tree that fits the default bound, a walk
// of it and a Stat of each file, done twice, make as many store requests
// the second time as with a cache bounded by nothing: the bound doesn't
// make a mount ask again for what it walked a moment ago. Both with
// TrustFor (no requests for a folder within it) and without (two
// conditional reads a folder).
func TestDirCacheRequests(t *testing.T) {
	store := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	ctx := system()
	s := icbfs.Settings{Name: "dircachereq", Size: 1 << 30, RootMode: 0o777}
	w := newDrive(t, store, s, "")
	var paths []string
	for a := range 3 {
		for b := range 3 {
			for f := range 4 {
				p := fmt.Sprintf("/a%d/b%d/f%d.txt", a, b, f)
				write(t, ctx, w, p, "content of "+p, drive.Condition{})
				paths = append(paths, p)
			}
		}
	}
	pass := func(d *Drive) {
		t.Helper()
		if err := drive.Walk(ctx, d, drive.ByPath("/"), drive.WalkOptions{}, func(drive.ItemRef, []drive.Item) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			if _, err := d.Stat(ctx, drive.ByPath(p), drive.At{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, trust := range []time.Duration{0, time.Hour} {
		got := map[int64]int{}
		for _, bound := range []int64{0, 1 << 40} {
			d := newDriveWith(t, store, s, Config{TrustFor: trust, DirCacheBytes: bound})
			pass(d) // warm
			store.take()
			pass(d)
			n, by := store.take()
			got[bound] = n
			t.Logf("TrustFor %v, bound %d: the second pass took %d requests:%s", trust, bound, n, by)
		}
		if got[0] > got[1<<40] {
			t.Errorf("TrustFor %v: the second pass took %d requests with the default bound, %d unbounded", trust, got[0], got[1<<40])
		}
	}
}

// statVersions is the drive, with a file's one version from a Stat: it
// resolves the file's path as Versions does (resolve), without a history
// listing (tens of milliseconds on MinIO).
type statVersions struct{ *Drive }

func (s statVersions) Versions(ctx context.Context, ref drive.ItemRef) ([]drive.Version, error) {
	it, err := s.Drive.Stat(ctx, ref, drive.At{})
	if err != nil || it.Version == nil {
		return nil, err
	}
	v := *it.Version
	v.Current = true
	if v.Checksums == nil {
		v.Checksums = it.Checksums
	}
	return []drive.Version{v}, nil
}
