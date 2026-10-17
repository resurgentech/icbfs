package namecache

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(ttl time.Duration, maxBytes int64, fold func(string) string) (*Cache, *clock) {
	c := New(ttl, maxBytes, fold)
	clk := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	c.now = clk.now
	return c, clk
}

func found(name, id string) Value {
	return Value{Found: true, Entry: block.Entry{Name: name, UUID: id, Type: icbfs.TypeFile}}
}

func TestEntriesExpire(t *testing.T) {
	c, clk := newTest(time.Second, 1<<20, nil)
	c.Put("dir", "a", found("a", "id-a"))
	c.Put("dir", "gone", Value{})
	if v, ok := c.Get("dir", "a"); !ok || v.Entry.UUID != "id-a" {
		t.Fatalf("a: %+v, %v", v, ok)
	}
	if v, ok := c.Get("dir", "gone"); !ok || v.Found {
		t.Fatalf("a cached not-found: %+v, %v", v, ok)
	}
	clk.advance(1001 * time.Millisecond)
	if _, ok := c.Get("dir", "a"); ok {
		t.Fatal("an entry outlived its time to live")
	}
	if c.Bytes() != 0 {
		t.Fatalf("%d bytes left after everything expired and was looked at", c.Bytes())
	}
}

func TestWholeListingAnswersNotFound(t *testing.T) {
	c, clk := newTest(time.Second, 1<<20, nil)
	c.PutListing("dir", []Value{found("a", "id-a"), found("b", "id-b")}, c.Gen())
	if v, ok := c.Get("dir", "desktop.ini"); !ok || v.Found {
		t.Fatalf("a name not in a whole listing: %+v, %v; want known not found", v, ok)
	}
	if _, ok := c.Get("other", "desktop.ini"); ok {
		t.Fatal("another directory's listing answered")
	}
	c.Put("dir", "new", found("new", "id-new")) // created here
	if v, ok := c.Get("dir", "new"); !ok || !v.Found {
		t.Fatalf("a name created after the listing: %+v, %v", v, ok)
	}
	c.Forget("dir", "a")
	if _, ok := c.Get("dir", "zzz"); ok {
		t.Fatal("Forget left the whole listing answering")
	}
	c.PutListing("dir", nil, c.Gen())
	clk.advance(2 * time.Second)
	if _, ok := c.Get("dir", "zzz"); ok {
		t.Fatal("a listing outlived its time to live")
	}
}

func TestFoldedKeys(t *testing.T) {
	c, _ := newTest(time.Second, 1<<20, block.FoldKey)
	c.Put("dir", "Report.docx", found("Report.docx", "id"))
	if v, ok := c.Get("dir", "REPORT.DOCX"); !ok || v.Entry.Name != "Report.docx" {
		t.Fatalf("case-insensitive hit: %+v, %v", v, ok)
	}
	exact, _ := newTest(time.Second, 1<<20, nil)
	exact.Put("dir", "Report.docx", found("Report.docx", "id"))
	if _, ok := exact.Get("dir", "REPORT.DOCX"); ok {
		t.Fatal("an exact cache matched another case")
	}
	// Two stored names that fold together: the first in byte order wins,
	// as a lookup in the store finds it.
	c.PutListing("dir2", []Value{found("b", "id-low"), found("B", "id-up")}, c.Gen())
	if v, _ := c.Get("dir2", "b"); v.Entry.UUID != "id-up" {
		t.Fatalf("folded collision kept %s, want id-up (\"B\" < \"b\")", v.Entry.UUID)
	}
}

func TestForgetItemDropsEveryName(t *testing.T) {
	c, _ := newTest(time.Second, 1<<20, nil)
	c.Put("d1", "a", found("a", "linked"))
	c.Put("d2", "b", found("b", "linked"))
	c.Put("d1", "c", found("c", "other"))
	c.ForgetItem("linked")
	for _, k := range [][2]string{{"d1", "a"}, {"d2", "b"}} {
		if _, ok := c.Get(k[0], k[1]); ok {
			t.Fatalf("%s/%s survived ForgetItem", k[0], k[1])
		}
	}
	if _, ok := c.Get("d1", "c"); !ok {
		t.Fatal("ForgetItem dropped another item")
	}
}

