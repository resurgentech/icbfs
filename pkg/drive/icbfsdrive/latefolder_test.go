package icbfsdrive

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// failOnce refuses the next write of key, once.
type failOnce struct {
	objstore.Store
	key atomic.Pointer[string]
}

var errFailOnce = errors.New("the store refused that one")

func (s *failOnce) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if k := s.key.Load(); k != nil && *k == key && s.key.CompareAndSwap(k, nil) {
		return nil, errFailOnce
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

// TestLateFolderWithoutItsTime: a file written into a folder behind the
// folder's latest state, whose write of the folder's modified time fails,
// fails (the time is the folder's history), and the folder is still
// reported late (found late from its dates, not from that write), so its
// settle puts its history right. (On a drive that defers late folders,
// settled through SettleFolder: since T83-1 round 3 only such a drive
// takes a late write.)
func TestLateFolderWithoutItsTime(t *testing.T) {
	store := &failOnce{Store: miniotest.Store(t)}
	d := newDriveWith(t, store, icbfs.Settings{Name: "latefolder", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	ctx := system()
	// /dir made in 2020, a later state in 2022: a file of 2021 is late
	// (one of 2019, older than the folder, would be on time at its first
	// state while it had no later one: T83-8, LF-36).
	for _, w := range []struct {
		y int
		p string
	}{{2020, "/dir/first.txt"}, {2022, "/dir/new.txt"}} {
		if _, err := d.Write(at(ctx, w.y), w.p, strings.NewReader(w.p), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := d.Stat(ctx, drive.ByPath("/dir"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	meta := dir.ID + ".metadata"
	store.key.Store(&meta)
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/old.txt", strings.NewReader("old"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, errFailOnce) {
		t.Fatalf("a late file whose folder's time couldn't be written: %v; want it to fail", err)
	}
	if _, err := settleAt(d, ctx, late.Take(), drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := sortedAt(t, d, "/dir", year(2021).AddDate(0, 6, 0)); got != "first.txt,old.txt" {
		t.Fatalf("/dir as of mid-2021: %q, want first.txt,old.txt", got)
	}
}

// TestLateCreateOnANameTakenThen: a file created late at a name another
// item held at that time, and has since left, doesn't put the name in
// the folder twice. Where the name was taken, the occupant keeps it; the
// late file has it from the state where it was free.
// (Settled through SettleFolder on a drive that defers late folders.)
func TestLateCreateOnANameTakenThen(t *testing.T) {
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "latetaken", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, Config{DeferLateFolders: true})
	ctx := system()
	for _, w := range []struct {
		y          int
		path, body string
	}{{2020, "/dir/x.txt", "first"}, {2022, "/dir/other.txt", "other"}} {
		if _, err := d.Write(at(ctx, w.y), w.path, strings.NewReader(w.body), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Move(at(ctx, 2023), drive.ByPath("/dir/x.txt"), "/dir/aside.txt", false); err != nil {
		t.Fatal(err)
	}
	late := &drive.LateChanges{}
	if _, err := d.Write(at(drive.WithLateChanges(ctx, late), 2021), "/dir/x.txt", strings.NewReader("late"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatalf("a late create at a name taken then: %v", err)
	}
	if _, err := settleAt(d, ctx, late.Take(), drive.SettleOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		when        time.Time
		names, xHas string
	}{
		{year(2021).AddDate(0, 6, 0), "x.txt", "first"},
		{year(2022).AddDate(0, 6, 0), "other.txt,x.txt", "first"},
		{time.Time{}, "aside.txt,other.txt,x.txt", "late"},
	} {
		listed := names(t, d, "/dir", c.when)
		if c.when.IsZero() {
			listed = names(t, d, "/dir", time.Now())
		}
		got := strings.Split(listed, ",")
		sort.Strings(got)
		if strings.Join(got, ",") != c.names {
			t.Errorf("/dir as of %v: %v, want %s", c.when, got, c.names)
		}
		if x := read(t, ctx, d, drive.ByPath("/dir/x.txt"), drive.At{Time: c.when}); x != c.xHas {
			t.Errorf("/dir/x.txt as of %v: %q, want %q", c.when, x, c.xHas)
		}
	}
}
