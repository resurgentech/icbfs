package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestAppObjects: a tool's objects beside the drive round-trip, are
// listed by tool, show nowhere in the tree, survive an item's purge and
// the trash's, are read as of a past time, and are known to fsck.
func TestAppObjects(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "appobj", Size: 1 << 30, RootMode: 0o755}, "") // the root: only its owner writes
	ctx := system()
	if !d.Capabilities().AppObjects {
		t.Fatal("the drive doesn't say it keeps app objects")
	}
	if err := d.SetAppObject(ctx, "mirror.names", []byte(`{"1":"first"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	between, _ := d.fs.Store().ServerTime(context.Background())
	time.Sleep(1100 * time.Millisecond)
	if err := d.SetAppObject(ctx, "mirror.names", []byte(`{"1":"first","2":"second"}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAppObject(ctx, "mirror.users", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAppObject(ctx, "other.thing", []byte(`x`)); err != nil {
		t.Fatal(err)
	}
	got, err := d.AppObject(ctx, "mirror.names")
	if err != nil || !bytes.Equal(got, []byte(`{"1":"first","2":"second"}`)) {
		t.Fatalf("get: %q, %v", got, err)
	}
	if names, err := d.ListAppObjects(ctx, "mirror."); err != nil || strings.Join(names, ",") != "mirror.names,mirror.users" {
		t.Fatalf("list mirror: %v, %v", names, err)
	}
	if names, err := d.ListAppObjects(ctx, ""); err != nil || len(names) != 3 {
		t.Fatalf("list all: %v, %v", names, err)
	}
	for _, bad := range []string{"nodot", "drive.x", "Mirror.x"} {
		if err := d.SetAppObject(ctx, bad, []byte("v")); !errors.Is(err, drive.ErrInvalid) {
			t.Fatalf("%q: %v; want ErrInvalid", bad, err)
		}
	}
	if err := d.SetAppObject(ctx, "mirror.big", make([]byte, icbfs.MaxAppObjectSize+1)); !errors.Is(err, drive.ErrTooLarge) {
		t.Fatalf("too big: %v; want ErrTooLarge", err)
	}
	// Nothing in the tree.
	write(t, ctx, d, "/doc.txt", "d", drive.Condition{})
	if page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("the tree: %+v, %v", page.Items, err)
	}
	// As of between the two puts: the first.
	v, err := d.at(ctx, drive.At{Time: between})
	if err != nil {
		t.Fatal(err)
	}
	if old, _, err := v.GetAppObject(ctx, "mirror.names"); err != nil || string(old) != `{"1":"first"}` {
		t.Fatalf("as of between: %q, %v", old, err)
	}
	// A purge of an item, and of the trash, leave them.
	if err := d.Delete(ctx, drive.ByPath("/doc.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	page, err := d.Trash(ctx, drive.TrashOptions{})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("trash: %+v, %v", page, err)
	}
	if err := d.Purge(ctx, drive.ByID(page.Entries[0].ItemID), false); err != nil {
		t.Fatal(err)
	}
	if names, err := d.ListAppObjects(ctx, ""); err != nil || len(names) != 3 {
		t.Fatalf("after a purge: %v, %v", names, err)
	}
	report, err := d.fs.Check(context.Background())
	if err != nil || report.Unreachable != 0 || len(report.Problems) != 0 {
		t.Fatalf("fsck: unreachable %d, problems %+v, %v", report.Unreachable, report.Problems, err)
	}
	// Gone when deleted; a second delete is not found.
	if err := d.DeleteAppObject(ctx, "other.thing"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AppObject(ctx, "other.thing"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := d.DeleteAppObject(ctx, "other.thing"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}
	// A reader without write access can read, not write.
	reader := as(1001, 1001)
	if _, err := d.AppObject(reader, "mirror.names"); err != nil {
		t.Fatalf("a reader: %v", err)
	}
	if err := d.SetAppObject(reader, "mirror.names", []byte("no")); err == nil {
		t.Fatal("a reader wrote an app object")
	}
}

// TestConditionalAppObjects: an app object's writes conditioned on the
// version read, as a file's are: a stale version loses with the current
// one named, a first write can insist on there being nothing, a delete
// can insist on a version, and the unconditional calls still work.
func TestConditionalAppObjects(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "appobjcond", Size: 1 << 30, RootMode: 0o755}, "")
	ctx := system()
	if !d.Capabilities().ConditionalAppObjects {
		t.Fatal("the drive doesn't say its app objects are conditional")
	}
	var c drive.ConditionalAppObjects = d

	// A first write that insists on nothing being there.
	v1, err := c.SetAppObjectIf(ctx, "tool.table", []byte("one"), drive.Condition{IfNoneMatch: true})
	if err != nil || v1 == "" {
		t.Fatalf("first write: %q, %v", v1, err)
	}
	if _, err := c.SetAppObjectIf(ctx, "tool.table", []byte("again"), drive.Condition{IfNoneMatch: true}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("second first-write: %v; want ErrPrecondition", err)
	}
	data, got, err := c.AppObjectVersion(ctx, "tool.table")
	if err != nil || string(data) != "one" || got != v1 {
		t.Fatalf("read: %q %q, %v; want one %q", data, got, err, v1)
	}

	// Two writers from one version: the second loses, and is told the
	// current version.
	v2, err := c.SetAppObjectIf(ctx, "tool.table", []byte("two"), drive.Condition{IfMatch: v1})
	if err != nil || v2 == "" || v2 == v1 {
		t.Fatalf("write from v1: %q, %v", v2, err)
	}
	_, err = c.SetAppObjectIf(ctx, "tool.table", []byte("two-prime"), drive.Condition{IfMatch: v1})
	var pe *drive.PreconditionError
	if !errors.As(err, &pe) || pe.Current != v2 {
		t.Fatalf("stale write: %v; want PreconditionError with current %q", err, v2)
	}
	if data, _, err := c.AppObjectVersion(ctx, "tool.table"); err != nil || string(data) != "two" {
		t.Fatalf("after the lost race: %q, %v", data, err)
	}
	// A version an object never had, and a condition on an object that
	// doesn't exist.
	if _, err := c.SetAppObjectIf(ctx, "tool.table", []byte("x"), drive.Condition{IfMatch: "nonesuch"}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("made-up version: %v; want ErrPrecondition", err)
	}
	if _, err := c.SetAppObjectIf(ctx, "tool.missing", []byte("x"), drive.Condition{IfMatch: v1}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("IfMatch on nothing: %v; want ErrPrecondition", err)
	}

	// Unconditional still works, and moves the version on.
	if err := c.SetAppObject(ctx, "tool.table", []byte("three")); err != nil {
		t.Fatal(err)
	}
	_, v3, err := c.AppObjectVersion(ctx, "tool.table")
	if err != nil || v3 == v2 {
		t.Fatalf("after an unconditional write: %q, %v", v3, err)
	}

	// Deletes.
	if err := c.DeleteAppObjectIf(ctx, "tool.table", drive.Condition{IfMatch: v2}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("stale delete: %v; want ErrPrecondition", err)
	}
	if err := c.DeleteAppObjectIf(ctx, "tool.table", drive.Condition{IfNoneMatch: true}); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("delete IfNoneMatch: %v; want ErrInvalid", err)
	}
	if err := c.DeleteAppObjectIf(ctx, "tool.table", drive.Condition{IfMatch: v3}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := c.AppObjectVersion(ctx, "tool.table"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("after delete: %v; want ErrNotFound", err)
	}
	// Conditioned on a version, a missing object is the same answer from
	// a delete as from a write: not the version read, and nothing there.
	if err := c.DeleteAppObjectIf(ctx, "tool.table", drive.Condition{IfMatch: v3}); !errors.As(err, &pe) || pe.Current != "" {
		t.Fatalf("delete again: %v; want PreconditionError with current %q", err, "")
	}
	if _, err := c.SetAppObjectIf(ctx, "tool.table", []byte("x"), drive.Condition{IfMatch: v3}); !errors.As(err, &pe) || pe.Current != "" {
		t.Fatalf("write again: %v; want PreconditionError with current %q", err, "")
	}
	if err := c.DeleteAppObject(ctx, "tool.table"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("unconditional delete of nothing: %v; want ErrNotFound", err)
	}

	// Rights as for the unconditional calls: a reader of the root can't write.
	if _, err := c.SetAppObjectIf(as(1000), "tool.table", []byte("x"), drive.Condition{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("write without the right: %v; want ErrForbidden", err)
	}
}

// TestAppObjectVersions: an object written three times lists its three
// versions, newest first, each reads back, a deleted object's versions
// still list and read, and the access checks are AppObject's.
func TestAppObjectVersions(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "appobjver", Size: 1 << 30, RootMode: 0o700}, "")
	ctx := system()
	var c drive.ConditionalAppObjects = d

	if _, err := c.AppObjectVersions(ctx, "mirror.log"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("never written: %v; want ErrNotFound", err)
	}
	bodies := []string{"one", "two!", "three"}
	var ids []string
	for _, b := range bodies {
		v, err := c.SetAppObjectIf(ctx, "mirror.log", []byte(b), drive.Condition{})
		if err != nil || v == "" {
			t.Fatalf("write %q: %q, %v", b, v, err)
		}
		ids = append(ids, v)
		time.Sleep(1100 * time.Millisecond)
	}
	check := func(when string, current bool) {
		t.Helper()
		vs, err := c.AppObjectVersions(ctx, "mirror.log")
		if err != nil || len(vs) != 3 {
			t.Fatalf("%s: %+v, %v; want 3 versions", when, vs, err)
		}
		for i, v := range vs {
			j := 2 - i // newest first
			if v.ID != ids[j] || v.Size != int64(len(bodies[j])) || v.Time.IsZero() || !v.Time.Equal(v.Stored) {
				t.Fatalf("%s: version %d is %+v; want id %q size %d", when, i, v, ids[j], len(bodies[j]))
			}
			if i > 0 && v.Time.After(vs[i-1].Time) {
				t.Fatalf("%s: version %d is newer than %d", when, i, i-1)
			}
			if v.Current != (current && i == 0) {
				t.Fatalf("%s: version %d Current = %v", when, i, v.Current)
			}
			got, err := c.AppObjectAt(ctx, "mirror.log", v.ID)
			if err != nil || string(got) != bodies[j] {
				t.Fatalf("%s: read %q: %q, %v", when, v.ID, got, err)
			}
		}
	}
	check("written", true)
	if err := c.DeleteAppObject(ctx, "mirror.log"); err != nil {
		t.Fatal(err)
	}
	check("deleted", false)

	if _, err := c.AppObjectAt(ctx, "mirror.log", "no-such-version"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("unknown version: %v; want ErrNotFound", err)
	}
	if _, err := c.AppObjectVersions(ctx, "nodot"); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("bad name, versions: %v; want ErrInvalid", err)
	}
	if _, err := c.AppObjectAt(ctx, "nodot", ids[0]); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("bad name, at: %v; want ErrInvalid", err)
	}
	// An actor who can't read the root is refused as AppObject refuses.
	other := as(1001, 1001)
	_, want := c.AppObject(other, "mirror.log")
	if want == nil {
		t.Fatal("AppObject let a non-reader through")
	}
	if _, err := c.AppObjectVersions(other, "mirror.log"); !errors.Is(err, drive.ErrForbidden) || !errors.Is(want, drive.ErrForbidden) {
		t.Fatalf("versions as non-reader: %v (AppObject: %v)", err, want)
	}
	if _, err := c.AppObjectAt(other, "mirror.log", ids[0]); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("at as non-reader: %v", err)
	}
}

