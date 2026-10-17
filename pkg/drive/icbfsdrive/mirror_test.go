package icbfsdrive

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// plainDrive hides FindByOrigin, so a Mirror on it uses the generic search
// any backend gets.
type plainDrive struct{ drive.Drive }

// readCounter counts a store's reads.
type readCounter struct {
	objstore.Store
	n atomic.Int64
}

func (s *readCounter) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.n.Add(1)
	return s.Store.Get(ctx, key)
}

func (s *readCounter) Head(ctx context.Context, key string) (*objstore.Object, error) {
	s.n.Add(1)
	return s.Store.Head(ctx, key)
}

func (s *readCounter) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.n.Add(1)
	return s.Store.List(ctx, prefix)
}

const sp = "sharepoint:site/library"

// source is a stand-in for a source item at one version: Apply calls its
// Open only when the bytes are needed, and the test counts how often.
type source struct {
	opens atomic.Int64
}

func (s *source) item(id, version, path, body string, mod time.Time) drive.SourceItem {
	return drive.SourceItem{
		Origin: drive.Origin{ID: id, Version: version, Path: "/sp" + path},
		Path:   path, Type: drive.TypeFile, ModTime: mod, ContentType: "text/plain",
		Open: func(context.Context) (io.ReadCloser, error) {
			s.opens.Add(1)
			return io.NopCloser(strings.NewReader(body)), nil
		},
	}
}

func actions(t *testing.T, what string, a drive.Applied, err error, want ...drive.Action) drive.Applied {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if len(a.Actions) != len(want) {
		t.Fatalf("%s: did %v, want %v", what, a.Actions, want)
	}
	for i := range want {
		if a.Actions[i] != want[i] {
			t.Fatalf("%s: did %v, want %v", what, a.Actions, want)
		}
	}
	return a
}

// TestMirror follows a SharePoint mirror through what its source does: a
// new file, versions, a move, a delete and the recycle bin bringing it
// back, a folder deleted and shared again, a bad pass undone. Each source
// item is applied as it is now; the mirror decides what that means, and
// asks for the bytes only when it must. Once with icbfs's origin index,
// once with the generic search.
func TestMirror(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		name := "indexed"
		if !indexed {
			name = "searched"
		}
		t.Run(name, func(t *testing.T) {
			store := miniotest.Store(t)
			idrv := newDrive(t, store, icbfs.Settings{Name: "mirror-" + name, Size: 1 << 30, RootMode: 0o777}, "")
			var d drive.Drive = idrv
			if !indexed {
				d = plainDrive{idrv}
			}
			m := drive.NewMirror(d, sp)
			ctx := system()
			src := &source{}
			jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)

			// A new file: created, with its time and origin in the write.
			a := apply(t, ctx, m, "new file", src.item("doc", "1.0", "/Docs/report.txt", "v1", jan), drive.Created)
			if !a.Item.Modified.Equal(jan) || a.Item.Origin == nil || a.Item.Origin.Version != "1.0" || a.Item.Origin.Drive != sp {
				t.Fatalf("created item %+v (origin %+v)", a.Item, a.Item.Origin)
			}
			if got := read(t, ctx, d, drive.ByPath("/Docs/report.txt"), drive.At{}); got != "v1" {
				t.Fatalf("content %q", got)
			}

			// The same version again: nothing, and no download.
			opens := src.opens.Load()
			apply(t, ctx, m, "same version", src.item("doc", "1.0", "/Docs/report.txt", "v1", jan), drive.Unchanged)
			if src.opens.Load() != opens {
				t.Fatal("an unchanged item was downloaded")
			}

			// A new version.
			feb := jan.AddDate(0, 1, 0)
			apply(t, ctx, m, "new version", src.item("doc", "2.0", "/Docs/report.txt", "v2", feb), drive.Updated)

			// Moved at the source: moved here, no download.
			opens = src.opens.Load()
			a = apply(t, ctx, m, "moved", src.item("doc", "2.0", "/Archive/report.txt", "v2", feb), drive.Moved)
			if src.opens.Load() != opens || a.Item.Path != "/Archive/report.txt" {
				t.Fatalf("move: %+v, downloaded %v", a.Item, src.opens.Load() != opens)
			}

			// Deleted at the source, then back from its recycle bin: the
			// trash entry is restored, history and all, no download.
			if ok, err := m.Remove(ctx, "doc"); err != nil || !ok {
				t.Fatalf("remove: %v, %v", ok, err)
			}
			if ok, err := m.Remove(ctx, "doc"); err != nil || ok {
				t.Fatalf("removing it again: %v, %v; want nothing to do", ok, err)
			}
			if _, trashed, found, err := m.Known(ctx, "doc"); err != nil || !found || trashed == nil {
				t.Fatalf("known after remove: trashed %v, found %v, %v", trashed, found, err)
			}
			opens = src.opens.Load()
			apply(t, ctx, m, "back from the recycle bin", src.item("doc", "2.0", "/Archive/report.txt", "v2", feb), drive.Restored)
			if src.opens.Load() != opens {
				t.Fatal("a restore downloaded the content")
			}
			vs, err := d.Versions(ctx, drive.ByPath("/Archive/report.txt"))
			if err != nil || len(vs) != 2 {
				t.Fatalf("versions after the restore: %d, %v; want both", len(vs), err)
			}

			// Deleted again; it comes back with a newer version, elsewhere.
			if _, err := m.Remove(ctx, "doc"); err != nil {
				t.Fatal(err)
			}
			apply(t, ctx, m, "restored and updated", src.item("doc", "3.0", "/Docs/report.txt", "v3", feb), drive.Restored, drive.Updated)
			if vs, _ := d.Versions(ctx, drive.ByPath("/Docs/report.txt")); len(vs) != 3 {
				t.Fatalf("versions: %d, want 3 (one history throughout)", len(vs))
			}

			// A folder, unshared (delta reports its items deleted) and
			// shared again: the folder comes back with what was in it.
			folder := drive.SourceItem{Origin: drive.Origin{ID: "matter"}, Path: "/Matter", Type: drive.TypeDir}
			apply(t, ctx, m, "folder", folder, drive.Created)
			apply(t, ctx, m, "file in it", src.item("brief", "1.0", "/Matter/brief.txt", "brief", jan), drive.Created)
			apply(t, ctx, m, "folder again", folder, drive.Unchanged)
			if _, err := m.Remove(ctx, "matter"); err != nil {
				t.Fatal(err)
			}
			apply(t, ctx, m, "folder shared again", folder, drive.Restored)
			opens = src.opens.Load()
			apply(t, ctx, m, "its file", src.item("brief", "1.0", "/Matter/brief.txt", "brief", jan), drive.Unchanged)
			if src.opens.Load() != opens {
				t.Fatal("a file restored with its folder was downloaded again")
			}

			// A bad pass (the source's listing came back empty) removes
			// everything; undoing the pass puts it all back.
			pass := drive.WithPass(ctx, "pass-7")
			for _, id := range []string{"doc", "matter"} {
				if _, err := m.Remove(pass, id); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := m.Undo(ctx, "pass-7"); err != nil || n != 2 {
				t.Fatalf("undo: %d, %v; want 2", n, err)
			}
			for _, p := range []string{"/Docs/report.txt", "/Matter", "/Matter/brief.txt"} {
				if _, err := d.Stat(ctx, drive.ByPath(p), drive.At{}); err != nil {
					t.Fatalf("%s after the undo: %v", p, err)
				}
			}

			// A file made on the drive is never written over.
			write(t, ctx, d, "/Docs/mine.txt", "local", drive.Condition{})
			if _, err := m.Apply(ctx, src.item("other", "1.0", "/Docs/mine.txt", "theirs", jan)); !errors.Is(err, drive.ErrNotMirrored) {
				t.Fatalf("applying over a local file: %v, want ErrNotMirrored", err)
			}
			if got := read(t, ctx, d, drive.ByPath("/Docs/mine.txt"), drive.At{}); got != "local" {
				t.Fatalf("the local file now reads %q", got)
			}
		})
	}
}

