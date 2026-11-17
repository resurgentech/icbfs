package objstore

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func settleCache(t *testing.T, store Store) *HistoryCache {
	t.Helper()
	c, err := OpenHistoryCache(context.Background(), store, filepath.Join(t.TempDir(), "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func tm(m int) time.Time { return time.Date(2020, 1, 1, 0, m, 0, 0, time.UTC) }

func entriesString(es []SettleEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name+":"+e.Item)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestSettleTable: a folder's states read into the working table become
// entry lifetimes; late changes played into it put each change at its
// time and after, a name taken then keeping its occupant; the states
// generated from it are the folder's with the changes. A dir's rows are
// its own: clearing one leaves another's, and a new table for a dir
// starts empty.
func TestSettleTable(t *testing.T) {
	ctx := context.Background()
	c := settleCache(t, clockStore{})

	other, err := c.SettleTable(ctx, "d2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ReadState(ctx, tm(1), []SettleEntry{{Name: "z", Item: "Z", Type: 1}}); err != nil {
		t.Fatal(err)
	}

	tab, err := c.SettleTable(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	a, b := SettleEntry{Name: "a", Item: "A", Type: 1}, SettleEntry{Name: "b", Item: "B", Type: 1}
	for _, s := range []struct {
		at      time.Time
		entries []SettleEntry
		kept    bool
	}{
		{tm(10), []SettleEntry{a}, true},
		{tm(20), []SettleEntry{a, b}, true},
		{tm(30), []SettleEntry{b}, true},
		{tm(35), []SettleEntry{b}, false}, // the same as the one before: collapsed
	} {
		kept, err := tab.ReadState(ctx, s.at, s.entries)
		if err != nil {
			t.Fatal(err)
		}
		if kept != s.kept {
			t.Fatalf("state at %v kept %v, want %v", s.at, kept, s.kept)
		}
	}
	if err := tab.AddBlocks(ctx, tm(10), "k1", "k2"); err != nil {
		t.Fatal(err)
	}
	if err := tab.AddBlocks(ctx, tm(20), "k1"); err != nil {
		t.Fatal(err)
	}
	if err := tab.AddBlocks(ctx, tm(5), "k2"); err != nil {
		t.Fatal(err)
	}
	if err := tab.AddBlocks(ctx, SettlePresent, "k2", "k3"); err != nil {
		t.Fatal(err)
	}
	if blocks, err := tab.Blocks(ctx); err != nil || len(blocks) != 3 ||
		blocks[0] != (SettleBlock{Key: "k1", Last: tm(20)}) || blocks[1] != (SettleBlock{Key: "k2", Last: tm(10), Present: true}) ||
		blocks[2] != (SettleBlock{Key: "k3", Present: true}) {
		t.Fatalf("blocks %+v, %v; want k1 last reached at 20, k2 at 10 and now, k3 now only", blocks, err)
	}

	// Play: c added at 15; a (another item) added at 15 where the name is
	// free, which is from 30 on; b removed at 25.
	play := func() {
		for _, at := range []time.Time{tm(15), tm(25)} {
			if err := tab.EnsureState(ctx, at); err != nil {
				t.Fatal(err)
			}
		}
		if err := tab.Add(ctx, tm(15), SettleEntry{Name: "c", Item: "C", Type: 1}); err != nil {
			t.Fatal(err)
		}
		if err := tab.Add(ctx, tm(15), SettleEntry{Name: "a", Item: "Z", Type: 1}); err != nil {
			t.Fatal(err)
		}
		if err := tab.Remove(ctx, tm(25), "b", "B"); err != nil {
			t.Fatal(err)
		}
		// A removal of another item of the name ends nothing.
		if err := tab.Remove(ctx, tm(15), "a", "Y"); err != nil {
			t.Fatal(err)
		}
	}
	want := map[time.Time]string{
		tm(10): "a:A",
		tm(15): "a:A,c:C",
		tm(20): "a:A,b:B,c:C",
		tm(25): "a:A,c:C",
		tm(30): "a:Z,c:C",
	}
	check := func(what string) {
		t.Helper()
		states, err := tab.States(ctx, tm(15))
		if err != nil {
			t.Fatal(err)
		}
		var times []string
		for _, s := range states {
			times = append(times, s.At.Format("04"))
		}
		if strings.Join(times, ",") != "15,20,25,30" {
			t.Fatalf("%s: states from 15 at %v, want 15,20,25,30", what, times)
		}
		for _, s := range states {
			if s.Read != (s.At.Equal(tm(20)) || s.At.Equal(tm(30))) {
				t.Errorf("%s: state at %v read %v", what, s.At, s.Read)
			}
		}
		for at, w := range want {
			es, err := tab.EntriesAt(ctx, at)
			if err != nil {
				t.Fatal(err)
			}
			if got := entriesString(es); got != w {
				t.Errorf("%s: entries at %v: %s, want %s", what, at.Format("04"), got, w)
			}
		}
	}
	play()
	check("played once")
	play() // a change played again changes nothing
	check("played twice")

	// The sum of a read state is its entries', in any order.
	states, _ := tab.States(ctx, tm(20))
	if states[0].Sum != SettleSum([]SettleEntry{b, a}) {
		t.Errorf("the sum of the state read at 20 isn't its entries'")
	}

	if err := tab.SetNewKeys(ctx, tm(15), []string{"n1", "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := tab.SetEncoded(ctx, tm(15), []byte("root"), []byte("meta")); err != nil {
		t.Fatal(err)
	}
	if root, meta, err := tab.Encoded(ctx, tm(15)); err != nil || string(root) != "root" || string(meta) != "meta" {
		t.Fatalf("encoded %q %q %v", root, meta, err)
	}
	states, _ = tab.States(ctx, tm(15))
	if strings.Join(states[0].NewKeys, ",") != "n1,n2" {
		t.Fatalf("new keys %v", states[0].NewKeys)
	}

	if err := tab.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if states, _ := tab.States(ctx, time.Time{}); len(states) != 0 {
		t.Fatalf("cleared, and %d states left", len(states))
	}
	if es, _ := tab.EntriesAt(ctx, tm(30)); len(es) != 0 {
		t.Fatalf("cleared, and entries left: %v", es)
	}
	if keys, _ := tab.Blocks(ctx); len(keys) != 0 {
		t.Fatalf("cleared, and blocks left: %v", keys)
	}
	if es, _ := other.EntriesAt(ctx, tm(1)); entriesString(es) != "z:Z" {
		t.Fatalf("another dir's rows went with it: %v", es)
	}
	// A new table for a dir starts empty, whatever another settle of it
	// has, and leaves that settle's rows alone.
	if _, err := other.ReadState(ctx, tm(2), []SettleEntry{{Name: "y", Item: "Y", Type: 1}}); err != nil {
		t.Fatal(err)
	}
	again, err := c.SettleTable(ctx, "d2")
	if err != nil {
		t.Fatal(err)
	}
	if states, _ := again.States(ctx, time.Time{}); len(states) != 0 {
		t.Fatalf("a new table for d2 has %d states", len(states))
	}
	if states, _ := other.States(ctx, time.Time{}); len(states) != 2 {
		t.Fatalf("a second table for d2 took the first's rows: %d states left", len(states))
	}
}

// TestSettleTableRunsReaped: a run nobody ended (its process killed) is
// cleared by the next table opened once it is a day old, and not before.
func TestSettleTableRunsReaped(t *testing.T) {
	ctx := context.Background()
	c := settleCache(t, clockStore{})
	left, err := c.SettleTable(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := left.ReadState(ctx, tm(1), []SettleEntry{{Name: "z", Item: "Z", Type: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SettleTable(ctx, "d2"); err != nil {
		t.Fatal(err)
	}
	if es, _ := left.EntriesAt(ctx, tm(1)); entriesString(es) != "z:Z" {
		t.Fatalf("a run of an hour ago cleared: %v", es)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE settle_runs SET started = ? WHERE run = ?`, nanos(time.Now().Add(-25*time.Hour)), left.run); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SettleTable(ctx, "d2"); err != nil {
		t.Fatal(err)
	}
	if es, _ := left.EntriesAt(ctx, tm(1)); len(es) != 0 {
		t.Fatalf("a run a day old kept: %v", es)
	}
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM settle_runs WHERE run = ?`, left.run).Scan(&n); err != nil || n != 0 {
		t.Fatalf("its run row: %d %v", n, err)
	}
}

// lateStore has a key "d" whose versions are dated (d.version) 10, 30
// and 20, stored in that order: the last is late. "d.metadata" has an
// undated version, and is never filtered against d's table.
type lateStore struct {
	clockStore
	dates []byte
}

func (s lateStore) History(_ context.Context, key string) ([]Object, error) {
	old := time.Now().Add(-time.Hour)
	switch key {
	case "d":
		return []Object{
			{Key: "d", VersionID: "late", LastModified: old.Add(3 * time.Second)},
			{Key: "d", VersionID: "v30", LastModified: old.Add(2 * time.Second)},
			{Key: "d", VersionID: "v10", LastModified: old.Add(time.Second)},
		}, nil
	case "d.version":
		return []Object{{Key: "d.version", VersionID: "dv", LastModified: old, Size: int64(len(s.dates))}}, nil
	}
	return nil, nil
}

func (s lateStore) GetVersion(_ context.Context, key, v string) (io.ReadCloser, *Object, error) {
	if key == "d.version" {
		return io.NopCloser(bytes.NewReader(s.dates)), &Object{Key: key, VersionID: v, Size: int64(len(s.dates))}, nil
	}
	return nil, nil, ErrNotFound
}

// TestHistoryWithoutLate: a cache WithoutLate leaves out of a key's
// history the versions stored after one dated later, which a plain view
// shares the database with and still sees.
func TestHistoryWithoutLate(t *testing.T) {
	ctx := context.Background()
	data, err := encodeDates(Dates{"v10": tm(10), "v30": tm(30), "late": tm(20)})
	if err != nil {
		t.Fatal(err)
	}
	c := settleCache(t, lateStore{dates: data})
	at := tm(25)
	pick := func(c *HistoryCache) string {
		v, err := NewAsOf(ctx, c, at)
		if err != nil {
			t.Fatal(err)
		}
		o, ok, err := v.resolve(ctx, "d")
		if err != nil || !ok {
			t.Fatalf("resolve: %v %v", ok, err)
		}
		return o.VersionID
	}
	if got := pick(c); got != "late" {
		t.Fatalf("a plain view at 25 picked %s, want late", got)
	}
	if got := pick(c.WithoutLate()); got != "v10" {
		t.Fatalf("a view without late versions at 25 picked %s, want v10", got)
	}
	if got := pick(c); got != "late" {
		t.Fatalf("the plain cache after: %s, want late", got)
	}
}
