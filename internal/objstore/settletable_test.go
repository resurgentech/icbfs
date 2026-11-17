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
	if _, err := other.ReadState(ctx, tm(1), []SettleEntry{{Name: "z", Item: "Z", Type: 1}}, nil, ""); err != nil {
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
		kept, err := tab.ReadState(ctx, s.at, s.entries, nil, "")
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
		// Until past every state: into every state from the change on.
		if err := tab.Add(ctx, tm(15), tm(40), SettleEntry{Name: "c", Item: "C", Type: 1}); err != nil {
			t.Fatal(err)
		}
		if err := tab.Add(ctx, tm(15), tm(40), SettleEntry{Name: "a", Item: "Z", Type: 1}); err != nil {
			t.Fatal(err)
		}
		if err := tab.Remove(ctx, tm(25), tm(40), "b", "B"); err != nil {
			t.Fatal(err)
		}
		// A removal of another item of the name ends nothing.
		if err := tab.Remove(ctx, tm(15), tm(40), "a", "Y"); err != nil {
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
	// The states as read are kept as they were.
	for at, w := range map[time.Time]string{tm(10): "a:A", tm(15): "a:A", tm(20): "a:A,b:B", tm(25): "a:A,b:B", tm(30): "b:B"} {
		es, err := tab.ReadEntriesAt(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if got := entriesString(es); got != w {
			t.Errorf("read entries at %v: %s, want %s", at.Format("04"), got, w)
		}
	}

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
	if _, err := other.ReadState(ctx, tm(2), []SettleEntry{{Name: "y", Item: "Y", Type: 1}}, nil, ""); err != nil {
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

// TestSettleTableRunsReaped: a run nobody ended that this machine can't
// judge (another machine's) is cleared by the next table opened once it
// is a day old, and not before; a live run of this process never is
// (ST-26).
func TestSettleTableRunsReaped(t *testing.T) {
	ctx := context.Background()
	c := settleCache(t, clockStore{})
	left, err := c.SettleTable(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := left.ReadState(ctx, tm(1), []SettleEntry{{Name: "z", Item: "Z", Type: 1}}, nil, ""); err != nil {
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
	// Its process (this one) is alive and its settle holds it: kept,
	// however old (ST-26).
	if es, _ := left.EntriesAt(ctx, tm(1)); entriesString(es) != "z:Z" {
		t.Fatalf("a live run a day old cleared: %v", es)
	}
	// Left by another machine, which this one can't judge: a day old, it goes.
	if _, err := c.db.ExecContext(ctx, `UPDATE settle_runs SET host = 'another-machine' WHERE run = ?`, left.run); err != nil {
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

// TestSettleTableUntil: a change is played into the states in [at,
// until) only: an add stops at until (the late write put it into the
// states from until on itself), and leaves a later removal of the name
// alone; a removal of a lifetime that goes on past until keeps its part
// from until on; until must be after at.
func TestSettleTableUntil(t *testing.T) {
	ctx := context.Background()
	c := settleCache(t, clockStore{})
	tab, err := c.SettleTable(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	a, x := SettleEntry{Name: "a", Item: "A", Type: 1}, SettleEntry{Name: "x", Item: "X", Type: 1}
	// Read: a from 10; x, written into the present at 30 by a late write
	// dated 15, from 30, and removed on time at 40.
	for _, s := range []struct {
		at      time.Time
		entries []SettleEntry
	}{{tm(10), []SettleEntry{a}}, {tm(20), []SettleEntry{a}}, {tm(30), []SettleEntry{a, x}}, {tm(40), []SettleEntry{a}}} {
		if _, err := tab.ReadState(ctx, s.at, s.entries, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Time{tm(15), tm(25)} {
		if err := tab.EnsureState(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	// The late add of x at 15 until 30; a late removal of a at 25 until 30
	// (a goes on, read, from 30).
	if err := tab.Add(ctx, tm(15), tm(30), x); err != nil {
		t.Fatal(err)
	}
	if err := tab.Remove(ctx, tm(25), tm(30), "a", "A"); err != nil {
		t.Fatal(err)
	}
	want := map[time.Time]string{
		tm(10): "a:A", tm(15): "a:A,x:X", tm(20): "a:A,x:X", tm(25): "x:X", tm(30): "a:A,x:X", tm(40): "a:A",
	}
	for at, w := range want {
		es, err := tab.EntriesAt(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if got := entriesString(es); got != w {
			t.Errorf("entries at %v: %s, want %s", at.Format("04"), got, w)
		}
	}
	if err := tab.Add(ctx, tm(15), tm(15), x); err == nil {
		t.Errorf("an add with until at its time: no error")
	}
	if err := tab.Remove(ctx, tm(15), tm(10), "a", "A"); err == nil {
		t.Errorf("a removal with until before its time: no error")
	}
}

// TestHistoryUpTo: a cache UpTo leaves out of a key's history the
// versions stored after the one it names, and nothing else (a version
// stored after one dated later stays: a view of its date sees it); a
// plain view shares the database and sees everything.
func TestHistoryUpTo(t *testing.T) {
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
	if got := pick(c.UpTo(nil)); got != "late" {
		t.Fatalf("a view up to nothing at 25 picked %s, want late (nothing inferred)", got)
	}
	// Up to v10: v30 and late, stored after it, are left out; at 35 only
	// v10 is there.
	at = tm(35)
	if got := pick(c.UpTo(map[string]string{"d": "v10"})); got != "v10" {
		t.Fatalf("a view up to v10 at 35 picked %s, want v10", got)
	}
	if got := pick(c); got != "v30" {
		t.Fatalf("the plain cache after: %s, want v30", got)
	}
}

// TestNotStates: a version with a newer one dated alike is shadowed, and
// lateness is judged without the shadowed: a settle's new states, stored
// after the old ones they shadow, are not late; an undated version is
// dated when it was stored, and is a state.
func TestNotStates(t *testing.T) {
	stored := time.Now().Add(-time.Hour)
	at := func(s int) time.Time { return stored.Add(time.Duration(s) * time.Second) }
	// Old states 10, 20, 30 stored in order; then a settle's new 15, 20,
	// 30 (newest first in the list); then an undated version.
	h := []Object{
		{VersionID: "undated", LastModified: at(7)},
		{VersionID: "n30", LastModified: at(6)},
		{VersionID: "n20", LastModified: at(5)},
		{VersionID: "n15", LastModified: at(4)},
		{VersionID: "o30", LastModified: at(3)},
		{VersionID: "o20", LastModified: at(2)},
		{VersionID: "o10", LastModified: at(1)},
	}
	d := Dates{"o10": tm(10), "o20": tm(20), "o30": tm(30), "n15": tm(15), "n20": tm(20), "n30": tm(30)}
	shadowed, late := NotStates(h, d)
	if len(shadowed) != 2 || !shadowed["o20"] || !shadowed["o30"] {
		t.Errorf("shadowed %v, want o20 and o30", shadowed)
	}
	if len(late) != 0 {
		t.Errorf("late %v, want none", late)
	}
	// Before the new states: a late write's version, stored after o30.
	h = []Object{{VersionID: "lw", LastModified: at(4)}, h[4], h[5], h[6]}
	d["lw"] = tm(15)
	if shadowed, late = NotStates(h, d); len(shadowed) != 0 || len(late) != 1 || !late["lw"] {
		t.Errorf("shadowed %v late %v, want lw late", shadowed, late)
	}
	// A settled folder (s15, s20, s30), then a second late write at 15:
	// its version shadows nothing (the settled state at 15 stays one), and
	// it is late.
	h = []Object{
		{VersionID: "lw2", LastModified: at(9)},
		{VersionID: "s30", LastModified: at(8)},
		{VersionID: "s20", LastModified: at(7)},
		{VersionID: "s15", LastModified: at(6)},
		{VersionID: "o10", LastModified: at(1)},
	}
	d = Dates{"o10": tm(10), "s15": tm(15), "s20": tm(20), "s30": tm(30), "lw2": tm(15)}
	if shadowed, late = NotStates(h, d); len(shadowed) != 0 || len(late) != 1 || !late["lw2"] {
		t.Errorf("shadowed %v late %v, want lw2 late and nothing shadowed", shadowed, late)
	}
}
