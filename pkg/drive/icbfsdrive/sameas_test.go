package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// sameAsData is a target's bytes: longer than a stub's own.
var sameAsData = strings.Repeat("the target's bytes, longer than a stub. ", 30)

// refTo is a Ref to it's current version, with its SHA-256.
func refTo(it drive.Item) *drive.Ref {
	r := &drive.Ref{ID: it.ID, Version: it.Version.ID}
	if it.Checksums != nil {
		r.SHA256 = it.Checksums.SHA256
	}
	return r
}

// sameAs writes p as a reference to ref, with no body.
func sameAs(ctx context.Context, d *Drive, p string, ref *drive.Ref, cond drive.Condition, opts drive.WriteOptions) (drive.Item, error) {
	opts.SameAs = ref
	return d.Write(ctx, p, nil, cond, opts)
}

// stubOf fails unless key's version is a stub naming targetKey's version
// targetVersion.
func stubOf(t *testing.T, store objstore.Store, key, version, targetKey, targetVersion string) {
	t.Helper()
	obj, err := store.HeadVersion(context.Background(), key, version)
	if err != nil {
		t.Fatal(err)
	}
	r, ok, err := icbfs.RefOf(obj)
	if !ok || err != nil {
		t.Fatalf("%s version %s isn't a stub (%v): %+v", key, version, err, obj.Metadata)
	}
	if r.Key != targetKey || r.VersionID != targetVersion {
		t.Fatalf("the stub names %s version %s, want %s version %s", r.Key, r.VersionID, targetKey, targetVersion)
	}
}

