package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// trashCounter counts a store's reads and listing requests (a page a
// request, and one for a listing that answers nothing), passing its
// paged listings through.
type trashCounter struct {
	objstore.Store
	gets, lists atomic.Int64
}

func (c *trashCounter) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, key)
}

func (c *trashCounter) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	objs, err := c.Store.List(ctx, prefix)
	c.lists.Add(int64(len(objs))/int64(objstore.HistoryPageSize) + 1)
	return objs, err
}

func (c *trashCounter) ListPages(ctx context.Context, prefix string, fn func([]objstore.Object) error) error {
	return c.ListPagesAfter(ctx, prefix, "", fn)
}

func (c *trashCounter) ListPagesAfter(ctx context.Context, prefix, after string, fn func([]objstore.Object) error) error {
	c.lists.Add(1)
	pages := 0
	return objstore.ListEachAfter(ctx, c.Store, prefix, after, func(p []objstore.Object) error {
		if pages++; pages > 1 {
			c.lists.Add(1)
		}
		return fn(p)
	})
}

func (c *trashCounter) requests() int64 { return c.gets.Load() + c.lists.Load() }

// trashedDrive is a mirror drive whose trash holds n files copied from
// drive "src" and one made on the drive itself.
func trashedDrive(t *testing.T, store objstore.Store, name string, n int) *Drive {
	t.Helper()
	d := newDrive(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	copied := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for i := range n {
		p := fmt.Sprintf("/docs/f%03d.txt", i)
		write(t, ctx, d, p, "x", drive.Condition{})
		if err := d.SetOrigin(ctx, drive.ByPath(p), drive.Origin{Drive: "src", ID: fmt.Sprintf("s%03d", i), Path: p, Copied: copied}); err != nil {
			t.Fatal(err)
		}
		if err := d.Delete(ctx, drive.ByPath(p), false, drive.Condition{}); err != nil {
			t.Fatal(err)
		}
	}
	write(t, ctx, d, "/local.txt", "made here", drive.Condition{})
	if err := d.Delete(ctx, drive.ByPath("/local.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	return d
}

// trashToTheEnd pages d's trash of src's items to the end, limit at a
// time; between the first page and the second, between(ids of what
// comes after the first page) runs if set.
func trashToTheEnd(t *testing.T, d *Drive, limit int, between func(rest []drive.TrashEntry)) ([]drive.TrashEntry, int) {
	t.Helper()
	ctx := system()
	var all []drive.TrashEntry
	calls := 0
	for cursor := ""; ; {
		page, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "src", Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		calls++
		if len(page.Entries) > limit {
			t.Fatalf("a page of %d, limit %d", len(page.Entries), limit)
		}
		all = append(all, page.Entries...)
		if page.Next == "" {
			return all, calls
		}
		if calls == 1 && between != nil {
			rest, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "src", Cursor: page.Next})
			if err != nil {
				t.Fatal(err)
			}
			between(rest.Entries)
		}
		cursor = page.Next
	}
}

// TestLK24_TheDrivePassesTheLimitDown (interloc T84, docs/STATES.md
// 4.7: the read-back after a lost database reads one page of the trash
// at a time): a mirror's trash of n of its source's items, paged 5 at a
// time by origin to the end, takes store requests in step with n (the
// same requests an entry at 20 and at 40 entries), and the pages are
// what one listing of everything gives, each entry once, in its order.
// Before, each page listed the trash whole and read every entry after
// the cursor: requests grew with n².
func TestLK24_TheDrivePassesTheLimitDown(t *testing.T) {
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 4
	store := miniotest.Store(t)
	per := map[int]float64{}
	for _, n := range []int{20, 40} {
		c := &trashCounter{Store: store}
		d := trashedDrive(t, c, fmt.Sprintf("trash%d", n), n)
		whole, err := d.Trash(system(), drive.TrashOptions{OriginDrive: "src"})
		if err != nil || len(whole.Entries) != n || whole.Next != "" {
			t.Fatalf("%d entries: everything at once gave %d, next %q, %v", n, len(whole.Entries), whole.Next, err)
		}
		before := c.requests()
		got, calls := trashToTheEnd(t, d, 5, nil)
		reqs := c.requests() - before
		per[n] = float64(reqs) / float64(n)
		t.Logf("%d entries: %d pages, %d requests, %.2f an entry", n, calls, reqs, per[n])
		if len(got) != n || calls != n/5 {
			t.Fatalf("%d entries: %d in %d pages, want %d in %d", n, len(got), calls, n, n/5)
		}
		for i := range got {
			if got[i].ID != whole.Entries[i].ID {
				t.Fatalf("%d entries: page entry %d is %s, want %s", n, i, got[i].Path, whole.Entries[i].Path)
			}
		}
	}
	if per[40] > per[20]*1.15 {
		t.Errorf("requests an entry grow with the trash: %.2f at 20 entries, %.2f at 40", per[20], per[40])
	}
}

// TestSP12_ChangesBetweenTwoPages (interloc T84, the events a
// paged listing meets): between two pages, an entry newer than the
// cursor is added (a delete), one after it is restored and one is
// purged. The pages that follow list none of the three and repeat
// nothing: an entry added sorts before the cursor (the trash is newest
// first), so this listing misses it; one restored or purged is gone.
// Everything else is listed once.
func TestSP12_ChangesBetweenTwoPages(t *testing.T) {
	store := miniotest.Store(t)
	d := trashedDrive(t, store, "between", 12)
	ctx := system()
	var restored, purged string
	got, _ := trashToTheEnd(t, d, 4, func(rest []drive.TrashEntry) {
		write(t, ctx, d, "/docs/new.txt", "n", drive.Condition{})
		if err := d.SetOrigin(ctx, drive.ByPath("/docs/new.txt"), drive.Origin{Drive: "src", ID: "snew", Path: "/docs/new.txt"}); err != nil {
			t.Fatal(err)
		}
		if err := d.Delete(ctx, drive.ByPath("/docs/new.txt"), false, drive.Condition{}); err != nil {
			t.Fatal(err)
		}
		restored, purged = rest[1].ID, rest[5].ID
		if _, err := d.Restore(ctx, restored, drive.RestoreOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := d.Purge(ctx, drive.ByID(rest[5].ItemID), false); err != nil {
			t.Fatal(err)
		}
	})
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.ID] {
			t.Fatalf("%s listed twice", e.Path)
		}
		seen[e.ID] = true
		if e.ID == restored || e.ID == purged || e.Path == "/docs/new.txt" {
			t.Fatalf("%s listed: restored, purged or added after the cursor", e.Path)
		}
	}
	if len(got) != 10 {
		t.Fatalf("%d listed, want the 12 less the restored and the purged", len(got))
	}
}

// TestLK24_TheDriveRefusesAMalformedCursor (interloc T84, docs/STATES.md
// LK-24): the drive's trash listing refuses a cursor it never gave as
// invalid (drive.ErrInvalid: the Drive API's 400), and still takes one it
// gave and an entry id, as pages gave before.
func TestLK24_TheDriveRefusesAMalformedCursor(t *testing.T) {
	store := miniotest.Store(t)
	d := trashedDrive(t, store, "badcursor", 6)
	ctx := system()
	first, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "src", Limit: 2})
	if err != nil || first.Next == "" {
		t.Fatal(first.Next, err)
	}
	if _, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "src", Limit: 2, Cursor: "not a cursor"}); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("a malformed cursor: %v, want drive.ErrInvalid", err)
	}
	for _, c := range []string{first.Next, first.Entries[1].ID} {
		page, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "src", Limit: 2, Cursor: c})
		if err != nil || len(page.Entries) != 2 || page.Entries[0].ID == first.Entries[0].ID || page.Entries[0].ID == first.Entries[1].ID {
			t.Fatalf("cursor %q: %+v, %v; want the next two", c, page, err)
		}
	}
}
