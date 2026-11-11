package icbfsdrive

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// requestCounter counts every request that reaches the store, by
// operation and the class of object it names.
type requestCounter struct {
	objstore.Store
	mu sync.Mutex
	n  map[string]int
}

func (c *requestCounter) add(op, key string) {
	class := "content"
	switch {
	case strings.HasSuffix(key, ".metadata"):
		class = ".metadata"
	case strings.HasSuffix(key, ".version"):
		class = ".version"
	case strings.Contains(key, "-origin-"):
		class = "origin"
	case strings.HasSuffix(key, "-root"):
		class = "root"
	case strings.HasSuffix(key, "-past"):
		class = "past"
	case strings.Contains(key, "-trash"):
		class = "trash"
	}
	c.mu.Lock()
	c.n[op+" "+class]++
	c.mu.Unlock()
}

// take returns how many requests were made since the last take, and
// them by operation and class for a failure message. A read of the
// store's clock isn't counted: how many a view of a moment just past
// takes depends on where in the store's second it lands (asof.go waits
// that second out). Nor is a past-changed mark: the drive sends at most
// one a second, whatever the operations (TestPastMarkedOncePerSecond).
func (c *requestCounter) take() (int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.n))
	total := 0
	for k, v := range c.n {
		keys = append(keys, k)
		if !strings.HasPrefix(k, "TIME ") && !strings.HasSuffix(k, " past") {
			total += v
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  %-18s %d", k, c.n[k])
	}
	c.n = map[string]int{}
	return total, b.String()
}

func (c *requestCounter) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	c.add("PUT", key)
	return c.Store.Put(ctx, key, body, md, ifMatch)
}

func (c *requestCounter) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	c.add("PUT-absent", key)
	return c.Store.PutIfAbsent(ctx, key, body, md)
}

func (c *requestCounter) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	c.add("PUT-parts", key)
	return c.Store.PutParts(ctx, key, parts, md, ifMatch)
}

func (c *requestCounter) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	c.add("GET", key)
	return c.Store.Get(ctx, key)
}

func (c *requestCounter) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	c.add("GET-if-changed", key)
	return c.Store.GetIfChanged(ctx, key, etag)
}

func (c *requestCounter) Head(ctx context.Context, key string) (*objstore.Object, error) {
	c.add("HEAD", key)
	return c.Store.Head(ctx, key)
}

func (c *requestCounter) GetVersion(ctx context.Context, key, v string) (io.ReadCloser, *objstore.Object, error) {
	c.add("GET-version", key)
	return c.Store.GetVersion(ctx, key, v)
}

func (c *requestCounter) HeadVersion(ctx context.Context, key, v string) (*objstore.Object, error) {
	c.add("HEAD-version", key)
	return c.Store.HeadVersion(ctx, key, v)
}

func (c *requestCounter) Delete(ctx context.Context, key string) error {
	c.add("DELETE", key)
	return c.Store.Delete(ctx, key)
}

func (c *requestCounter) Undelete(ctx context.Context, key string) error {
	c.add("UNDELETE", key)
	return c.Store.Undelete(ctx, key)
}

func (c *requestCounter) History(ctx context.Context, key string) ([]objstore.Object, error) {
	c.add("HISTORY", key)
	return c.Store.History(ctx, key)
}

func (c *requestCounter) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	c.add("HISTORY-under", prefix)
	return c.Store.HistoryUnder(ctx, prefix)
}

func (c *requestCounter) Erase(ctx context.Context, key, v string) error {
	c.add("ERASE", key)
	return c.Store.Erase(ctx, key, v)
}

func (c *requestCounter) EraseAll(ctx context.Context, prefix string) error {
	c.add("ERASE-all", prefix)
	return c.Store.EraseAll(ctx, prefix)
}

func (c *requestCounter) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	c.add("LIST", prefix)
	return c.Store.List(ctx, prefix)
}

func (c *requestCounter) ServerTime(ctx context.Context) (time.Time, error) {
	c.add("TIME", "(uncounted)")
	return c.Store.ServerTime(ctx)
}

