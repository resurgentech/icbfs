package icbfsdrive

import (
	"reflect"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestTrashEntriesCarryOrigin: a mirror (icbfs-sharepoint) trashes what
// its source deleted, and when the source brings the item back with the
// same id, finds the trash entry by origin and restores it, history and
// all, instead of copying every version again.
//
//   - A deleted item's trash entry carries the origin it had, and so does
//     every entry of a recursive delete, the folder's included.
//   - Trash filters by origin: by source drive, and by source item.
//   - Restoring keeps the origin and every content version; restoring a
//     folder brings back its contents with theirs.
func TestTrashEntriesCarryOrigin(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "mirror", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	copied := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	origin := func(id, version string) drive.Origin {
		return drive.Origin{Drive: "sharepoint:site/library", ID: id, Version: version, Path: "/x", Copied: copied}
	}
	setOrigin := func(p string, o drive.Origin) {
		t.Helper()
		if err := d.SetOrigin(ctx, drive.ByPath(p), o); err != nil {
			t.Fatalf("set origin %s: %v", p, err)
		}
	}

	// A file with three versions, its origin at the last.
	for _, body := range []string{"one", "one two", "one two three"} {
		write(t, ctx, d, "/docs/report.docx", body, drive.Condition{})
	}
	setOrigin("/docs/report.docx", origin("sp-report", "3.0"))
	before, err := d.Versions(ctx, drive.ByPath("/docs/report.docx"))
	if err != nil || len(before) != 3 {
		t.Fatalf("versions before: %v, %v", before, err)
	}

	// A folder and two files in it, each with its origin, and a file with
	// none.
	write(t, ctx, d, "/matter/a.txt", "a", drive.Condition{})
	write(t, ctx, d, "/matter/b.txt", "b", drive.Condition{})
	setOrigin("/matter", origin("sp-matter", ""))
	setOrigin("/matter/a.txt", origin("sp-a", "1.0"))
	setOrigin("/matter/b.txt", origin("sp-b", "1.0"))
	write(t, ctx, d, "/local.txt", "made here", drive.Condition{})

	if err := d.Delete(ctx, drive.ByPath("/docs/report.docx"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, drive.ByPath("/matter"), true, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, drive.ByPath("/local.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}

	all, err := d.Trash(ctx, drive.TrashOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]drive.TrashEntry{}
	for _, e := range all.Entries {
		byPath[e.Path] = e
	}
	for p, id := range map[string]string{"/docs/report.docx": "sp-report", "/matter": "sp-matter", "/matter/a.txt": "sp-a", "/matter/b.txt": "sp-b"} {
		e, ok := byPath[p]
		if !ok {
			t.Fatalf("no trash entry for %s in %+v", p, all.Entries)
		}
		if e.Origin == nil || e.Origin.ID != id || e.Origin.Drive != "sharepoint:site/library" || !e.Origin.Copied.Equal(copied) {
			t.Errorf("%s: trash entry origin %+v, want id %s", p, e.Origin, id)
		}
	}
	if e := byPath["/local.txt"]; e.Origin != nil {
		t.Errorf("an item with no origin got one in the trash: %+v", e.Origin)
	}
	if e := byPath["/docs/report.docx"]; e.Origin.Version != "3.0" {
		t.Errorf("report's origin version %q, want 3.0 (what the mirror copies after)", e.Origin.Version)
	}

	// Filtering: by source drive (every entry with an origin), by source
	// item (just that one), and a page of one still filtered.
	fromSP, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "sharepoint:site/library"})
	if err != nil || len(fromSP.Entries) != 4 {
		t.Fatalf("filtered by origin drive: %d entries, %v; want 4", len(fromSP.Entries), err)
	}
	one, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "sharepoint:site/library", OriginID: "sp-b"})
	if err != nil || len(one.Entries) != 1 || one.Entries[0].Path != "/matter/b.txt" {
		t.Fatalf("filtered by origin id: %+v, %v", one.Entries, err)
	}
	if none, _ := d.Trash(ctx, drive.TrashOptions{OriginDrive: "other"}); len(none.Entries) != 0 {
		t.Fatalf("another source drive matched: %+v", none.Entries)
	}
	page, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "sharepoint:site/library", Limit: 1})
	if err != nil || len(page.Entries) != 1 || page.Next == "" {
		t.Fatalf("a page of one: %+v, next %q, %v", page.Entries, page.Next, err)
	}
	rest, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: "sharepoint:site/library", Cursor: page.Next})
	if err != nil || len(rest.Entries) != 3 {
		t.Fatalf("the rest after the cursor: %d entries, %v; want 3", len(rest.Entries), err)
	}

	// Restore the file by its origin, to where the source has it now.
	found, _ := d.Trash(ctx, drive.TrashOptions{OriginDrive: "sharepoint:site/library", OriginID: "sp-report"})
	if len(found.Entries) != 1 {
		t.Fatalf("finding the report by origin: %+v", found.Entries)
	}
	restored, err := d.Restore(ctx, found.Entries[0].ID, drive.RestoreOptions{To: "/moved/report.docx", Parents: true})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Origin == nil || restored.Origin.ID != "sp-report" || restored.Origin.Version != "3.0" {
		t.Fatalf("restored item's origin %+v", restored.Origin)
	}
	after, err := d.Versions(ctx, drive.ByPath("/moved/report.docx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("versions after restore: %d, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Size != before[i].Size {
			t.Errorf("version %d after restore %+v, before %+v", i, after[i], before[i])
		}
	}
	if got := read(t, ctx, d, drive.ByPath("/moved/report.docx"), drive.At{}); got != "one two three" {
		t.Fatalf("restored content %q", got)
	}

	// Restore the folder: its batch comes back, and each file keeps its
	// origin.
	folder := byPath["/matter"]
	if _, err := d.Restore(ctx, folder.ID, drive.RestoreOptions{}); err != nil {
		t.Fatalf("restore folder: %v", err)
	}
	for p, id := range map[string]string{"/matter": "sp-matter", "/matter/a.txt": "sp-a", "/matter/b.txt": "sp-b"} {
		st, err := d.Stat(ctx, drive.ByPath(p), drive.At{})
		if err != nil {
			t.Fatalf("stat %s after restoring the folder: %v", p, err)
		}
		want := origin(id, map[string]string{"sp-matter": "", "sp-a": "1.0", "sp-b": "1.0"}[id])
		if st.Origin == nil || !reflect.DeepEqual(*st.Origin, want) {
			t.Errorf("%s after restore: origin %+v, want %+v", p, st.Origin, want)
		}
	}
}
