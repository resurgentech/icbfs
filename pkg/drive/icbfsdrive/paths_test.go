package icbfsdrive

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestPathRuleNoEscape (T82-14): a backslash is a name's character, not
// a separator, so "/A/..\B\contract.txt" names nothing in /B; "." and
// ".." and empty names are refused, not tidied; a new name may not hold
// a backslash; and a name stored with one (made below the drive) is
// still listed, read, renamed to a legal name and removed. A symlink is
// never followed by a path.
func TestPathRuleNoEscape(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "pathrule", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	write(t, ctx, d, "/A/letter.txt", "A\n", drive.Condition{})
	write(t, ctx, d, "/B/contract.txt", "ORIGINAL B\n", drive.Condition{})

	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, drive.ErrInvalidName) {
			t.Errorf("%s: %v, want ErrInvalidName", what, err)
		}
	}
	absent := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, drive.ErrNotFound) && !errors.Is(err, drive.ErrInvalidName) {
			t.Errorf("%s: %v, want not found or invalid", what, err)
		}
	}
	for _, p := range []string{`/A/..\B\contract.txt`, `/A\..\B\contract.txt`, `\B\contract.txt`, `/A/../B/contract.txt`, `/A/./../B/contract.txt`, `/A//../B/contract.txt`, `/A/..\..\B/contract.txt`} {
		_, _, err := d.Read(ctx, drive.ByPath(p), drive.At{}, nil)
		absent("read "+p, err)
		_, err = d.Stat(ctx, drive.ByPath(p), drive.At{})
		absent("stat "+p, err)
		_, err = d.Write(ctx, p, strings.NewReader("EVIL\n"), drive.Condition{}, drive.WriteOptions{Parents: true})
		refused("write "+p, err)
		err = d.Delete(ctx, drive.ByPath(p), false, drive.Condition{})
		absent("delete "+p, err)
		_, err = d.Move(ctx, drive.ByPath("/A/letter.txt"), p, true)
		absent("move to "+p, err)
		_, err = d.Mkdir(ctx, p, true)
		refused("mkdir "+p, err)
		_, err = d.List(ctx, drive.ByPath(p), drive.ListOptions{})
		absent("list "+p, err)
	}
	for _, p := range []string{"/A/../B", "/A/..", "/A/./letter.txt", "/A//letter.txt", "/A/letter.txt/"} {
		_, err := d.Stat(ctx, drive.ByPath(p), drive.At{})
		refused("stat "+p, err)
	}
	if got := read(t, ctx, d, drive.ByPath("/B/contract.txt"), drive.At{}); got != "ORIGINAL B\n" {
		t.Fatalf("/B/contract.txt = %q", got)
	}
	if got := read(t, ctx, d, drive.ByPath("/A/letter.txt"), drive.At{}); got != "A\n" {
		t.Fatalf("/A/letter.txt = %q", got)
	}
	ls, err := d.List(ctx, drive.ByPath("/A"), drive.ListOptions{})
	if err != nil || len(ls.Items) != 1 {
		t.Fatalf("/A holds %+v, %v; want only letter.txt", ls.Items, err)
	}

	// A new name with a backslash: refused on every way a name is made.
	_, err = d.Write(ctx, `/A/a\b.txt`, strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{})
	refused("create a\\b.txt", err)
	_, err = d.Mkdir(ctx, `/A/d\e`, false)
	refused("mkdir d\\e", err)
	_, err = d.Move(ctx, drive.ByPath("/A/letter.txt"), `/A/l\x.txt`, false)
	refused("rename to l\\x.txt", err)

	// A name already stored with a backslash (made through the core, as a
	// POSIX mount or an old write could): every route still reaches it.
	a, err := d.resolvePath(ctx, d.fs, "/A")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.fs.CreateWith(ctx, a.key, `old\name.txt`, 0o644, 0, 0, []byte("OLD\n"), nil, icbfs.ItemInfo{}); err != nil {
		t.Fatal(err)
	}
	d.changed(a)
	if _, _, err := d.fs.Mkdir(ctx, a.key, `old\dir`, 0o755, 0, 0); err != nil {
		t.Fatal(err)
	}
	d.changed(a)
	old := `/A/old\name.txt`
	if got := read(t, ctx, d, drive.ByPath(old), drive.At{}); got != "OLD\n" {
		t.Fatalf("read %s = %q", old, got)
	}
	write(t, ctx, d, old, "OLD 2\n", drive.Condition{}) // its own name, written again
	write(t, ctx, d, `/A/old\dir/in.txt`, "IN\n", drive.Condition{})
	ls, err = d.List(ctx, drive.ByPath("/A"), drive.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, it := range ls.Items {
		names[it.Name] = true
	}
	if !names[`old\name.txt`] || !names[`old\dir`] {
		t.Fatalf("listing of /A = %v", names)
	}
	if _, err := d.Move(ctx, drive.ByPath(old), "/A/new-name.txt", false); err != nil {
		t.Fatalf("rename %s to a legal name: %v", old, err)
	}
	if got := read(t, ctx, d, drive.ByPath("/A/new-name.txt"), drive.At{}); got != "OLD 2\n" {
		t.Fatalf("renamed = %q", got)
	}
	if err := d.Delete(ctx, drive.ByPath(`/A/old\dir`), true, drive.Condition{}); err != nil {
		t.Fatalf("delete old\\dir: %v", err)
	}

	// A symlink in the way: a path is never followed through it.
	if _, _, err := d.fs.Symlink(ctx, a.key, "link", "../B", 0, 0); err != nil {
		t.Fatal(err)
	}
	d.changed(a)
	_, _, err = d.Read(ctx, drive.ByPath("/A/link/contract.txt"), drive.At{}, nil)
	if !errors.Is(err, drive.ErrNotDir) {
		t.Fatalf("read through a symlink: %v, want ErrNotDir", err)
	}
	_, err = d.Write(ctx, "/A/link/contract.txt", strings.NewReader("EVIL"), drive.Condition{}, drive.WriteOptions{})
	if !errors.Is(err, drive.ErrNotDir) {
		t.Fatalf("write through a symlink: %v, want ErrNotDir", err)
	}
	rc, _, err := d.Read(ctx, drive.ByPath("/B/contract.txt"), drive.At{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "ORIGINAL B\n" {
		t.Fatalf("/B/contract.txt = %q", b)
	}
}

// TestExistingNamesCopied: a write that copies names which already exist
// (drive.WithExistingNames: a layer copied at a commit, a stash put
// back) may make a name with a backslash; ".." and "" stay refused.
func TestExistingNamesCopied(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "copied", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := drive.WithExistingNames(system())
	if _, err := d.Write(ctx, `/c/old\dir/old\x.txt`, strings.NewReader("X"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
		t.Fatalf("copy of a backslash name: %v", err)
	}
	if got := read(t, ctx, d, drive.ByPath(`/c/old\dir/old\x.txt`), drive.At{}); got != "X" {
		t.Fatalf("read = %q", got)
	}
	if _, err := d.Mkdir(ctx, `/c/m\n`, false); err != nil {
		t.Fatalf("mkdir of a copied name: %v", err)
	}
	for _, p := range []string{"/c/../x", "/c//x"} {
		if _, err := d.Write(ctx, p, strings.NewReader("X"), drive.Condition{}, drive.WriteOptions{Parents: true}); !errors.Is(err, drive.ErrInvalidName) {
			t.Errorf("write %s: %v", p, err)
		}
	}
	// Without it, a new backslash name is refused as before.
	if _, err := d.Write(system(), `/c/new\y.txt`, strings.NewReader("Y"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrInvalidName) {
		t.Errorf("a new backslash name: %v", err)
	}
}