// requestRuns are the drives TestRequestsPerOperation counts on: the
// mirror writing now and in source time, each on a drive that checks
// every folder it walks (TrustFor 0) and on one that trusts what it read
// (TrustFor set, as a daemon that is its filesystem's only writer runs).
var requestRuns = []struct {
	name       string
	sourceTime bool
	trustFor   time.Duration
}{
	{"now", false, 0},
	{"source time", true, 0},
	{"now, trusting", false, time.Hour},
	{"source time, trusting", true, time.Hour},
}

// requestCeilings are the most store requests an operation may take, in
// each of requestRuns. Each is today's count: a change that makes an
// operation cheaper lowers its ceiling here, so the count can't creep
// back.
var requestCeilings = map[string][4]int{
	// The first folder in source time also dates the root, listing its
	// versions so far, and marks the filesystem as written in source time,
	// once.
	"new top-level folder":  {21, 68, 19, 66},
	"new folder at depth 2": {27, 33, 19, 25},
	"new file at depth 3":   {16, 20, 12, 16},
	"the same file again":   {9, 10, 5, 6},
	// And once more: the file's creation wrote its folder, which the
	// drive then read again; a pass over unchanged files reads it once.
	// The origin record and the file's stat, when trusting.
	"the same file, a third time": {9, 10, 3, 4},
	// One of them is the file's lock object, read before the content
	// goes, as a mount's flush reads it (lockCheck).
	"a new version of it": {24, 29, 12, 17},
	// The root, then a step per folder and the file's own stat: two
	// requests each (a folder's are none when trusted), and its .version
	// in source time.
	"stat at depth 3": {8, 9, 2, 3},
	// A listing's cost past its first file, per file.
	"listing, per file": {2, 3, 2, 3},
}

// TestRequestsPerOperation counts the store requests a mirror's
// operations take through Mirror.Apply, and a listing's, and holds each
// to its ceiling in requestCeilings.
func TestRequestsPerOperation(t *testing.T) {
	for i, run := range requestRuns {
		t.Run(run.name, func(t *testing.T) {
			store := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
			d := newDriveWith(t, store, icbfs.Settings{Name: fmt.Sprintf("requests-%d", i), Size: 1 << 30, RootMode: 0o777}, Config{TrustFor: run.trustFor})
			m := drive.NewMirror(d, srcDrive)
			if run.sourceTime {
				m = m.InSourceTime()
			}
			ctx := system()
			jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
			src := &source{}
			store.take()

			check := func(op string, n int, by string) {
				t.Helper()
				ceiling := requestCeilings[op][i]
				switch {
				case n > ceiling:
					t.Errorf("%s: %d requests, ceiling %d:%s", op, n, ceiling, by)
				case n < ceiling:
					t.Logf("%s: %d requests, under the ceiling of %d; lower it", op, n, ceiling)
				}
			}
			counted := func(op string, f func()) {
				t.Helper()
				f()
				n, by := store.take()
				check(op, n, by)
			}
			// A listing after a stat of the folder, so both listings
			// start from the same: the folder just read (and cached).
			listed := func() int {
				t.Helper()
				if _, err := d.Stat(ctx, drive.ByPath("/Clients/Acme"), drive.At{}); err != nil {
					t.Fatal(err)
				}
				store.take()
				if _, err := d.List(ctx, drive.ByPath("/Clients/Acme"), drive.ListOptions{}); err != nil {
					t.Fatal(err)
				}
				n, _ := store.take()
				return n
			}

			dir := drive.SourceItem{Origin: drive.Origin{ID: "d1", Version: "1", Path: "/src/Clients"}, Path: "/Clients", Type: drive.TypeDir, ModTime: jan, Created: jan}
			counted("new top-level folder", func() { apply(t, ctx, m, "folder", dir, drive.Created) })
			sub := drive.SourceItem{Origin: drive.Origin{ID: "d2", Version: "1", Path: "/src/Clients/Acme"}, Path: "/Clients/Acme", Type: drive.TypeDir, ModTime: jan, Created: jan}
			counted("new folder at depth 2", func() { apply(t, ctx, m, "subfolder", sub, drive.Created) })

			f := src.item("f1", "1.0", "/Clients/Acme/report.txt", "v1", jan)
			f.Created = jan
			counted("new file at depth 3", func() { apply(t, ctx, m, "file", f, drive.Created) })
			counted("the same file again", func() { apply(t, ctx, m, "same", f, drive.Unchanged) })
			counted("the same file, a third time", func() { apply(t, ctx, m, "same", f, drive.Unchanged) })
			f2 := src.item("f1", "2.0", "/Clients/Acme/report.txt", "v2", jan.AddDate(0, 1, 0))
			counted("a new version of it", func() { apply(t, ctx, m, "new version", f2, drive.Updated) })

			counted("stat at depth 3", func() {
				if _, err := d.Stat(ctx, drive.ByPath("/Clients/Acme/report.txt"), drive.At{}); err != nil {
					t.Fatal(err)
				}
			})

			one := listed()
			const more = 20
			for j := range more {
				g := src.item(fmt.Sprintf("g%d", j), "1.0", fmt.Sprintf("/Clients/Acme/doc%02d.txt", j), "x", jan)
				g.Created = jan
				apply(t, ctx, m, "another file", g, drive.Created)
			}
			store.take()
			many := listed()
			if (many-one)%more != 0 {
				t.Errorf("a listing of %d files took %d requests, of 1 file %d: not the same per file", 1+more, many, one)
			}
			check("listing, per file", (many-one)/more, "")
		})
	}
}

