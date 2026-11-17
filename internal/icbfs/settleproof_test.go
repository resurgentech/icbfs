package icbfs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// TestSettleProof: the settle's proof, before anything is written, takes
// a working table only if every state from the earliest change on is the
// state read there with exactly the changes that cover it, and the last
// is the present: a table holding anything else (an entry no change
// says, a change not played or played where it doesn't belong, a present
// the states read don't end in) is ErrSettleRefused.
// STATES: ST-2
func TestSettleProof(t *testing.T) {
	ctx := context.Background()
	cache, err := objstore.OpenHistoryCache(ctx, nil, filepath.Join(t.TempDir(), "history.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	yr := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	ent := func(n string) objstore.SettleEntry {
		return objstore.SettleEntry{Name: n, Item: "id-" + n, Type: int(TypeFile)}
	}
	blk := func(names ...string) []block.Entry {
		var out []block.Entry
		for _, n := range names {
			out = append(out, block.Entry{Name: n, UUID: "id-" + n, Type: TypeFile})
		}
		return out
	}
	// The folder: a from 2020, b from 2022, x written late (dated 2021)
	// into its present at 2024, with c.
	addX := DirChange{Dir: "d", At: yr(2021), Until: yr(2024), Op: ChangeAdd, Name: "x", Item: "id-x", Type: TypeFile}
	build := func(t *testing.T, play func(tab *objstore.SettleTable)) *objstore.SettleTable {
		t.Helper()
		tab, err := cache.SettleTable(ctx, "d")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tab.Clear(ctx) })
		for _, s := range []struct {
			at time.Time
			es []objstore.SettleEntry
		}{
			{yr(2020), []objstore.SettleEntry{ent("a")}},
			{yr(2022), []objstore.SettleEntry{ent("a"), ent("b")}},
			{yr(2024), []objstore.SettleEntry{ent("a"), ent("b"), ent("c"), ent("x")}},
		} {
			if _, err := tab.ReadState(ctx, s.at, s.es, nil, ""); err != nil {
				t.Fatal(err)
			}
		}
		for _, at := range []time.Time{yr(2021), yr(2024)} {
			if err := tab.EnsureState(ctx, at); err != nil {
				t.Fatal(err)
			}
		}
		play(tab)
		return tab
	}
	f := &Filesystem{}
	present := blk("a", "b", "c", "x")
	right := func(tab *objstore.SettleTable) {
		if err := tab.Add(ctx, addX.At, addX.Until, ent("x")); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name    string
		play    func(tab *objstore.SettleTable)
		changes []DirChange
		present []block.Entry
		ok      bool
	}{
		{"as played", right, []DirChange{addX}, present, true},
		{"a change not played", func(*objstore.SettleTable) {}, []DirChange{addX}, present, false},
		{"an entry no change says", func(tab *objstore.SettleTable) {
			right(tab)
			if err := tab.Add(ctx, yr(2021), yr(2022), ent("y")); err != nil {
				t.Fatal(err)
			}
		}, []DirChange{addX}, present, false},
		{"a change played past its until", func(tab *objstore.SettleTable) {
			if err := tab.Add(ctx, addX.At, yr(2030), ent("x")); err != nil {
				t.Fatal(err)
			}
			if err := tab.Remove(ctx, yr(2024), yr(2030), "c", "id-c"); err != nil {
				t.Fatal(err)
			}
		}, []DirChange{addX}, present, false},
		{"a present the states read don't end in", right, []DirChange{addX}, blk("a", "b", "x"), false},
		{"a present with another item under a name", right, []DirChange{addX},
			append(blk("a", "b", "c"), block.Entry{Name: "x", UUID: "id-other", Type: TypeFile}), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tab := build(t, c.play)
			err := f.settleProof(ctx, tab, c.changes, yr(2021), c.present, Attr{})
			if c.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !c.ok && !errors.Is(err, ErrSettleRefused) {
				t.Fatalf("%v, want ErrSettleRefused", err)
			}
			if err != nil {
				t.Logf("%v", err)
			}
		})
	}
}
