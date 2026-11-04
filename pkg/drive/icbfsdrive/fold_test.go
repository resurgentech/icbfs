package icbfsdrive

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// foldDrive is a drive and a mirror on it: with icbfs's origin index, or,
// not indexed, behind plainDrive (the generic search).
func foldDrive(t *testing.T, name string, indexed bool) (*Drive, drive.Drive, *drive.Mirror) {
	t.Helper()
	idrv := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777}, "")
	var d drive.Drive = idrv
	if !indexed {
		d = plainDrive{idrv}
	}
	return idrv, d, drive.NewMirror(d, srcDrive)
}

// eachIndex runs f with icbfs's origin index, then with the search.
func eachIndex(t *testing.T, f func(t *testing.T, indexed bool)) {
	for _, indexed := range []bool{true, false} {
		name := "indexed"
		if !indexed {
			name = "searched"
		}
		t.Run(name, func(t *testing.T) { f(t, indexed) })
	}
}

// into is it said to have the same name in the same folder as the source
// item winner (SourceItem.FoldInto), and to belong at foldPath.
func into(it drive.SourceItem, winner, foldPath string) drive.SourceItem {
	it.FoldInto, it.FoldPath = winner, foldPath
	return it
}

// aliasIDs is the ids of an origin's aliases, in order.
func aliasIDs(o *drive.Origin) []string {
	if o == nil {
		return nil
	}
	var ids []string
	for _, a := range o.Aliases {
		ids = append(ids, a.ID)
	}
	return ids
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// emptyTrash fails if anything is in the trash.
func emptyTrash(t *testing.T, ctx context.Context, d drive.Drive) {
	t.Helper()
	page, err := d.Trash(ctx, drive.TrashOptions{})
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("the trash holds %+v (%v), want nothing", page.Entries, err)
	}
}

