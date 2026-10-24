package history

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
	"github.com/resurgentech/icbfs/pkg/drive/icbfsdrive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

func pause() { time.Sleep(1100 * time.Millisecond) }

func names(items []drive.Item) string {
	var n []string
	for _, it := range items {
		n = append(n, it.Name)
	}
	return strings.Join(n, ",")
}

// TestHistory: a filesystem's past, as a profile: listings at past
// moments, a file's versions, and search finding what's there and what
// was deleted, with a moment to see it at.
func TestHistory(t *testing.T) {
	store, srv := miniotest.Start(t)
	ctx := context.Background()
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if _, err := icbfs.CreateFilesystem(ctx, store, icbfs.Settings{Name: "h", Size: 1 << 30, RootMode: 0o755, RootUid: uid, RootGid: gid}); err != nil {
		t.Fatal(err)
	}
	live, err := icbfsdrive.New(ctx, icbfsdrive.Config{Store: store, Filesystem: "h", ID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	me := drive.WithProfile(ctx, drive.Profile{POSIX: &drive.POSIXProfile{Uid: uid, Gids: []uint32{gid}}})
	write := func(p, content string) {
		t.Helper()
		if _, err := live.Write(me, p, strings.NewReader(content), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	write("/a.txt", "one")
	pause()
	t1 := time.Now()
	pause()
	write("/a.txt", "two!")
	write("/dir/b.txt", "b")
	pause()
	if err := live.Delete(me, drive.ByPath("/a.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	pause()

	h, err := Open(ctx, Config{Store: pkgicbfs.StoreConfig{S3: pkgicbfs.S3{Endpoint: srv.Endpoint, Bucket: srv.Bucket,
		AccessKey: srv.AccessKey, SecretKey: srv.SecretKey}}, Filesystem: "h", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx = me

	if from, to, err := h.Span(ctx); err != nil || !from.Before(t1) || !to.After(t1) {
		t.Fatalf("span %v to %v (%v) doesn't hold %v", from, to, err, t1)
	}
	then, err := h.List(ctx, "/", t1)
	if err != nil || names(then) != "a.txt" || then[0].Size != 3 {
		t.Fatalf("as of t1: %v, %v; want a.txt of 3 bytes", then, err)
	}
	now, err := h.List(ctx, "/", time.Time{})
	if err != nil || names(now) != "dir" {
		t.Fatalf("now: %v, %v; want dir", names(now), err)
	}

	versions, err := h.Versions(ctx, drive.ByPath("/dir/b.txt"))
	if err != nil || len(versions) != 1 || versions[0].Size != 1 {
		t.Fatalf("versions: %v, %v", versions, err)
	}

	r, err := h.Search(ctx, "A.TXT", 0)
	if err != nil || len(r.Hits) != 1 || !r.Hits[0].Deleted {
		t.Fatalf("search for the deleted file: %+v, %v", r, err)
	}
	if v, err := h.Versions(ctx, drive.ByID(r.Hits[0].ID)); err != nil || len(v) != 2 {
		t.Fatalf("the deleted file's versions, by its id: %v, %v; want 2", v, err)
	}
	seen, err := h.List(ctx, "/", r.Hits[0].See)
	if err != nil || !strings.Contains(names(seen), "a.txt") {
		t.Fatalf("at the hit's moment, %v: %v, %v; want a.txt there", r.Hits[0].See, names(seen), err)
	}
	r, err = h.Search(ctx, "b.", 0)
	if err != nil || len(r.Hits) != 1 || r.Hits[0].Path != "/dir/b.txt" || r.Hits[0].Deleted || !r.Hits[0].See.IsZero() {
		t.Fatalf("search for the current file: %+v, %v", r, err)
	}
}

// countingStore counts the requests that reach the store.
type countingStore struct {
	objstore.Store
	mu       sync.Mutex
	requests int
	log      []string
}

func (c *countingStore) count(what ...string) {
	c.mu.Lock()
	c.requests++
	c.log = append(c.log, strings.Join(what, " "))
	c.mu.Unlock()
}

func (c *countingStore) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func (c *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	c.count("get", key)
	return c.Store.Get(ctx, key)
}

func (c *countingStore) Head(ctx context.Context, key string) (*objstore.Object, error) {
	c.count("head", key)
	return c.Store.Head(ctx, key)
}

func (c *countingStore) GetVersion(ctx context.Context, key, v string) (io.ReadCloser, *objstore.Object, error) {
	c.count("getversion", key)
	return c.Store.GetVersion(ctx, key, v)
}

func (c *countingStore) HeadVersion(ctx context.Context, key, v string) (*objstore.Object, error) {
	c.count("headversion", key)
	return c.Store.HeadVersion(ctx, key, v)
}

func (c *countingStore) History(ctx context.Context, key string) ([]objstore.Object, error) {
	c.count("history", key)
	return c.Store.History(ctx, key)
}

func (c *countingStore) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	c.count("historyunder", prefix)
	return c.Store.HistoryUnder(ctx, prefix)
}

func (c *countingStore) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	c.count("list", prefix)
	return c.Store.List(ctx, prefix)
}

func (c *countingStore) ServerTime(ctx context.Context) (time.Time, error) {
	c.count("servertime")
	return c.Store.ServerTime(ctx)
}

// TestRevisitsNeedNoRequests: once a directory has been listed at past
// moments, listing it at them again takes nothing from the store, in
// this run or the next: what lets a slider through time move fast.
func TestRevisitsNeedNoRequests(t *testing.T) {
	store, srv := miniotest.Start(t)
	ctx := context.Background()
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	if _, err := icbfs.CreateFilesystem(ctx, store, icbfs.Settings{Name: "h", Size: 1 << 30, RootMode: 0o755, RootUid: uid, RootGid: gid}); err != nil {
		t.Fatal(err)
	}
	live, err := icbfsdrive.New(ctx, icbfsdrive.Config{Store: store, Filesystem: "h", ID: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	me := drive.WithProfile(ctx, drive.Profile{POSIX: &drive.POSIXProfile{Uid: uid, Gids: []uint32{gid}}})
	var moments []time.Time
	for i := 0; i < 3; i++ {
		for _, f := range []string{"/x", "/y", "/z"} {
			if _, err := live.Write(me, f, strings.NewReader(strings.Repeat("v", i+1)), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		pause()
		moments = append(moments, time.Now())
		pause()
	}
	// A history fetched now is trusted only to a little before now.
	time.Sleep(2 * time.Second)

	counting := &countingStore{}
	openStore = func(c pkgicbfs.StoreConfig) (objstore.Store, error) {
		s, err := pkgicbfs.Open(c)
		counting.Store = s
		return counting, err
	}
	defer func() { openStore = pkgicbfs.Open }()
	cfg := Config{Store: pkgicbfs.StoreConfig{S3: pkgicbfs.S3{Endpoint: srv.Endpoint, Bucket: srv.Bucket,
		AccessKey: srv.AccessKey, SecretKey: srv.SecretKey}}, Filesystem: "h", CacheDir: t.TempDir()}

	ctx = me
	for session := 0; session < 2; session++ {
		h, err := Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for pass := 0; pass < 2; pass++ {
			for i, at := range moments {
				before := counting.n()
				items, err := h.List(ctx, "/", at)
				if err != nil || names(items) != "x,y,z" || items[0].Size != int64(i+1) {
					t.Fatalf("session %d, pass %d, moment %d: %v, %v", session, pass, i, items, err)
				}
				// The first pass of the first session fetches.
				if n := counting.n() - before; n > 0 && (session > 0 || pass > 0) {
					t.Errorf("session %d, pass %d, moment %d: %d store requests; want 0: %q", session, pass, i, n, counting.log[before:])
				}
			}
		}
		h.Close()
	}
}