// apply applies src and checks what the mirror did.
func apply(t *testing.T, ctx context.Context, m *drive.Mirror, what string, src drive.SourceItem, want ...drive.Action) drive.Applied {
	t.Helper()
	a, err := m.Apply(ctx, src)
	return actions(t, what, a, err, want...)
}

// TestMirrorAdoptsItemsCopiedBeforeTheIndex: an item a daemon copied
// before the drive indexed origins (its origin only in its metadata) is
// found where it is and adopted, not refused as someone else's file.
func TestMirrorAdoptsItemsCopiedBeforeTheIndex(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "adopt", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	it := write(t, ctx, d, "/old.txt", "copied long ago", drive.Condition{})
	data := []byte(`{"drive":"` + sp + `","id":"old","version":"4.0","path":"/sp/old.txt","copied":"2026-01-01T00:00:00Z"}`)
	if _, err := d.fs.SetAppData(context.Background(), it.ID, icbfs.TypeFile, icbfs.AppDriveOrigin, data); err != nil {
		t.Fatal(err)
	}
	m := drive.NewMirror(d, sp)
	src := &source{}
	a := apply(t, ctx, m, "adopted", src.item("old", "4.0", "/old.txt", "unused", time.Time{}), drive.Unchanged)
	if a.Item.ID != it.ID || src.opens.Load() != 0 {
		t.Fatalf("adopted %+v, downloaded %d times", a.Item, src.opens.Load())
	}
	if _, _, err := d.FindByOrigin(ctx, sp, "old"); err != nil {
		t.Fatalf("not indexed after adopting: %v", err)
	}
}

// TestMirrorLookupsStayCheap: with the index, finding a source item costs
// the same however much else the drive holds (a record, and the item's
// path checked), where a search lists everything.
func TestMirrorLookupsStayCheap(t *testing.T) {
	counter := &readCounter{Store: miniotest.Store(t)}
	d := newDrive(t, counter, icbfs.Settings{Name: "cheap", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	m := drive.NewMirror(d, sp)
	src := &source{}
	if _, err := m.Apply(ctx, src.item("doc", "1.0", "/deep/a/b/doc.txt", "v1", time.Time{})); err != nil {
		t.Fatal(err)
	}
	known := func(wantTrashed bool) int64 {
		t.Helper()
		before := counter.n.Load()
		_, trashed, found, err := m.Known(ctx, "doc")
		if err != nil || !found || (trashed != nil) != wantTrashed {
			t.Fatalf("known: trashed %v, found %v, %v", trashed, found, err)
		}
		return counter.n.Load() - before
	}
	few := known(false)
	for i := range 40 {
		write(t, ctx, d, "/other/f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt", "x", drive.Condition{})
	}
	if many := known(false); many != few {
		t.Fatalf("finding a live item took %d reads with 40 more files on the drive, %d before", many, few)
	}
	if _, err := m.Remove(ctx, "doc"); err != nil {
		t.Fatal(err)
	}
	if reads := known(true); reads > few {
		t.Fatalf("finding a trashed item took %d reads, more than a live one's %d", reads, few)
	}
}
