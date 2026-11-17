package icbfsdrive

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestBackslashIsNotASeparator (T82-14): "/" is the drive's only
// separator. "/A/..\B\contract.txt" is one name in /A, so it reaches
// nothing of /B; a name with a backslash is a legal POSIX name, made,
// read, listed, renamed and removed like any other. "." and ".." are
// tidied within the drive's root, as before T82-14 (Drafts and mirrors
// refuse them before they call the drive). A symlink is not followed.
func TestBackslashIsNotASeparator(t *testing.T) {
	store := miniotest.Store(t)
	whole := newDrive(t, store, icbfs.Settings{Name: "backslash", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	write(t, ctx, whole, "/A/letter.txt", "A\n", drive.Condition{})
	write(t, ctx, whole, "/B/contract.txt", "ORIGINAL B\n", drive.Condition{})
	d := newDrive(t, store, icbfs.Settings{Name: "backslash", Size: 1 << 30, RootMode: 0o777}, "/A")

	// Through the drive rooted at /A: nothing of /B, whatever the shape.
	for _, p := range []string{`/..\B\contract.txt`, `\..\B\contract.txt`, `..\B/contract.txt`, `/x\..\..\B\contract.txt`} {
		if _, _, err := d.Read(ctx, drive.ByPath(p), drive.At{}, nil); !errors.Is(err, drive.ErrNotFound) {
			t.Errorf("read %q: %v, want not found", p, err)
		}
		if err := d.Delete(ctx, drive.ByPath(p), true, drive.Condition{}); !errors.Is(err, drive.ErrNotFound) {
			t.Errorf("delete %q: %v, want not found", p, err)
		}
	}
	// The finding's shape: a folder's path glued onto a caller's string,
	// read through the whole filesystem (as icbfs-git did): one name in
	// /A, not /B's file.
	if _, _, err := whole.Read(ctx, drive.ByPath(`/A/..\B\contract.txt`), drive.At{}, nil); !errors.Is(err, drive.ErrNotFound) {
		t.Errorf(`whole: read /A/..\B\contract.txt: %v, want not found`, err)
	}
	// A write makes the name in /A, as on any POSIX filesystem.
	write(t, ctx, d, `/..\B\contract.txt`, "ONE NAME\n", drive.Condition{})
	if got := read(t, ctx, whole, drive.ByPath(`/A/..\B\contract.txt`), drive.At{}); got != "ONE NAME\n" {
		t.Fatalf(`/A/..\B\contract.txt = %q`, got)
	}
	// ".." is tidied within the root: the drive at /A never leaves it.
	if got := read(t, ctx, d, drive.ByPath("/../letter.txt"), drive.At{}); got != "A\n" {
		t.Fatalf("/../letter.txt = %q", got)
	}
	if _, _, err := d.Read(ctx, drive.ByPath("/../B/contract.txt"), drive.At{}, nil); !errors.Is(err, drive.ErrNotFound) {
		t.Errorf("read /../B/contract.txt from /A: %v, want not found", err)
	}
	if got := read(t, ctx, whole, drive.ByPath("/B/contract.txt"), drive.At{}); got != "ORIGINAL B\n" {
		t.Fatalf("/B/contract.txt = %q", got)
	}
	// Names with a backslash: made, listed, renamed, removed.
	if _, err := d.Mkdir(ctx, `/d\e`, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(ctx, drive.ByPath("/letter.txt"), `/l\x.txt`, false); err != nil {
		t.Fatal(err)
	}
	ls, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range ls.Items {
		names = append(names, it.Name)
	}
	if got := strings.Join(names, "|"); !strings.Contains(got, `l\x.txt`) || !strings.Contains(got, `d\e`) || !strings.Contains(got, `..\B\contract.txt`) {
		t.Fatalf("listing of /A: %s", got)
	}
	if _, err := d.Move(ctx, drive.ByPath(`/l\x.txt`), "/letter.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, drive.ByPath(`/d\e`), true, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	// A symlink in the way is not followed.
	a, err := whole.resolvePath(ctx, whole.fs, "/A")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := whole.fs.Symlink(ctx, a.key, "link", "../B", 0, 0); err != nil {
		t.Fatal(err)
	}
	whole.changed(a)
	d.changed(a)
	if _, _, err := d.Read(ctx, drive.ByPath("/link/contract.txt"), drive.At{}, nil); !errors.Is(err, drive.ErrNotDir) {
		t.Fatalf("read through a symlink: %v, want ErrNotDir", err)
	}
}

// TestHintedLookupOfOddNames (T82-14 over T82-13): a mirror's hint is a
// path like any other: "/" alone separates. A held path whose name holds
// %5C (how the mirrors now write a backslash) finds its item with no
// walk; so does one whose stored name holds a backslash itself; that
// path split at the backslash, as the code before T82-14 read it, finds
// nothing there and falls through to the answer no hint gives. An item
// the old code mis-filed (at another matter's path) is found where it
// is, by its hint, for the next pass to move it. A hint never answers
// for another item (the lookup checks the item's key).
func TestHintedLookupOfOddNames(t *testing.T) {
	store := miniotest.Store(t)
	s := icbfs.Settings{Name: "hintodd", Size: 1 << 30, RootMode: 0o777}
	ctx := system()
	d := newDrive(t, store, s, "")
	m := drive.NewMirror(d, srcDrive)
	src := &source{}
	for _, c := range []struct{ id, path string }{
		{"pct", "/M1/a%5Cb.pdf"},
		{"bs", `/M1/x\y.txt`},        // a name the drive stores with a backslash
		{"misfiled", "/M2/real.pdf"}, // where the old code put M1's "..\M2\real.pdf"
		{"plain", "/M1/plain.txt"},
	} {
		if _, err := m.Apply(ctx, src.item(c.id, "1", c.path, mirrorData(c.id), time.Time{})); err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
	}
	restart := func() *Drive { return newDrive(t, store, s, "") }
	lookup := func(d *Drive, id, hint string) (found, int64) {
		t.Helper()
		before := d.searches.Load()
		f := foundOf(d.FindByOriginHint(ctx, srcDrive, id, hint))
		return f, d.searches.Load() - before
	}
	for _, c := range []struct{ id, hint, want string }{
		{"pct", "/M1/a%5Cb.pdf", "/M1/a%5Cb.pdf"},
		{"bs", `/M1/x\y.txt`, `/M1/x\y.txt`},
		{"misfiled", "/M2/real.pdf", "/M2/real.pdf"},
	} {
		got, walks := lookup(restart(), c.id, c.hint)
		if got.path != c.want || got.origin != c.id || walks != 0 {
			t.Errorf("%s hinted at %s: %+v, %d walks; want it at %s with no walk", c.id, c.hint, got, walks, c.want)
		}
	}
	// The backslash name split as the old code split it: nothing there.
	noHint, _ := lookup(restart(), "bs", "")
	if got, _ := lookup(restart(), "bs", "/M1/x/y.txt"); got != noHint {
		t.Errorf("bs hinted at /M1/x/y.txt: %+v; with no hint %+v", got, noHint)
	}
	// Another item's path as a hint: never that item.
	if got, _ := lookup(restart(), "pct", "/M1/plain.txt"); got.origin != "pct" || got.path != "/M1/a%5Cb.pdf" {
		t.Errorf("pct hinted at plain's path: %+v", got)
	}
}
