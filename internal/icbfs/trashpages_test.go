package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// fakeTrash is a store that holds nothing but a trash of n entries, made
// up from their index when asked: so its own memory does not grow with
// n, and what a listing holds is the listing's. It counts the requests a
// real store would take: a listing request a page, a read an object.
// Entry i is live again when live(i) (a directory whose metadata is
// there), and a read of entry failAt fails with a store error.
type fakeTrash struct {
	objstore.Store // nil: only List, the paged listings and Get are asked
	fsid           string
	n              int
	live           func(i int) bool
	failAt         int
	lists, gets    atomic.Int64
	sample         func()
}

func newFakeTrash(n int) *fakeTrash {
	return &fakeTrash{fsid: "fsid", n: n, live: func(int) bool { return false }, failAt: -1}
}

func (s *fakeTrash) prefix() string { return s.fsid + "-trash-" }

func (s *fakeTrash) key(i int) string {
	return fmt.Sprintf("%s%019d-%s", s.prefix(), int64(1e15)+int64(i), s.entryID(i))
}

func (s *fakeTrash) entryID(i int) string { return fmt.Sprintf("e%09d", i) }

func (s *fakeTrash) itemID(i int) string { return fmt.Sprintf("%s-item%09d", s.fsid, i) }

// index is the entry a trash key names.
func (s *fakeTrash) index(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, s.prefix())
	if !ok || len(rest) < 19 {
		return 0, false
	}
	v, err := strconv.ParseInt(rest[:19], 10, 64)
	if err != nil {
		return 0, false
	}
	return int(v - 1e15), true
}

func (s *fakeTrash) objs(from, to int) []objstore.Object {
	page := make([]objstore.Object, 0, to-from)
	for i := from; i < to; i++ {
		page = append(page, objstore.Object{Key: s.key(i), Size: 100})
	}
	return page
}

func (s *fakeTrash) pages(n int) int64 {
	return int64((n + int(objstore.HistoryPageSize) - 1) / int(objstore.HistoryPageSize))
}

func (s *fakeTrash) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	if !strings.HasPrefix(s.prefix(), prefix) {
		return nil, nil
	}
	s.lists.Add(max(1, s.pages(s.n)))
	return s.objs(0, s.n), nil
}

func (s *fakeTrash) ListPages(ctx context.Context, prefix string, fn func([]objstore.Object) error) error {
	return s.ListPagesAfter(ctx, prefix, "", fn)
}

