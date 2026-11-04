package icbfsdrive

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
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

func (s *readCounter) GetIfChanged(ctx context.Context, key, etag string) (io.ReadCloser, *objstore.Object, bool, error) {
	s.n.Add(1)
	return s.Store.GetIfChanged(ctx, key, etag)
}

func (s *readCounter) Head(ctx context.Context, key string) (*objstore.Object, error) {
	s.n.Add(1)
	return s.Store.Head(ctx, key)
}

func (s *readCounter) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.n.Add(1)
	return s.Store.List(ctx, prefix)
}

const srcDrive = "webdav:docs"

// source is a stand-in for a source item at one version: Apply calls its
// Open only when the bytes are needed, and the test counts how often.
type source struct {
	opens atomic.Int64
}

func (s *source) item(id, version, path, body string, mod time.Time) drive.SourceItem {
	return drive.SourceItem{
		Origin: drive.Origin{ID: id, Version: version, Path: "/src" + path},
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

// TestMirror follows a mirror through what its source does: a
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
			m := drive.NewMirror(d, srcDrive)
			ctx := system()
			src := &source{}
			jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)

			// A new file: created, with its time and origin in the write.
			a := apply(t, ctx, m, "new file", src.item("doc", "1.0", "/Docs/report.txt", "v1", jan), drive.Created)
			if !a.Item.Modified.Equal(jan) || a.Item.Origin == nil || a.Item.Origin.Version != "1.0" || a.Item.Origin.Drive != srcDrive {
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
			folder := drive.SourceItem{Origin: drive.Origin{ID: "reports"}, Path: "/Reports", Type: drive.TypeDir}
			apply(t, ctx, m, "folder", folder, drive.Created)
			apply(t, ctx, m, "file in it", src.item("brief", "1.0", "/Reports/brief.txt", "brief", jan), drive.Created)
			apply(t, ctx, m, "folder again", folder, drive.Unchanged)
			if _, err := m.Remove(ctx, "reports"); err != nil {
				t.Fatal(err)
			}
			apply(t, ctx, m, "folder shared again", folder, drive.Restored)
			opens = src.opens.Load()
			apply(t, ctx, m, "its file", src.item("brief", "1.0", "/Reports/brief.txt", "brief", jan), drive.Unchanged)
			if src.opens.Load() != opens {
				t.Fatal("a file restored with its folder was downloaded again")
			}

			// A bad pass (the source's listing came back empty) removes
			// everything; undoing the pass puts it all back.
			pass := drive.WithPass(ctx, "pass-7")
			for _, id := range []string{"doc", "reports"} {
				if _, err := m.Remove(pass, id); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := m.Undo(ctx, "pass-7"); err != nil || n != 2 {
				t.Fatalf("undo: %d, %v; want 2", n, err)
			}
			for _, p := range []string{"/Docs/report.txt", "/Reports", "/Reports/brief.txt"} {
				if _, err := d.Stat(ctx, drive.ByPath(p), drive.At{}); err != nil {
					t.Fatalf("%s after the undo: %v", p, err)
				}
			}

			// A file made on the drive is never written over, nor the
			// source item downloaded to find that out.
			write(t, ctx, d, "/Docs/mine.txt", "local", drive.Condition{})
			before := src.opens.Load()
			if _, err := m.Apply(ctx, src.item("other", "1.0", "/Docs/mine.txt", "theirs", jan)); !errors.Is(err, drive.ErrNotMirrored) {
				t.Fatalf("applying over a local file: %v, want ErrNotMirrored", err)
			}
			if n := src.opens.Load() - before; n != 0 {
				t.Fatalf("applying over a local file opened the source %d times", n)
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
	data := []byte(`{"drive":"` + srcDrive + `","id":"old","version":"4.0","path":"/src/old.txt","copied":"2026-01-01T00:00:00Z"}`)
	if err := d.fs.SetAppData(context.Background(), it.ID, icbfs.TypeFile, icbfs.AppDriveOrigin, data); err != nil {
		t.Fatal(err)
	}
	m := drive.NewMirror(d, srcDrive)
	src := &source{}
	a := apply(t, ctx, m, "adopted", src.item("old", "4.0", "/old.txt", "unused", time.Time{}), drive.Unchanged)
	if a.Item.ID != it.ID || src.opens.Load() != 0 {
		t.Fatalf("adopted %+v, downloaded %d times", a.Item, src.opens.Load())
	}
	if _, _, err := d.FindByOrigin(ctx, srcDrive, "old"); err != nil {
		t.Fatalf("not indexed after adopting: %v", err)
	}
}

// TestMirrorLookupsStayCheap: with the index, finding a source item costs
// the same however much else the drive holds (a record, and the item's
// path checked), where a search lists everything. A trashed one costs a
// read more: whether the item is still deleted.
func TestMirrorLookupsStayCheap(t *testing.T) {
	counter := &readCounter{Store: miniotest.Store(t)}
	d := newDrive(t, counter, icbfs.Settings{Name: "cheap", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	m := drive.NewMirror(d, srcDrive)
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
	if reads := known(true); reads > few+1 {
		t.Fatalf("finding a trashed item took %d reads, more than a live one's %d and one", reads, few)
	}
}

// mirrorData is a body longer than a stub's own, one per name.
func mirrorData(name string) string { return strings.Repeat("the mirrored bytes of "+name+". ", 40) }

// sha256Of is body's SHA-256, as the drive records it.
func sha256Of(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// hashed is src.item with the source's own hash and size, as a listing
// gives them (SharePoint's QuickXorHash, say): here MD5, a kind the mirror
// computes itself.
func (s *source) hashed(id, version, path, body string, mod time.Time) drive.SourceItem {
	it := s.item(id, version, path, body, mod)
	sum := md5.Sum([]byte(body))
	it.Hash = drive.Hash{Kind: "md5", Value: hex.EncodeToString(sum[:])}
	it.Size = int64(len(body))
	return it
}

// noRefs is a drive that keeps no references: it ignores SameAs, as a
// drive other than icbfs does, and isn't a FileWriter.
type noRefs struct{ drive.Drive }

func (n noRefs) Write(ctx context.Context, p string, body io.Reader, cond drive.Condition, opts drive.WriteOptions) (drive.Item, error) {
	opts.SameAs = nil
	return n.Drive.Write(ctx, p, body, cond, opts)
}

// TestMirrorTwin: a source item with the bytes of one the mirror wrote
// before, elsewhere, is a stub of that item's version, not a second
// upload of the bytes. On a drive that keeps no references it's the bytes.
func TestMirrorTwin(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "mirror-twin", Size: 1 << 30, RootMode: 0o777}, "")
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
	x := mirrorData("x")

	a := apply(t, ctx, m, "first", src.item("a", "1", "/a.txt", x, jan), drive.Created)
	notStub(t, store, a.Item.ID, a.Item.Version.ID)
	b := apply(t, ctx, m, "twin", src.item("b", "1", "/elsewhere/b.txt", x, jan), drive.Created)
	stubOf(t, store, b.Item.ID, b.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if got := read(t, ctx, d, drive.ByPath("/elsewhere/b.txt"), drive.At{}); got != x {
		t.Fatalf("the twin reads %q", got[:min(len(got), 30)])
	}
	if b.Item.Size != int64(len(x)) || b.Item.Origin == nil || b.Item.Origin.ID != "b" || !b.Item.Modified.Equal(jan) {
		t.Fatalf("the twin %+v (origin %+v)", b.Item, b.Item.Origin)
	}
	// Empty files are written, not stubs of each other.
	e1 := apply(t, ctx, m, "empty", src.item("e1", "1", "/e1.txt", "", jan), drive.Created)
	e2 := apply(t, ctx, m, "empty too", src.item("e2", "1", "/e2.txt", "", jan), drive.Created)
	notStub(t, store, e1.Item.ID, e1.Item.Version.ID)
	notStub(t, store, e2.Item.ID, e2.Item.Version.ID)
	// Other bytes are written.
	c := apply(t, ctx, m, "not a twin", src.item("c", "1", "/c.txt", x+"!", jan), drive.Created)
	notStub(t, store, c.Item.ID, c.Item.Version.ID)
	// A new version that's a twin of another item's.
	c = apply(t, ctx, m, "a version that's a twin", src.item("c", "2", "/c.txt", x, jan.Add(time.Hour)), drive.Updated)
	stubOf(t, store, c.Item.ID, c.Item.Version.ID, a.Item.ID, a.Item.Version.ID)

	// A drive that keeps no references gets the bytes, from the spool.
	nstore := miniotest.Store(t)
	nd := newDrive(t, nstore, icbfs.Settings{Name: "mirror-norefs", Size: 1 << 30, RootMode: 0o777}, "")
	nm := drive.NewMirror(noRefs{nd}, srcDrive)
	apply(t, ctx, nm, "first, no references", src.item("a", "1", "/a.txt", x, jan), drive.Created)
	nb := apply(t, ctx, nm, "twin, no references", src.item("b", "1", "/b.txt", x, jan), drive.Created)
	notStub(t, nstore, nb.Item.ID, nb.Item.Version.ID)
	if got := read(t, ctx, nd, drive.ByPath("/b.txt"), drive.At{}); got != x {
		t.Fatalf("the twin on a drive without references reads %q", got[:min(len(got), 30)])
	}
}

// TestMirrorIdenticalVersion: a new version at the source with the bytes
// of the item's current one is a stub at that version: the history keeps
// the source's event, at its time, and no bytes go twice.
func TestMirrorIdenticalVersion(t *testing.T) {
	d, store := sourceDrive(t, "mirror-same")
	m := drive.NewMirror(d, srcDrive).InSourceTime()
	ctx := system()
	src := &source{}
	x := mirrorData("x")
	v1 := apply(t, ctx, m, "v1", src.item("a", "1", "/a.txt", x, year(2021)), drive.Created)
	// The map has no say: here it names a version that's no good.
	m.Hashes().Add(sha256Of(x), drive.HashEntry{ID: "0", Version: "0", Size: int64(len(x))})
	v2 := apply(t, ctx, m, "v2, the same bytes", src.item("a", "2", "/a.txt", x, year(2022)), drive.Updated)
	stubOf(t, store, v2.Item.ID, v2.Item.Version.ID, v1.Item.ID, v1.Item.Version.ID)
	if v2.Item.Origin == nil || v2.Item.Origin.Version != "2" || v2.Item.Size != int64(len(x)) {
		t.Fatalf("v2 %+v (origin %+v)", v2.Item, v2.Item.Origin)
	}
	vs, err := d.Versions(ctx, drive.ByID(v1.Item.ID))
	if err != nil || len(vs) != 2 {
		t.Fatalf("versions: %d, %v; want 2", len(vs), err)
	}
	if !vs[0].Time.Equal(year(2022)) || !vs[1].Time.Equal(year(2021)) || vs[0].ID != v2.Item.Version.ID {
		t.Fatalf("versions %+v; want v2 at 2022 over v1 at 2021", vs)
	}
	if got := read(t, ctx, d, drive.ByID(v1.Item.ID), drive.At{Version: v2.Item.Version.ID}); got != x {
		t.Fatalf("v2 reads %q", got[:min(len(got), 30)])
	}
	// Then other bytes: written.
	v3 := apply(t, ctx, m, "v3, other bytes", src.item("a", "3", "/a.txt", x+"3", year(2023)), drive.Updated)
	notStub(t, store, v3.Item.ID, v3.Item.Version.ID)
}

// TestMirrorSourceHash: a source item whose own hash the mirror has seen
// before, with the same size, is a stub without a download; a different
// size, or a target since erased, is downloaded.
func TestMirrorSourceHash(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "mirror-srchash", Size: 1 << 30, RootMode: 0o777}, "")
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
	x := mirrorData("x")

	a := apply(t, ctx, m, "first", src.hashed("a", "1", "/a.txt", x, jan), drive.Created)
	if n := src.opens.Load(); n != 1 {
		t.Fatalf("the first write opened the source %d times", n)
	}
	b := apply(t, ctx, m, "the same hash", src.hashed("b", "1", "/b.txt", x, jan), drive.Created)
	if n := src.opens.Load(); n != 1 {
		t.Fatalf("a known hash opened the source (%d opens)", n)
	}
	stubOf(t, store, b.Item.ID, b.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if got := read(t, ctx, d, drive.ByPath("/b.txt"), drive.At{}); got != x {
		t.Fatalf("the stub reads %q", got[:min(len(got), 30)])
	}
	// The item's next version, with the same hash: a stub at its own.
	a2 := apply(t, ctx, m, "a's next version, the same hash", src.hashed("a", "2", "/a.txt", x, jan.Add(time.Hour)), drive.Updated)
	if n := src.opens.Load(); n != 1 {
		t.Fatalf("an identical version with a known hash opened the source (%d opens)", n)
	}
	stubOf(t, store, a2.Item.ID, a2.Item.Version.ID, a.Item.ID, a.Item.Version.ID)

	// The size isn't the one the hash was seen with: downloaded.
	wrong := src.hashed("c", "1", "/c.txt", x, jan)
	wrong.Size++
	apply(t, ctx, m, "another size", wrong, drive.Created)
	if n := src.opens.Load(); n != 2 {
		t.Fatalf("a known hash of another size: %d opens, want 2", n)
	}

	// The version the hash names is gone: downloaded, and the bytes written.
	y := mirrorData("y")
	e := apply(t, ctx, m, "e", src.hashed("e", "1", "/e.txt", y, jan), drive.Created)
	apply(t, ctx, m, "e moves on", src.hashed("e", "2", "/e.txt", y+"2", jan.Add(time.Hour)), drive.Updated)
	if err := store.Erase(context.Background(), e.Item.ID, e.Item.Version.ID); err != nil {
		t.Fatal(err)
	}
	before := src.opens.Load()
	f := apply(t, ctx, m, "a hash whose version is gone", src.hashed("f", "1", "/f.txt", y, jan), drive.Created)
	if n := src.opens.Load() - before; n != 1 {
		t.Fatalf("a stale hash opened the source %d times, want 1", n)
	}
	notStub(t, store, f.Item.ID, f.Item.Version.ID)
	if got := read(t, ctx, d, drive.ByPath("/f.txt"), drive.At{}); got != y {
		t.Fatalf("f reads %q", got[:min(len(got), 30)])
	}
}

// TestMirrorStaleHash: a map entry whose version was erased since gives
// the bytes, and the entry is the new item's from then on.
func TestMirrorStaleHash(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "mirror-stale", Size: 1 << 30, RootMode: 0o777}, "")
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
	x := mirrorData("x")
	v1 := apply(t, ctx, m, "v1", src.item("a", "1", "/a.txt", x, jan), drive.Created)
	apply(t, ctx, m, "v2", src.item("a", "2", "/a.txt", x+"2", jan.Add(time.Hour)), drive.Updated)
	if e, ok := m.Hashes().Lookup(sha256Of(x)); !ok || e.ID != v1.Item.ID || e.Version != v1.Item.Version.ID {
		t.Fatalf("the map has %+v, %v for v1's bytes", e, ok)
	}
	if err := store.Erase(context.Background(), v1.Item.ID, v1.Item.Version.ID); err != nil {
		t.Fatal(err)
	}
	b := apply(t, ctx, m, "a twin of an erased version", src.item("b", "1", "/b.txt", x, jan), drive.Created)
	notStub(t, store, b.Item.ID, b.Item.Version.ID)
	if got := read(t, ctx, d, drive.ByPath("/b.txt"), drive.At{}); got != x {
		t.Fatalf("b reads %q", got[:min(len(got), 30)])
	}
	if e, ok := m.Hashes().Lookup(sha256Of(x)); !ok || e.ID != b.Item.ID || e.Version != b.Item.Version.ID || e.Size != int64(len(x)) {
		t.Fatalf("after the fallback the map has %+v, %v; want b's version", e, ok)
	}
}

// memHashes keeps a mirror's hash map in memory.
type memHashes struct{ data []byte }

func (s *memHashes) LoadHashes(context.Context) ([]byte, error) { return s.data, nil }
func (s *memHashes) SaveHashes(_ context.Context, data []byte) error {
	s.data = data
	return nil
}

// TestMirrorRebuildHashes: the map rebuilt from the drive's listing of the
// source's items equals the one the writes built, version by version,
// skipping versions with no checksum recorded; and it's kept by a
// HashStore.
func TestMirrorRebuildHashes(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		name := "indexed"
		if !indexed {
			name = "searched"
		}
		t.Run(name, func(t *testing.T) {
			store := miniotest.Store(t)
			idrv := newDrive(t, store, icbfs.Settings{Name: "mirror-rebuild-" + name, Size: 1 << 30, RootMode: 0o777}, "")
			var d drive.Drive = idrv
			if !indexed {
				d = plainDrive{idrv}
			}
			m := drive.NewMirror(d, srcDrive)
			ctx := system()
			src := &source{}
			jan := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
			x, y, z := mirrorData("x"), mirrorData("y"), mirrorData("z")
			apply(t, ctx, m, "a", src.item("a", "1", "/a.txt", x, jan), drive.Created)
			apply(t, ctx, m, "b, a's twin", src.item("b", "1", "/dir/b.txt", x, jan), drive.Created)
			apply(t, ctx, m, "a v2", src.item("a", "2", "/a.txt", y, jan.Add(time.Hour)), drive.Updated)
			apply(t, ctx, m, "a v3, as v2", src.item("a", "3", "/a.txt", y, jan.Add(2*time.Hour)), drive.Updated)
			apply(t, ctx, m, "c, hashed", src.hashed("c", "1", "/c.txt", z, jan), drive.Created)
			apply(t, ctx, m, "a folder", drive.SourceItem{Origin: drive.Origin{ID: "d"}, Path: "/folder", Type: drive.TypeDir}, drive.Created)
			write(t, ctx, d, "/mine.txt", mirrorData("local"), drive.Condition{})
			// A version with no checksum recorded: skipped, not computed.
			cur, err := d.Stat(ctx, drive.ByPath("/c.txt"), drive.At{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(context.Background(), cur.ID, strings.NewReader("no checksum recorded"), nil, ""); err != nil {
				t.Fatal(err)
			}

			built := m.Hashes().Entries()
			if len(built) != 3 {
				t.Fatalf("the writes built %d entries, want 3: %+v", len(built), built)
			}
			fresh := drive.NewMirror(d, srcDrive)
			if err := fresh.RebuildHashes(ctx); err != nil {
				t.Fatal(err)
			}
			if got := fresh.Hashes().Entries(); !reflect.DeepEqual(got, built) {
				t.Fatalf("rebuilt %+v\nbuilt   %+v", got, built)
			}
			if _, ok := fresh.Hashes().Lookup(sha256Of("no checksum recorded")); ok {
				t.Fatal("a version with no checksum recorded was hashed")
			}
			// A rebuild keeps what the source's hashes say of the hashes
			// still there.
			sources := m.Hashes().SourceEntries()
			if len(sources) != 1 {
				t.Fatalf("source hashes %+v, want c's", sources)
			}
			if err := m.RebuildHashes(ctx); err != nil {
				t.Fatal(err)
			}
			if got := m.Hashes().Entries(); !reflect.DeepEqual(got, built) {
				t.Fatalf("rebuilt in place %+v\nbuilt %+v", got, built)
			}
			if got := m.Hashes().SourceEntries(); !reflect.DeepEqual(got, sources) {
				t.Fatalf("source hashes after a rebuild %+v, want %+v", got, sources)
			}

			// Saved and loaded.
			kept := &memHashes{}
			if err := m.WithHashStore(kept).SaveHashes(ctx); err != nil {
				t.Fatal(err)
			}
			loaded := drive.NewMirror(d, srcDrive).WithHashStore(kept)
			if err := loaded.LoadHashes(ctx); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.Hashes().Entries(), built) || !reflect.DeepEqual(loaded.Hashes().SourceEntries(), sources) {
				t.Fatalf("loaded %+v %+v", loaded.Hashes().Entries(), loaded.Hashes().SourceEntries())
			}
			kept.data = []byte("not a hash map")
			if err := loaded.LoadHashes(ctx); !errors.Is(err, drive.ErrInvalid) {
				t.Fatalf("loading a corrupt map: %v, want ErrInvalid", err)
			}
		})
	}
}

// TestMirrorLargeFile: a body past what the spool keeps in memory is
// spooled to a file and written from it (WriteFrom); its twin is a stub.
func TestMirrorLargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 65 MiB")
	}
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "mirror-large", Size: 1 << 30, RootMode: 0o777}, "")
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	big := strings.Repeat("0123456789abcdef", int(drive.SpoolInMemory/16)+1<<16)
	a := apply(t, ctx, m, "large", src.item("a", "1", "/a.bin", big, time.Time{}), drive.Created)
	notStub(t, store, a.Item.ID, a.Item.Version.ID)
	if a.Item.Size != int64(len(big)) || a.Item.Checksums == nil || a.Item.Checksums.SHA256 != sha256Of(big) {
		t.Fatalf("the large file %+v (checksums %+v)", a.Item, a.Item.Checksums)
	}
	if got := read(t, ctx, d, drive.ByPath("/a.bin"), drive.At{}); got != big {
		t.Fatalf("the large file reads back %d bytes, not what was written", len(got))
	}
	b := apply(t, ctx, m, "its twin", src.item("b", "1", "/b.bin", big, time.Time{}), drive.Created)
	stubOf(t, store, b.Item.ID, b.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := src.opens.Load(); n != 2 {
		t.Fatalf("opened the source %d times, want 2", n)
	}
}