// TestMirrorFold: a source item with the same name in the same folder as
// an older one, and the same bytes, is folded into it: an alias on its
// item, no item of its own. With a source hash the mirror knows, nothing
// is downloaded. FoldPath moves the item, history and origin kept.
func TestMirrorFold(t *testing.T) {
	eachIndex(t, func(t *testing.T, indexed bool) {
		_, d, m := foldDrive(t, "fold", indexed)
		ctx := system()
		src := &source{}
		jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
		x := mirrorData("x")

		w := apply(t, ctx, m, "winner", src.hashed("w", "1", "/Docs/a.txt", x, jan), drive.Created)
		opens := src.opens.Load()
		f := apply(t, ctx, m, "folded", into(src.hashed("a1", "7", "/Docs/a (2).txt", x, jan.Add(time.Hour)), "w", ""), drive.Folded)
		if src.opens.Load() != opens {
			t.Fatal("a fold with a known source hash downloaded the bytes")
		}
		if f.Item.ID != w.Item.ID || f.Item.Path != "/Docs/a.txt" || f.Item.Origin == nil || f.Item.Origin.ID != "w" {
			t.Fatalf("folded into %+v (origin %+v), want the winner's item, where it was", f.Item, f.Item.Origin)
		}
		if got := aliasIDs(f.Item.Origin); !sameIDs(got, []string{"a1"}) {
			t.Fatalf("aliases %v, want [a1]", got)
		}
		if f.Item.Origin.Aliases[0].Version != "7" {
			t.Fatalf("alias %+v, want version 7", f.Item.Origin.Aliases[0])
		}
		if _, err := d.Stat(ctx, drive.ByPath("/Docs/a (2).txt"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
			t.Fatalf("the alias's own path: %v, want nothing there", err)
		}
		vs, err := d.Versions(ctx, drive.ByID(w.Item.ID))
		if err != nil || len(vs) != 1 {
			t.Fatalf("versions %d, %v: a fold writes no version", len(vs), err)
		}
		// Again: nothing to do.
		apply(t, ctx, m, "folded again", into(src.hashed("a1", "7", "/Docs/a (2).txt", x, jan.Add(time.Hour)), "w", ""), drive.Unchanged)
		// The winner applied as it is: unchanged, its aliases kept.
		u := apply(t, ctx, m, "winner again", src.hashed("w", "1", "/Docs/a.txt", x, jan), drive.Unchanged)
		if got := aliasIDs(u.Item.Origin); !sameIDs(got, []string{"a1"}) {
			t.Fatalf("aliases after the winner's apply %v, want [a1]", got)
		}

		// Without a source hash, the bytes are read and compared.
		opens = src.opens.Load()
		apply(t, ctx, m, "folded, downloaded", into(src.item("a2", "1", "/Docs/a (3).txt", x, jan.Add(2*time.Hour)), "w", ""), drive.Folded)
		if src.opens.Load() != opens+1 {
			t.Fatalf("a fold without a source hash opened the source %d times, want once", src.opens.Load()-opens)
		}

		// FoldPath: the item moves there, once, history and origin kept.
		mv := apply(t, ctx, m, "folded and moved", into(src.item("a3", "1", "/Docs/a (4).txt", x, jan.Add(3*time.Hour)), "w", "/Docs/a (4).txt"), drive.Folded, drive.Moved)
		if mv.Item.ID != w.Item.ID || mv.Item.Path != "/Docs/a (4).txt" || mv.Item.Origin.ID != "w" || mv.Item.Origin.Version != "1" {
			t.Fatalf("moved %+v (origin %+v)", mv.Item, mv.Item.Origin)
		}
		if got := aliasIDs(mv.Item.Origin); !sameIDs(got, []string{"a1", "a2", "a3"}) {
			t.Fatalf("aliases %v, want [a1 a2 a3]", got)
		}
		if vs, err := d.Versions(ctx, drive.ByID(w.Item.ID)); err != nil || len(vs) != 1 {
			t.Fatalf("versions after the move %d, %v; want the one", len(vs), err)
		}
		if got := read(t, ctx, d, drive.ByPath("/Docs/a (4).txt"), drive.At{}); got != x {
			t.Fatalf("the folded item reads %q", got[:min(len(got), 30)])
		}
		emptyTrash(t, ctx, d)

		// Other bytes: no fold, an item of its own.
		o := apply(t, ctx, m, "not the same bytes", into(src.item("b", "1", "/Docs/a (5).txt", x+"!", jan), "w", ""), drive.Created)
		if o.Item.ID == w.Item.ID || o.Item.Path != "/Docs/a (5).txt" {
			t.Fatalf("other bytes: %+v", o.Item)
		}
		// A winner the drive doesn't hold: no fold.
		apply(t, ctx, m, "no winner", into(src.item("c", "1", "/Docs/c (2).txt", x, jan), "nobody", ""), drive.Created)
	})
}

// TestMirrorFindByAlias: an alias's id finds the winner's item, which
// says the id asked was an alias of its origin.
func TestMirrorFindByAlias(t *testing.T) {
	eachIndex(t, func(t *testing.T, indexed bool) {
		idrv, _, m := foldDrive(t, "fold-find", indexed)
		ctx := system()
		src := &source{}
		x := mirrorData("x")
		w := apply(t, ctx, m, "winner", src.item("w", "1", "/a.txt", x, time.Time{}), drive.Created)
		apply(t, ctx, m, "folded", into(src.item("a1", "1", "/a (2).txt", x, time.Time{}), "w", ""), drive.Folded)

		it, trashed, found, err := m.Known(ctx, "a1")
		if err != nil || !found || trashed != nil || it.ID != w.Item.ID || it.Alias != "a1" || it.Origin == nil || it.Origin.ID != "w" {
			t.Fatalf("known a1: %+v (origin %+v), trashed %v, found %v, %v", it, it.Origin, trashed, found, err)
		}
		it, _, _, err = m.Known(ctx, "w")
		if err != nil || it.ID != w.Item.ID || it.Alias != "" {
			t.Fatalf("known w: %+v, %v; want the item, not as an alias", it, err)
		}
		if indexed {
			it, _, err := idrv.FindByOrigin(ctx, srcDrive, "a1")
			if err != nil || it.ID != w.Item.ID || it.Alias != "a1" {
				t.Fatalf("FindByOrigin(a1): %+v, %v", it, err)
			}
			rec, _, err := idrv.fs.OriginRecord(ctx, srcDrive, "a1")
			if err != nil || !rec.Alias || rec.ItemID != w.Item.ID || rec.Version != "1" {
				t.Fatalf("a1's record %+v, %v; want an alias naming the winner's item", rec, err)
			}
			if rec, _, err := idrv.fs.OriginRecord(ctx, srcDrive, "w"); err != nil || rec.Alias {
				t.Fatalf("w's record %+v, %v; want no alias", rec, err)
			}
		}
	})
}

// TestMirrorUnfold: a fold holds only while the name, folder and bytes
// agree. A new version with other bytes, a rename (FoldInto gone), or the
// winner's bytes changing unfolds the alias: its own item at its own
// path, by bytes, or by a stub of a version the drive has.
func TestMirrorUnfold(t *testing.T) {
	eachIndex(t, func(t *testing.T, indexed bool) {
		idrv, d, m := foldDrive(t, "unfold", indexed)
		store := idrv.fs.Store()
		ctx := system()
		src := &source{}
		jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
		x, y := mirrorData("x"), mirrorData("y")

		w := apply(t, ctx, m, "winner", src.item("w", "1", "/a.txt", x, jan), drive.Created)
		apply(t, ctx, m, "folded", into(src.item("a1", "1", "/a (2).txt", x, jan), "w", ""), drive.Folded)
		apply(t, ctx, m, "folded too", into(src.item("a2", "1", "/a (3).txt", x, jan), "w", ""), drive.Folded)
		apply(t, ctx, m, "folded three", into(src.item("a3", "1", "/a (4).txt", x, jan), "w", ""), drive.Folded)

		// A new version with other bytes: its own item, those bytes.
		u := apply(t, ctx, m, "new bytes", into(src.item("a1", "2", "/a (2).txt", y, jan.Add(time.Hour)), "w", ""), drive.Unfolded, drive.Created)
		if u.Item.ID == w.Item.ID || u.Item.Path != "/a (2).txt" || u.Item.Origin == nil || u.Item.Origin.ID != "a1" || u.Item.Origin.Version != "2" {
			t.Fatalf("unfolded %+v (origin %+v)", u.Item, u.Item.Origin)
		}
		if got := read(t, ctx, d, drive.ByPath("/a (2).txt"), drive.At{}); got != y {
			t.Fatalf("the unfolded item reads %q", got[:min(len(got), 30)])
		}
		it, _, _, err := m.Known(ctx, "a1")
		if err != nil || it.ID != u.Item.ID || it.Alias != "" {
			t.Fatalf("known a1 after the unfold: %+v, %v", it, err)
		}
		wi, _, _, _ := m.Known(ctx, "w")
		if got := aliasIDs(wi.Origin); !sameIDs(got, []string{"a2", "a3"}) {
			t.Fatalf("winner's aliases %v, want [a2 a3]", got)
		}
		if indexed {
			if rec, _, err := idrv.fs.OriginRecord(ctx, srcDrive, "a1"); err != nil || rec.Alias || rec.ItemID != u.Item.ID {
				t.Fatalf("a1's record after the unfold %+v, %v", rec, err)
			}
		}

		// Renamed at the source (no longer the winner's sibling): its own
		// item, a stub of the winner's version, no new bytes.
		r := apply(t, ctx, m, "renamed", src.item("a2", "1", "/renamed.txt", x, jan), drive.Unfolded, drive.Created)
		if r.Item.ID == w.Item.ID || r.Item.Path != "/renamed.txt" || r.Item.Origin.ID != "a2" {
			t.Fatalf("renamed %+v", r.Item)
		}
		stubOf(t, store, r.Item.ID, r.Item.Version.ID, w.Item.ID, w.Item.Version.ID)
		if indexed {
			if rec, _, err := idrv.fs.OriginRecord(ctx, srcDrive, "a2"); err != nil || rec.Alias || rec.ItemID != r.Item.ID {
				t.Fatalf("a2's record after the rename %+v, %v", rec, err)
			}
		}

		// The winner's bytes change: the alias left holds the old ones, so
		// it's unfolded, a stub of the winner's old version, no download.
		w2 := apply(t, ctx, m, "winner's new bytes", src.item("w", "2", "/a.txt", y+"w", jan.Add(2*time.Hour)), drive.Updated)
		if got := aliasIDs(w2.Item.Origin); !sameIDs(got, []string{"a3"}) {
			t.Fatalf("aliases after the winner's new version %v, want [a3]", got)
		}
		opens := src.opens.Load()
		o := apply(t, ctx, m, "winner moved on", into(src.item("a3", "1", "/a (4).txt", x, jan), "w", ""), drive.Unfolded, drive.Created)
		if src.opens.Load() != opens {
			t.Fatal("an alias unfolded with bytes the drive has downloaded them")
		}
		stubOf(t, store, o.Item.ID, o.Item.Version.ID, w.Item.ID, w.Item.Version.ID)
		if got := read(t, ctx, d, drive.ByPath("/a (4).txt"), drive.At{}); got != x {
			t.Fatalf("the unfolded alias reads %q", got[:min(len(got), 30)])
		}
		wi, _, _, _ = m.Known(ctx, "w")
		if len(wi.Origin.Aliases) != 0 {
			t.Fatalf("winner's aliases %v, want none", aliasIDs(wi.Origin))
		}
		emptyTrash(t, ctx, d)
	})
}

// TestMirrorFoldWinnerRemoved: the winner deleted at the source while it
// has aliases: the oldest alias becomes the item's origin, the item stays
// where it is, and the others stay its aliases.
func TestMirrorFoldWinnerRemoved(t *testing.T) {
	eachIndex(t, func(t *testing.T, indexed bool) {
		idrv, d, m := foldDrive(t, "fold-winner", indexed)
		ctx := system()
		src := &source{}
		x := mirrorData("x")
		jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
		w := apply(t, ctx, m, "winner", src.item("w", "1", "/a.txt", x, jan), drive.Created)
		// Folded newest first: the oldest is the one created first.
		newer := src.item("a2", "5", "/a (3).txt", x, jan)
		newer.Created = jan.AddDate(0, 2, 0)
		older := src.item("a1", "9", "/a (2).txt", x, jan)
		older.Created = jan.AddDate(0, 1, 0)
		apply(t, ctx, m, "newer", into(newer, "w", ""), drive.Folded)
		apply(t, ctx, m, "older", into(older, "w", ""), drive.Folded)

		if ok, err := m.Remove(ctx, "w"); err != nil || !ok {
			t.Fatalf("remove the winner: %v, %v", ok, err)
		}
		emptyTrash(t, ctx, d)
		it, err := d.Stat(ctx, drive.ByPath("/a.txt"), drive.At{})
		if err != nil || it.ID != w.Item.ID || it.Origin == nil {
			t.Fatalf("the item after the winner's removal: %+v, %v", it, err)
		}
		if it.Origin.ID != "a1" || it.Origin.Version != "9" || it.Origin.Path != "/src/a (2).txt" {
			t.Fatalf("origin %+v, want the oldest alias's", it.Origin)
		}
		if got := aliasIDs(it.Origin); !sameIDs(got, []string{"a2"}) {
			t.Fatalf("aliases %v, want [a2]", got)
		}
		if _, _, found, err := m.Known(ctx, "w"); err != nil || found {
			t.Fatalf("known w after its removal: found %v, %v", found, err)
		}
		k, _, _, err := m.Known(ctx, "a1")
		if err != nil || k.ID != w.Item.ID || k.Alias != "" {
			t.Fatalf("known a1: %+v, %v; want the item, as its origin", k, err)
		}
		k, _, _, err = m.Known(ctx, "a2")
		if err != nil || k.ID != w.Item.ID || k.Alias != "a2" {
			t.Fatalf("known a2: %+v, %v; want the item, as an alias", k, err)
		}
		if indexed {
			if rec, _, err := idrv.fs.OriginRecord(ctx, srcDrive, "a1"); err != nil || rec.Alias || rec.ItemID != w.Item.ID || rec.Version != "9" {
				t.Fatalf("a1's record %+v, %v; want it rewritten as the origin", rec, err)
			}
		}
		// The promoted alias applied, where the item is: no move and no
		// download (its times may be set, as the origin's now).
		opens := src.opens.Load()
		p, err := m.Apply(ctx, into(older, "", "/a.txt"))
		if err != nil || p.Item.ID != w.Item.ID || p.Item.Path != "/a.txt" || src.opens.Load() != opens {
			t.Fatalf("the promoted alias applied: %+v, %v, downloaded %v", p, err, src.opens.Load() != opens)
		}
		for _, a := range p.Actions {
			if a != drive.Unchanged && a != drive.Retimed {
				t.Fatalf("the promoted alias applied: did %v", p.Actions)
			}
		}

		// The last two go too: then it's trashed like any other.
		if ok, err := m.Remove(ctx, "a1"); err != nil || !ok {
			t.Fatalf("remove a1: %v, %v", ok, err)
		}
		if it, err := d.Stat(ctx, drive.ByPath("/a.txt"), drive.At{}); err != nil || it.Origin.ID != "a2" || len(it.Origin.Aliases) != 0 {
			t.Fatalf("after a1 went: %+v, %v", it.Origin, err)
		}
		if ok, err := m.Remove(ctx, "a2"); err != nil || !ok {
			t.Fatalf("remove a2: %v, %v", ok, err)
		}
		if _, err := d.Stat(ctx, drive.ByPath("/a.txt"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
			t.Fatalf("the item after its last member went: %v, want trashed", err)
		}
	})
}

// TestMirrorFoldAliasRemoved: an alias deleted at the source is dropped,
// and only it.
func TestMirrorFoldAliasRemoved(t *testing.T) {
	eachIndex(t, func(t *testing.T, indexed bool) {
		idrv, d, m := foldDrive(t, "fold-alias", indexed)
		ctx := system()
		src := &source{}
		x := mirrorData("x")
		w := apply(t, ctx, m, "winner", src.item("w", "1", "/a.txt", x, time.Time{}), drive.Created)
		apply(t, ctx, m, "folded", into(src.item("a1", "1", "/a (2).txt", x, time.Time{}), "w", ""), drive.Folded)
		apply(t, ctx, m, "folded too", into(src.item("a2", "1", "/a (3).txt", x, time.Time{}), "w", ""), drive.Folded)

		if ok, err := m.Remove(ctx, "a1"); err != nil || !ok {
			t.Fatalf("remove a1: %v, %v", ok, err)
		}
		if ok, err := m.Remove(ctx, "a1"); err != nil || ok {
			t.Fatalf("remove a1 again: %v, %v; want nothing to do", ok, err)
		}
		emptyTrash(t, ctx, d)
		it, err := d.Stat(ctx, drive.ByPath("/a.txt"), drive.At{})
		if err != nil || it.ID != w.Item.ID || it.Origin.ID != "w" || !sameIDs(aliasIDs(it.Origin), []string{"a2"}) {
			t.Fatalf("after the alias went: %+v (origin %+v), %v", it, it.Origin, err)
		}
		if _, _, found, err := m.Known(ctx, "a1"); err != nil || found {
			t.Fatalf("known a1 after its removal: found %v, %v", found, err)
		}
		if indexed {
			if _, _, err := idrv.fs.OriginRecord(ctx, srcDrive, "a1"); !errors.Is(err, icbfs.ErrNotFound) {
				t.Fatalf("a1's record after its removal: %v, want none", err)
			}
		}
		if k, _, _, err := m.Known(ctx, "a2"); err != nil || k.Alias != "a2" {
			t.Fatalf("known a2: %+v, %v", k, err)
		}
	})
}