// TestAppObjectNamePart: after "<tool>." an app object's name may hold
// any character but NUL: a slash, a space, git's ref specials, "..", a
// percent, a plus and a non-ASCII letter each round-trip through every
// app-object call, a listing by a prefix inside or before the slash finds
// them, fsck reports nothing and the tree stays empty. NUL is refused.
func TestAppObjectNamePart(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "appobjnames", Size: 1 << 30, RootMode: 0o755}, "")
	ctx := system()
	fs := d.fs
	id := strings.Repeat("0123456789abcdef", 4)
	note := "clone.note.desk/comments." + id
	// Through the drive first, as a caller meets it.
	if err := d.SetAppObject(ctx, note, []byte("via drive")); err != nil {
		t.Fatalf("drive set %q: %v", note, err)
	}
	if got, err := d.AppObject(ctx, note); err != nil || string(got) != "via drive" {
		t.Fatalf("drive get %q: %q, %v", note, got, err)
	}
	names := []string{
		note,
		"clone.note.a b." + id,
		`clone.note.~^:?*[\.` + id,
		"clone.note.a..b." + id,
		"clone.note.100%." + id,
		"clone.note.a+b." + id,
		"clone.note.café/ü." + id,
		"other.x/y",
	}
	bg := context.Background()
	for _, n := range names {
		first, second := []byte("one "+n), []byte("two "+n)
		v1, err := fs.PutAppObject(bg, n, first)
		if err != nil {
			t.Fatalf("put %q: %v", n, err)
		}
		v2, err := fs.PutAppObjectIf(bg, n, second, v1.ETag, false)
		if err != nil {
			t.Fatalf("put if %q: %v", n, err)
		}
		if got, obj, err := fs.GetAppObject(bg, n); err != nil || !bytes.Equal(got, second) || obj.VersionID != v2.VersionID {
			t.Fatalf("get %q: %q, %+v, %v", n, got, obj, err)
		}
		if obj, err := fs.HeadAppObject(bg, n); err != nil || obj.VersionID != v2.VersionID {
			t.Fatalf("head %q: %+v, %v", n, obj, err)
		}
		hist, err := fs.AppObjectHistory(bg, n)
		if err != nil || len(hist) < 2 || hist[0].VersionID != v2.VersionID {
			t.Fatalf("history %q: %+v, %v", n, hist, err)
		}
		if got, err := fs.GetAppObjectVersion(bg, n, v1.VersionID); err != nil || !bytes.Equal(got, first) {
			t.Fatalf("version %q: %q, %v", n, got, err)
		}

	}
	all, err := fs.ListAppObjects(bg, "")
	if err != nil || len(all) != len(names) {
		t.Fatalf("list all: %q, %v; want %d", all, err, len(names))
	}
	for _, c := range []struct {
		prefix string
		want   []string
	}{
		{"clone.note.desk/com", []string{note}}, // ends inside the part after the slash
		{"clone.note.desk", []string{note}},     // ends before the slash
		{"clone.note.café/", []string{"clone.note.café/ü." + id}},
		{"other.x/", []string{"other.x/y"}},
	} {
		got, err := fs.ListAppObjects(bg, c.prefix)
		if err != nil || strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Fatalf("list %q: %q, %v; want %q", c.prefix, got, err, c.want)
		}
	}
	report, err := fs.Check(bg)
	if err != nil || report.Unreachable != 0 || len(report.Problems) != 0 {
		t.Fatalf("fsck: unreachable %d, problems %+v, %v", report.Unreachable, report.Problems, err)
	}
	if page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("the tree: %+v, %v", page.Items, err)
	}
	for _, n := range names {
		if err := fs.DeleteAppObject(bg, n); err != nil {
			t.Fatalf("delete %q: %v", n, err)
		}
		if _, _, err := fs.GetAppObject(bg, n); !errors.Is(err, icbfs.ErrNotFound) {
			t.Fatalf("after delete %q: %v", n, err)
		}
	}
	// NUL, anywhere in the name, is refused.
	for _, bad := range []string{"clone.note.a\x00b", "clone.\x00", "clone.x\x00", "clo\x00ne.x"} {
		if _, err := fs.PutAppObject(bg, bad, []byte("v")); !errors.Is(err, icbfs.ErrInvalidArgument) {
			t.Fatalf("put %q: %v; want ErrInvalidArgument", bad, err)
		}
		if _, _, err := fs.GetAppObject(bg, bad); !errors.Is(err, icbfs.ErrInvalidArgument) {
			t.Fatalf("get %q: %v; want ErrInvalidArgument", bad, err)
		}
	}
	// The tool part is unchanged: no dot, an empty tool or an empty name.
	for _, bad := range []string{"nodot", ".x", "clone.", "Clone.x", "a/b.x"} {
		if _, err := fs.PutAppObject(bg, bad, []byte("v")); !errors.Is(err, icbfs.ErrInvalidArgument) {
			t.Fatalf("put %q: %v; want ErrInvalidArgument", bad, err)
		}
	}
}