func TestBoundedBySize(t *testing.T) {
	c, _ := newTest(time.Hour, 64<<10, nil)
	for i := range 2000 {
		n := strings.Repeat("x", 20) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))
		c.Put("dir", n, found(n, "id-"+n))
	}
	if b := c.Bytes(); b > 64<<10 {
		t.Fatalf("%d bytes held, over the 64 KiB limit", b)
	}
	// The most recently used survive; the oldest went.
	last := strings.Repeat("x", 20) + string(rune('a'+1999%26)) + string(rune('a'+1999/26%26)) + string(rune('a'+1999/676))
	if _, ok := c.Get("dir", last); !ok {
		t.Fatal("the newest entry was evicted")
	}
	if _, ok := c.Get("dir", strings.Repeat("x", 20)+"aaa"); ok {
		t.Fatal("the oldest entry survived")
	}
	huge := found("big", "id-big")
	huge.Attr.Xattrs = map[string][]byte{"user.big": make([]byte, 32<<10)}
	c.Put("dir", "big", huge)
	if _, ok := c.Get("dir", "big"); ok {
		t.Fatal("an entry bigger than a sixteenth of the cache was kept")
	}
}

func TestConcurrentMissesShareOneLoad(t *testing.T) {
	c, _ := newTest(time.Second, 1<<20, nil)
	var loads atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Lookup("dir", "a", func() (Value, error) {
				loads.Add(1)
				<-release
				return found("a", "id-a"), nil
			})
			if err != nil || v.Entry.UUID != "id-a" {
				t.Errorf("lookup: %+v, %v", v, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Fatalf("%d loads for one name looked up at once, want 1", n)
	}
}

func TestDisabledCachesNothing(t *testing.T) {
	c := New(0, 1<<20, nil)
	c.Put("dir", "a", found("a", "id"))
	if _, ok := c.Get("dir", "a"); ok {
		t.Fatal("a cache with no time to live kept an entry")
	}
	calls := 0
	for range 2 {
		_, _ = c.Lookup("dir", "a", func() (Value, error) { calls++; return Value{}, nil })
	}
	if calls != 2 {
		t.Fatalf("%d loads, want 2: nothing is cached", calls)
	}
	var none *Cache
	if none.Enabled() || none.Bytes() != 0 {
		t.Fatal("a nil cache claims to cache")
	}
}

// TestReadsBeforeAChangeAreNotStored: a lookup or listing that read the
// store before this machine changed a name doesn't store what it read,
// which would hide the change until it expired (winfsp-tests' querydir
// test, creating files as listings ran).
func TestReadsBeforeAChangeAreNotStored(t *testing.T) {
	c, _ := newTest(time.Hour, 1<<20, nil)
	gen := c.Gen()
	c.Put("dir", "new", found("new", "id-new")) // created meanwhile
	c.PutListing("dir", nil, gen)               // a listing read before it, without it
	if v, ok := c.Get("dir", "new"); !ok || !v.Found {
		t.Fatalf("a stale listing hid a name created meanwhile: %+v, %v", v, ok)
	}
	if _, ok := c.Get("dir", "other"); ok {
		t.Fatal("the stale listing was stored")
	}

	v, err := c.Lookup("dir", "late", func() (Value, error) {
		c.Put("dir", "late", found("late", "id-late")) // created while the lookup read
		return Value{}, nil                            // what the store said before
	})
	if err != nil || v.Found {
		t.Fatalf("the lookup's own answer: %+v, %v", v, err)
	}
	if got, ok := c.Get("dir", "late"); !ok || !got.Found {
		t.Fatalf("a lookup read before a create overwrote it: %+v, %v", got, ok)
	}
}