// versionCount is how many versions key's content has.
func versionCount(t *testing.T, store objstore.Store, key string) int {
	t.Helper()
	h, err := store.History(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return len(h)
}

// TestSameAsWrite: a reference write reads back as X's bytes, new or over
// an existing file, under its condition, with the times, app keys, mode
// and content type a byte write records; the item it returns has the
// real size, the stub's version and X's checksums.
func TestSameAsWrite(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "sameas", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	x := write(t, ctx, d, "/x.txt", sameAsData, drive.Condition{})
	n := int64(len(sameAsData))
	if x.Checksums == nil || x.Checksums.SHA256 == "" {
		t.Fatalf("x has no checksums: %+v", x)
	}

	mod := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	created := time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
	mode := uint32(0o640)
	opts := drive.WriteOptions{Parents: true, ContentType: "text/x-test", ModTime: mod, Created: created,
		App: map[string][]byte{"test.k": []byte("v")}, Mode: &mode}
	y, err := sameAs(ctx, d, "/new/dir/y.txt", refTo(x), drive.Condition{IfNoneMatch: true}, opts)
	if err != nil {
		t.Fatalf("reference write: %v", err)
	}
	if y.Version == nil {
		t.Fatalf("no version: %+v", y)
	}
	stubOf(t, store, y.ID, y.Version.ID, x.ID, x.Version.ID)
	if y.Size != n || y.Version.Size != n {
		t.Fatalf("size %d, version size %d, want %d", y.Size, y.Version.Size, n)
	}
	if y.Checksums == nil || *y.Checksums != *x.Checksums {
		t.Fatalf("checksums %+v, want x's %+v", y.Checksums, x.Checksums)
	}
	if y.Version.Checksums == nil || *y.Version.Checksums != *x.Checksums {
		t.Fatalf("version checksums %+v, want x's %+v", y.Version.Checksums, x.Checksums)
	}
	if got := read(t, ctx, d, drive.ByPath("/new/dir/y.txt"), drive.At{}); got != sameAsData {
		t.Fatalf("read back %d bytes, want x's %d", len(got), n)
	}
	st, err := d.Stat(ctx, drive.ByPath("/new/dir/y.txt"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Modified.Equal(mod) || !st.Created.Equal(created) || st.Mode&0o7777 != mode ||
		string(st.App["test.k"]) != "v" || st.ContentType != "text/x-test" || st.Size != n {
		t.Fatalf("stat %+v: want modified %v, created %v, mode %o, app test.k, content type", st, mod, created, mode)
	}
	if !y.Modified.Equal(mod) || y.Mode&0o7777 != mode || string(y.App["test.k"]) != "v" {
		t.Fatalf("the returned item %+v doesn't carry the write's options", y)
	}

	// Over an existing file, under If-Match; an origin recorded with it.
	z := write(t, ctx, d, "/z.txt", "old", drive.Condition{})
	before := versionCount(t, store, z.ID)
	if _, err := sameAs(ctx, d, "/z.txt", refTo(x), drive.Condition{IfMatch: "not-the-version"}, drive.WriteOptions{}); !errors.As(err, new(*drive.PreconditionError)) {
		t.Fatalf("a stale If-Match: %v; want a PreconditionError", err)
	}
	if _, err := sameAs(ctx, d, "/z.txt", refTo(x), drive.Condition{IfNoneMatch: true}, drive.WriteOptions{}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("If-None-Match on an existing file: %v; want ErrPrecondition", err)
	}
	if got := versionCount(t, store, z.ID); got != before {
		t.Fatalf("a refused reference write left %d versions, had %d", got, before)
	}
	origin := &drive.Origin{Drive: "elsewhere", ID: "src-1", Copied: time.Now().UTC().Truncate(time.Second)}
	z2, err := sameAs(ctx, d, "/z.txt", refTo(x), drive.Condition{IfMatch: z.Version.ID}, drive.WriteOptions{Origin: origin, ModTime: mod})
	if err != nil {
		t.Fatalf("reference write over a file: %v", err)
	}
	stubOf(t, store, z2.ID, z2.Version.ID, x.ID, x.Version.ID)
	if z2.ID != z.ID || z2.Size != n || z2.Checksums == nil || *z2.Checksums != *x.Checksums || !z2.Modified.Equal(mod) {
		t.Fatalf("over a file: %+v", z2)
	}
	if z2.Origin == nil || z2.Origin.ID != "src-1" {
		t.Fatalf("origin %+v", z2.Origin)
	}
	if got := read(t, ctx, d, drive.ByPath("/z.txt"), drive.At{}); got != sameAsData {
		t.Fatalf("read back over a file: %q", got[:min(len(got), 20)])
	}
	if got := read(t, ctx, d, drive.ByID(z.ID), drive.At{Version: z.Version.ID}); got != "old" {
		t.Fatalf("the file's older version: %q", got)
	}

	// An older version of x.
	write(t, ctx, d, "/x.txt", "x's next bytes", drive.Condition{})
	old, err := sameAs(ctx, d, "/old.txt", &drive.Ref{ID: x.ID, Version: x.Version.ID}, drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, d, drive.ByID(old.ID), drive.At{}); got != sameAsData {
		t.Fatalf("a reference to x's older version reads %q", got[:min(len(got), 20)])
	}

	// No body, no Size: WriteFrom too.
	wf, err := d.WriteFrom(ctx, "/wf.txt", nil, 0, drive.Condition{}, drive.WriteOptions{SameAs: refTo(x)})
	if err != nil {
		t.Fatalf("WriteFrom with SameAs: %v", err)
	}
	stubOf(t, store, wf.ID, wf.Version.ID, x.ID, x.Version.ID)
}

// TestSameAsNeedsRead: without the right to read X a reference to it is
// ErrForbidden, and nothing is written.
func TestSameAsNeedsRead(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "sameasperm", Size: 1 << 30, RootMode: 0o777}, "")
	alice := as(1000, 1000)
	bob := as(1001, 1001)
	private := uint32(0o600)
	x, err := d.Write(alice, "/secret.txt", strings.NewReader(sameAsData), drive.Condition{}, drive.WriteOptions{Mode: &private})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sameAs(bob, d, "/bob.txt", refTo(x), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("a reference without read on X: %v; want ErrForbidden", err)
	}
	if _, err := d.Stat(system(), drive.ByPath("/bob.txt"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a refused reference made a file: %v", err)
	}
	// Bob's own file, over which he may write: still no.
	b := write(t, bob, d, "/bob2.txt", "bob's", drive.Condition{})
	if _, err := sameAs(bob, d, "/bob2.txt", refTo(x), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("a reference over his own file without read on X: %v; want ErrForbidden", err)
	}
	if got := versionCount(t, store, b.ID); got != 1 {
		t.Fatalf("bob's file has %d versions", got)
	}
	// The destination's own check still holds: alice may read X but not
	// write bob's file.
	bobsMode := uint32(0o644)
	if _, err := d.Write(bob, "/bob3.txt", strings.NewReader("b"), drive.Condition{}, drive.WriteOptions{Mode: &bobsMode}); err != nil {
		t.Fatal(err)
	}
	if _, err := sameAs(alice, d, "/bob3.txt", refTo(x), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("a reference over a file she can't write: %v; want ErrForbidden", err)
	}
	// Alice, who may read it, can.
	if _, err := sameAs(alice, d, "/alice.txt", refTo(x), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatalf("a reference with read on X: %v", err)
	}
}

// TestSameAsUnavailable: a version the drive can't reference is
// ErrRefUnavailable, and nothing is written: X on another filesystem,
// gone, not dated, or with other checksums.
func TestSameAsUnavailable(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "sameasun", Size: 1 << 30, RootMode: 0o777}, "")
	other := newDrive(t, store, icbfs.Settings{Name: "sameasother", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	x := write(t, ctx, d, "/x.txt", sameAsData, drive.Condition{})
	dest := write(t, ctx, d, "/dest.txt", "dest", drive.Condition{})

	unavailable := func(what string, ref *drive.Ref, opts drive.WriteOptions) {
		t.Helper()
		if _, err := sameAs(ctx, d, "/new.txt", ref, drive.Condition{}, opts); !errors.Is(err, drive.ErrRefUnavailable) {
			t.Fatalf("%s, a new file: %v; want ErrRefUnavailable", what, err)
		}
		if _, err := d.Stat(ctx, drive.ByPath("/new.txt"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
			t.Fatalf("%s: a new file was made (%v)", what, err)
		}
		before := versionCount(t, store, dest.ID)
		if _, err := sameAs(ctx, d, "/dest.txt", ref, drive.Condition{}, opts); !errors.Is(err, drive.ErrRefUnavailable) {
			t.Fatalf("%s, over a file: %v; want ErrRefUnavailable", what, err)
		}
		if got := versionCount(t, store, dest.ID); got != before {
			t.Fatalf("%s: the file has %d versions, had %d", what, got, before)
		}
	}

	// On another filesystem.
	ox := write(t, ctx, other, "/x.txt", sameAsData, drive.Condition{})
	unavailable("another filesystem's", refTo(ox), drive.WriteOptions{})

	// Other checksums: SameAs's, or the write's own.
	wrong := strings.Repeat("ab", 32)
	unavailable("another SHA-256", &drive.Ref{ID: x.ID, Version: x.Version.ID, SHA256: wrong}, drive.WriteOptions{})
	unavailable("another MD5", refTo(x), drive.WriteOptions{Checksums: &drive.Checksums{MD5: strings.Repeat("cd", 16)}})
	unavailable("another Checksums SHA-256", &drive.Ref{ID: x.ID, Version: x.Version.ID}, drive.WriteOptions{Checksums: &drive.Checksums{SHA256: wrong}})

	// Gone: no such item, no such version, a version erased.
	unavailable("no such item", &drive.Ref{ID: x.ID[:len(x.ID)-4] + "0000", Version: x.Version.ID}, drive.WriteOptions{})
	v2 := write(t, ctx, d, "/x.txt", "x's second", drive.Condition{})
	if err := store.Erase(context.Background(), x.ID, x.Version.ID); err != nil {
		t.Fatal(err)
	}
	unavailable("an erased version", &drive.Ref{ID: x.ID, Version: x.Version.ID}, drive.WriteOptions{})
	// A directory isn't a version's.
	dir, err := d.Mkdir(ctx, "/dir", false)
	if err != nil {
		t.Fatal(err)
	}
	unavailable("a directory", &drive.Ref{ID: dir.ID, Version: v2.Version.ID}, drive.WriteOptions{})
	// A trashed item.
	if err := d.Delete(ctx, drive.ByPath("/x.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	unavailable("a trashed item", refTo(v2), drive.WriteOptions{})

	// A version with no id is the caller's mistake.
	if _, err := sameAs(ctx, d, "/new.txt", &drive.Ref{ID: dest.ID}, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("no version: %v; want ErrInvalid", err)
	}
	if _, err := sameAs(ctx, d, "/new.txt", &drive.Ref{ID: dest.ID, Version: dest.Version.ID, SHA256: "not hex"}, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("a SHA-256 not in hex: %v; want ErrInvalid", err)
	}
}

// TestSameAsUndated: in a filesystem written in a source's time, a
// version its .version doesn't list (a write that may not have finished)
// is ErrRefUnavailable.
func TestSameAsUndated(t *testing.T) {
	d, store := sourceDrive(t, "sameasundated")
	ctx := at(system(), 2021)
	x, err := d.Write(ctx, "/x.txt", strings.NewReader(sameAsData), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Put(context.Background(), x.ID, bytes.NewReader([]byte("never dated")), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sameAs(at(system(), 2022), d, "/y.txt", &drive.Ref{ID: x.ID, Version: raw.VersionID}, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrRefUnavailable) {
		t.Fatalf("an undated version: %v; want ErrRefUnavailable", err)
	}
	if _, err := d.Stat(system(), drive.ByPath("/y.txt"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a file was made (%v)", err)
	}
	// The dated one is fine, dated as the write is.
	y, err := sameAs(at(system(), 2022), d, "/y.txt", &drive.Ref{ID: x.ID, Version: x.Version.ID}, drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatalf("a dated version: %v", err)
	}
	if !y.Version.Time.Equal(year(2022)) {
		t.Fatalf("the stub is dated %v, want %v", y.Version.Time, year(2022))
	}
	allDated(t, store, y.ID)
}

// TestSameAsStub: a reference to a stub is a stub at its target: never a
// chain.
func TestSameAsStub(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "sameaschain", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	x := write(t, ctx, d, "/x.txt", sameAsData, drive.Condition{})
	y, err := sameAs(ctx, d, "/y.txt", refTo(x), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	z, err := sameAs(ctx, d, "/z.txt", refTo(y), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatalf("a reference to a stub: %v", err)
	}
	stubOf(t, store, z.ID, z.Version.ID, x.ID, x.Version.ID)
	if got := read(t, ctx, d, drive.ByPath("/z.txt"), drive.At{}); got != sameAsData {
		t.Fatalf("read back %q", got[:min(len(got), 20)])
	}
	if z.Size != int64(len(sameAsData)) || z.Checksums == nil || *z.Checksums != *x.Checksums {
		t.Fatalf("z %+v", z)
	}
	// Over an existing file, too.
	w := write(t, ctx, d, "/w.txt", "w", drive.Condition{})
	w2, err := sameAs(ctx, d, "/w.txt", refTo(y), drive.Condition{IfMatch: w.Version.ID}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stubOf(t, store, w2.ID, w2.Version.ID, x.ID, x.Version.ID)
}

// TestCopyWritesStub: drive.Copy within one drive writes a stub, a file's
// or every file's in a directory; from another drive, or of a version it
// can't reference, it streams the bytes as before.
func TestCopyWritesStub(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "sameascopy", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	x := write(t, ctx, d, "/x.txt", sameAsData, drive.Condition{})
	c, err := drive.Copy(ctx, d, "drive-1", drive.ByPath("/x.txt"), drive.At{}, d, "/copy.txt", drive.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	stubOf(t, store, c.ID, c.Version.ID, x.ID, x.Version.ID)
	if got := read(t, ctx, d, drive.ByPath("/copy.txt"), drive.At{}); got != sameAsData {
		t.Fatalf("the copy reads %q", got[:min(len(got), 20)])
	}
	if c.Origin == nil || c.Origin.ID != x.ID || c.Origin.Version != x.Version.ID || c.Size != int64(len(sameAsData)) {
		t.Fatalf("the copy %+v (origin %+v)", c, c.Origin)
	}

	// A directory.
	f := write(t, ctx, d, "/dir/f.txt", sameAsData+"f", drive.Condition{})
	if _, err := drive.Copy(ctx, d, "drive-1", drive.ByPath("/dir"), drive.At{}, d, "/dir2", drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	cf, err := d.Stat(ctx, drive.ByPath("/dir2/f.txt"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	stubOf(t, store, cf.ID, cf.Version.ID, f.ID, f.Version.ID)

	// From another drive: the bytes.
	other := newDrive(t, store, icbfs.Settings{Name: "sameascopyother", Size: 1 << 30, RootMode: 0o777}, "")
	ox := write(t, ctx, other, "/o.txt", sameAsData+"o", drive.Condition{})
	oc, err := drive.Copy(ctx, other, "drive-2", drive.ByPath("/o.txt"), drive.At{}, d, "/ocopy.txt", drive.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	notStub(t, store, oc.ID, oc.Version.ID)
	if got := read(t, ctx, d, drive.ByPath("/ocopy.txt"), drive.At{}); got != sameAsData+"o" || oc.Origin == nil || oc.Origin.ID != ox.ID {
		t.Fatalf("the copy from another drive: %q, origin %+v", got[:min(len(got), 20)], oc.Origin)
	}
}

// TestCopyFallsBack: within one drive, a version the drive can't
// reference (here, one not dated) is copied as bytes.
func TestCopyFallsBack(t *testing.T) {
	d, store := sourceDrive(t, "sameascopyback")
	ctx := at(system(), 2021)
	x, err := d.Write(ctx, "/x.txt", strings.NewReader(sameAsData), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Put(context.Background(), x.ID, bytes.NewReader([]byte("never dated, but bytes")), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := drive.Copy(at(system(), 2022), d, "drive-1", drive.ByID(x.ID), drive.At{Version: raw.VersionID}, d, "/copy.txt", drive.Condition{})
	if err != nil {
		t.Fatalf("copy of an undated version: %v", err)
	}
	notStub(t, store, c.ID, c.Version.ID)
	if got := read(t, system(), d, drive.ByPath("/copy.txt"), drive.At{}); got != "never dated, but bytes" {
		t.Fatalf("the copy reads %q", got)
	}
}

// notStub fails if key's version is a stub.
func notStub(t *testing.T, store objstore.Store, key, version string) {
	t.Helper()
	obj, err := store.HeadVersion(context.Background(), key, version)
	if err != nil {
		t.Fatal(err)
	}
	if icbfs.IsRef(obj) {
		t.Fatalf("%s version %s is a stub", key, version)
	}
}
