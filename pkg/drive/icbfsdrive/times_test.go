package icbfsdrive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestMirrorKeepsSourceTimes: a mirrored file and folder have their
// creation and modified times at the source, not the copy's, through new
// versions; and an item copied before the daemon passed them gets them
// later with no download and no new version.
func TestMirrorKeepsSourceTimes(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "times", Size: 1 << 30, RootMode: 0o777}, "")
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	born := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	edited := time.Date(2021, 8, 9, 10, 11, 12, 0, time.UTC)
	timed := func(it drive.SourceItem, created time.Time) drive.SourceItem {
		it.Created = created
		return it
	}
	same := func(what string, it drive.Item, modified, created time.Time) {
		t.Helper()
		if !it.Modified.Equal(modified) || !it.Created.Equal(created) {
			t.Fatalf("%s: modified %v, created %v; want %v, %v", what, it.Modified, it.Created, modified, created)
		}
		// As the mounts see it, too.
		st, err := d.Filesystem().Stat(context.Background(), it.ID, map[string]icbfs.EntryType{drive.TypeFile: icbfs.TypeFile, drive.TypeDir: icbfs.TypeDir}[it.Type])
		if err != nil {
			t.Fatal(err)
		}
		if !st.Btime.Equal(created) {
			t.Fatalf("%s: the filesystem's creation time %v, want %v", what, st.Btime, created)
		}
	}

	// A new file: both times in the write.
	a := apply(t, ctx, m, "new file", timed(src.item("doc", "1.0", "/Docs/brief.txt", "v1", edited), born), drive.Created)
	same("new file", a.Item, edited, born)
	apply(t, ctx, m, "same again", timed(src.item("doc", "1.0", "/Docs/brief.txt", "v1", edited), born), drive.Unchanged)

	// A new version keeps the creation time; the modified time follows.
	later := edited.AddDate(1, 0, 0)
	a = apply(t, ctx, m, "new version", timed(src.item("doc", "2.0", "/Docs/brief.txt", "v2", later), born), drive.Updated)
	same("new version", a.Item, later, born)

	// Copied before the daemon passed a creation time: the next pass sets
	// it, with no download and no new version.
	apply(t, ctx, m, "untimed", src.item("old", "1.0", "/Docs/old.txt", "old", edited), drive.Created)
	versions := func(id string) int {
		v, err := d.Versions(ctx, drive.ByID(id))
		if err != nil {
			t.Fatal(err)
		}
		return len(v)
	}
	opens := src.opens.Load()
	a = apply(t, ctx, m, "retimed", timed(src.item("old", "1.0", "/Docs/old.txt", "old", edited), born), drive.Retimed)
	same("retimed", a.Item, edited, born)
	if src.opens.Load() != opens || versions(a.Item.ID) != 1 {
		t.Fatalf("retiming downloaded (%v) or made a version (%d versions)", src.opens.Load() != opens, versions(a.Item.ID))
	}
	apply(t, ctx, m, "retimed again", timed(src.item("old", "1.0", "/Docs/old.txt", "old", edited), born), drive.Unchanged)

	// A folder: both times when it's made; after that only its creation
	// time follows the source, its modified time being its entries'.
	dir := drive.SourceItem{Origin: drive.Origin{ID: "folder", Path: "/src/Reports"}, Path: "/Reports", Type: drive.TypeDir, ModTime: edited, Created: born}
	a = apply(t, ctx, m, "new folder", dir, drive.Created)
	same("new folder", a.Item, edited, born)
	apply(t, ctx, m, "folder again", dir, drive.Unchanged)

	// A folder the mirror adopts (made as a file's parent first) gets its
	// creation time.
	apply(t, ctx, m, "file in a new folder", timed(src.item("memo", "1.0", "/Projects/memo.txt", "m", edited), born), drive.Created)
	parent, err := d.Stat(ctx, drive.ByPath("/Projects"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	a = apply(t, ctx, m, "adopted folder", drive.SourceItem{Origin: drive.Origin{ID: "projects", Path: "/src/Projects"}, Path: "/Projects", Type: drive.TypeDir, ModTime: edited, Created: born}, drive.Created)
	same("adopted folder", a.Item, parent.Modified, born)
}

// TestWriteAndSetTimes: the drive API's own calls, without a mirror.
func TestWriteAndSetTimes(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "settimes", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	born := time.Date(2018, 1, 2, 3, 4, 5, 600, time.UTC)
	edited := time.Date(2020, 6, 7, 8, 9, 10, 0, time.UTC)

	it, err := d.Write(ctx, "/a.txt", strings.NewReader("a"), drive.Condition{}, drive.WriteOptions{ModTime: edited, Created: born})
	if err != nil {
		t.Fatal(err)
	}
	if !it.Created.Equal(born) || !it.Modified.Equal(edited) {
		t.Fatalf("written: created %v, modified %v", it.Created, it.Modified)
	}
	// A later write without times keeps the creation time; the modified
	// time is the write's again.
	it, err = d.Write(ctx, "/a.txt", strings.NewReader("b"), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !it.Created.Equal(born) || it.Modified.Equal(edited) {
		t.Fatalf("rewritten: created %v, modified %v", it.Created, it.Modified)
	}

	dir, err := d.Mkdir(ctx, "/d", false)
	if err != nil {
		t.Fatal(err)
	}
	var ts drive.TimeSetter = d
	got, err := ts.SetTimes(ctx, drive.ByID(dir.ID), edited, born)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created.Equal(born) || !got.Modified.Equal(edited) {
		t.Fatalf("folder: created %v, modified %v", got.Created, got.Modified)
	}
	// Zero leaves a time as it is.
	if got, err = ts.SetTimes(ctx, drive.ByPath("/d"), time.Time{}, born.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if !got.Modified.Equal(edited) || !got.Created.Equal(born.AddDate(1, 0, 0)) {
		t.Fatalf("folder after: created %v, modified %v", got.Created, got.Modified)
	}
}