func (s *fakeTrash) ListPagesAfter(ctx context.Context, prefix, after string, fn func([]objstore.Object) error) error {
	if !strings.HasPrefix(s.prefix(), prefix) {
		s.lists.Add(1)
		return nil
	}
	i := sort.Search(s.n, func(i int) bool { return s.key(i) > after })
	if i == s.n {
		s.lists.Add(1)
		return nil
	}
	for i < s.n {
		j := min(s.n, i+int(objstore.HistoryPageSize))
		s.lists.Add(1)
		if s.sample != nil {
			s.sample()
		}
		if err := fn(s.objs(i, j)); err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (s *fakeTrash) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	if n := s.gets.Add(1); s.sample != nil && n%16 == 0 {
		s.sample()
	}
	if i, ok := s.index(key); ok && i >= 0 && i < s.n {
		if i == s.failAt {
			return nil, nil, errors.New("the store did not answer")
		}
		typ := TypeFile
		if s.live(i) {
			typ = TypeDir
		}
		data, err := encodeTrashEntry(TrashEntry{EntryID: s.entryID(i), ItemID: s.itemID(i), Type: typ,
			Name: fmt.Sprintf("f%d", i), Path: fmt.Sprintf("/d/f%d", i), DeletedAt: time.Unix(1e9-int64(i), 0).UTC()})
		if err != nil {
			return nil, nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), &objstore.Object{Key: key, LastModified: time.Unix(1e9, 0)}, nil
	}
	if id, ok := strings.CutSuffix(key, ".metadata"); ok {
		if i, err := strconv.Atoi(strings.TrimPrefix(id, s.fsid+"-item")); err == nil && s.live(i) {
			return io.NopCloser(strings.NewReader("dir")), &objstore.Object{Key: key}, nil
		}
	}
	return nil, nil, fmt.Errorf("get %s: %w", key, objstore.ErrNotFound)
}

func trashFS(t *testing.T, s *fakeTrash) *Filesystem {
	t.Helper()
	f := New(s, "trash-pages")
	f.id = s.fsid
	return f
}

// allPages lists f's trash to its end in pages of limit: the entries in
// order, and how many calls it took.
func allPages(t *testing.T, f *Filesystem, opts TrashOptions) ([]TrashEntry, int) {
	t.Helper()
	var all []TrashEntry
	calls := 0
	for {
		page, next, err := f.ListTrashPage(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		calls++
		if len(page) > opts.Limit {
			t.Fatalf("a page of %d, limit %d", len(page), opts.Limit)
		}
		all = append(all, page...)
		if next == "" {
			return all, calls
		}
		if calls > 100000 {
			t.Fatal("the listing does not end")
		}
		opts.After = next
	}
}

// TestTrashPages_RequestsInStepWithTheTrash (interloc T84, docs/STATES.md
// 4.7, "the read-back after a lost database: one page of the trash at a
// time"): listing a trash of n entries to its end in pages of 50 takes
// store requests in step with n: about two reads an entry (the entry and
// the check that its item is not live) and a listing request a page of
// keys, at 1,000 entries and at 2,000 alike. Before, every page listed the
// trash's keys whole again: n²/(page·keys a request) listing requests.
func TestTrashPages_RequestsInStepWithTheTrash(t *testing.T) {
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 10
	const limit = 50
	per := map[int]float64{}
	for _, n := range []int{1000, 2000} {
		s := newFakeTrash(n)
		f := trashFS(t, s)
		got, calls := allPages(t, f, TrashOptions{Limit: limit})
		if len(got) != n {
			t.Fatalf("%d entries: listed %d", n, len(got))
		}
		reqs := s.lists.Load() + s.gets.Load()
		per[n] = float64(reqs) / float64(n)
		t.Logf("%d entries: %d calls, %d listing requests, %d reads: %.2f requests an entry", n, calls, s.lists.Load(), s.gets.Load(), per[n])
		if per[n] > 2.5 {
			t.Errorf("%d entries: %.2f requests an entry, want at most 2.5 (two reads, a listing request for every %d keys, and a page's look ahead)", n, per[n], objstore.HistoryPageSize)
		}
	}
	if per[2000] > per[1000]*1.1 {
		t.Errorf("requests an entry grow with the trash: %.2f at 1,000, %.2f at 2,000", per[1000], per[2000])
	}
}

// TestTrashPages_HeapOfAPageFlat (interloc T84): one page of the trash,
// the first and the one after it, holds no more at 200,000 entries than
// at 20,000: a page of keys and the page's entries, never the trash's
// whole listing.
func TestTrashPages_HeapOfAPageFlat(t *testing.T) {
	peak := map[int]uint64{}
	for _, n := range []int{20000, 200000} {
		s := newFakeTrash(n)
		f := trashFS(t, s)
		heap := func() uint64 {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			return ms.HeapAlloc
		}
		base := heap()
		var top uint64
		s.sample = func() {
			if h := heap(); h > base && h-base > top {
				top = h - base
			}
		}
		page, next, err := f.ListTrashPage(context.Background(), TrashOptions{Limit: 100})
		if err != nil || len(page) != 100 || next == "" {
			t.Fatalf("%d entries: first page %d entries, next %q, %v", n, len(page), next, err)
		}
		page, _, err = f.ListTrashPage(context.Background(), TrashOptions{Limit: 100, After: next})
		if err != nil || len(page) != 100 || page[0].EntryID != s.entryID(100) {
			t.Fatalf("%d entries: second page %d entries, %v", n, len(page), err)
		}
		peak[n] = top
		t.Logf("%d entries: peak live heap +%d KB over two pages", n, top>>10)
	}
	if a, b := peak[20000], peak[200000]; b > a+512<<10 {
		t.Errorf("a page's heap grows with the trash: +%d KB at 20,000 entries, +%d KB at 200,000", a>>10, b>>10)
	}
}

// TestTrashPages_CarryOnExactlyWhereItStopped (interloc T84): pages of
// any size, put together, are what one listing of everything gives, in
// its order, each entry once; an entry whose item is live again is in
// none of them; a further filter (Keep) counts only what it keeps toward
// a page; and the last page says nothing is left, also when the trash
// ends exactly at a page's end.
func TestTrashPages_CarryOnExactlyWhereItStopped(t *testing.T) {
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 4
	s := newFakeTrash(57)
	s.live = func(i int) bool { return i%7 == 3 }
	f := trashFS(t, s)
	ctx := context.Background()
	for _, keep := range []func(TrashEntry) bool{nil, func(e TrashEntry) bool { return !strings.HasSuffix(e.Name, "5") }} {
		whole, err := f.ListTrash(ctx, TrashOptions{Keep: keep})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range whole {
			if i, _ := strconv.Atoi(strings.TrimPrefix(e.EntryID, "e")); s.live(i) {
				t.Fatalf("listed %s, whose item is live", e.EntryID)
			}
		}
		for _, limit := range []int{1, 3, 4, 10, len(whole), len(whole) + 1} {
			got, calls := allPages(t, f, TrashOptions{Limit: limit, Keep: keep})
			if len(got) != len(whole) {
				t.Fatalf("pages of %d: %d entries, want %d", limit, len(got), len(whole))
			}
			for i := range got {
				if got[i].EntryID != whole[i].EntryID {
					t.Fatalf("pages of %d: entry %d is %s, want %s", limit, i, got[i].EntryID, whole[i].EntryID)
				}
			}
			if want := (len(whole) + limit - 1) / limit; calls != want {
				t.Errorf("pages of %d over %d entries: %d calls, want %d (no empty last page)", limit, len(whole), calls, want)
			}
		}
	}
}

// TestTrashPages_AnOlderCursorCarriesOn (interloc T84): a cursor that is
// an entry's id, as pages gave before (a caller's, kept across an
// upgrade), still carries on after that entry.
func TestTrashPages_AnOlderCursorCarriesOn(t *testing.T) {
	s := newFakeTrash(30)
	f := trashFS(t, s)
	page, next, err := f.ListTrashPage(context.Background(), TrashOptions{Limit: 5, After: s.entryID(9)})
	if err != nil || len(page) != 5 || page[0].EntryID != s.entryID(10) || next == "" {
		t.Fatalf("after %s: %d entries from %v, next %q, %v; want 5 from %s", s.entryID(9), len(page), page, next, err, s.entryID(10))
	}
	rest, _ := allPages(t, f, TrashOptions{Limit: 7, After: next})
	if len(rest) != 15 || rest[0].EntryID != s.entryID(15) {
		t.Fatalf("carried on with %d entries; want the 15 from %s", len(rest), s.entryID(15))
	}
}

// TestTrashPages_AStoreErrorOnAnEntryIsAnError (interloc T84, a row the
// table lacks: a store error on a page): a read of an entry that fails
// for any reason but "not found" ends the listing with that error; it is
// not taken for an entry purged since the listing, which would leave the
// removal out of the read-back without a word (G4).
func TestTrashPages_AStoreErrorOnAnEntryIsAnError(t *testing.T) {
	s := newFakeTrash(20)
	s.failAt = 7
	f := trashFS(t, s)
	page, _, err := f.ListTrashPage(context.Background(), TrashOptions{Limit: 10})
	if err == nil {
		t.Fatalf("a failed read of entry 7: no error, %d entries", len(page))
	}
	if _, err := f.ListTrash(context.Background(), TrashOptions{}); err == nil {
		t.Fatal("a failed read of entry 7: ListTrash answered no error")
	}
	// Gone since the listing: passed over, as before.
	s.failAt = -1
	s.n = 20
	gone := &goneAt{fakeTrash: s, at: 7}
	f = trashFS(t, s)
	f.store = objstore.Recording(gone)
	all, err := f.ListTrash(context.Background(), TrashOptions{})
	if err != nil || len(all) != 19 {
		t.Fatalf("entry 7 gone since the listing: %d entries, %v; want 19", len(all), err)
	}
}

// goneAt is fakeTrash with entry at purged after the listing.
type goneAt struct {
	*fakeTrash
	at int
}

func (g *goneAt) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	if i, ok := g.index(key); ok && i == g.at {
		return nil, nil, fmt.Errorf("get %s: %w", key, objstore.ErrNotFound)
	}
	return g.fakeTrash.Get(ctx, key)
}