// TestFindByOriginAfterMove: an item moved through the drive is found by
// its origin without a search of the tree, though its origin record
// still names where it was (a mirror's fold renames an item its members'
// records were written for). The lookup costs the same in a tree of a
// few folders as in one of twenty more: a file moved, and a file whose
// folder moved.
func TestFindByOriginAfterMove(t *testing.T) {
	store := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d := newDriveWith(t, store, icbfs.Settings{Name: "moved", Size: 1 << 30, RootMode: 0o777}, Config{})
	ctx := system()
	setOrigin := func(p, id string) {
		t.Helper()
		if err := d.SetOrigin(ctx, drive.ByPath(p), drive.Origin{Drive: srcDrive, ID: id, Version: "1"}); err != nil {
			t.Fatalf("set origin %s: %v", p, err)
		}
	}
	move := func(from, to string) {
		t.Helper()
		if _, err := d.Move(ctx, drive.ByPath(from), to, false); err != nil {
			t.Fatalf("move %s: %v", from, err)
		}
	}
	lookup := func(id, want string) int {
		t.Helper()
		store.take()
		it, _, err := d.FindByOrigin(ctx, srcDrive, id)
		if err != nil || it.Path != want {
			t.Fatalf("FindByOrigin(%s): %q, %v; want %q", id, it.Path, err, want)
		}
		n, _ := store.take()
		return n
	}
	round := func(r int) (file, inFolder int) {
		write(t, ctx, d, fmt.Sprintf("/in%d/f.txt", r), "f", drive.Condition{})
		setOrigin(fmt.Sprintf("/in%d/f.txt", r), fmt.Sprintf("f%d", r))
		write(t, ctx, d, fmt.Sprintf("/out%d/keep.txt", r), "k", drive.Condition{})
		move(fmt.Sprintf("/in%d/f.txt", r), fmt.Sprintf("/out%d/f.txt", r))
		write(t, ctx, d, fmt.Sprintf("/dir%d/g.txt", r), "g", drive.Condition{})
		setOrigin(fmt.Sprintf("/dir%d/g.txt", r), fmt.Sprintf("g%d", r))
		move(fmt.Sprintf("/dir%d", r), fmt.Sprintf("/moved%d", r))
		return lookup(fmt.Sprintf("f%d", r), fmt.Sprintf("/out%d/f.txt", r)),
			lookup(fmt.Sprintf("g%d", r), fmt.Sprintf("/moved%d/g.txt", r))
	}
	file0, folder0 := round(0)
	for j := range 20 {
		write(t, ctx, d, fmt.Sprintf("/pad%02d/x.txt", j), "x", drive.Condition{})
	}
	file1, folder1 := round(1)
	if file1 != file0 || folder1 != folder0 {
		t.Errorf("found after a move: a file in %d requests, then %d with 20 more folders; a file whose folder moved in %d, then %d", file0, file1, folder0, folder1)
	}
}
