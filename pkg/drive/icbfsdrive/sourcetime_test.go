package icbfsdrive

import (
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

func year(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }

// sourceDrive is a drive on a filesystem whose history starts in 2015, as
// a mirror's does, written as system.
func sourceDrive(t *testing.T, name string) (*Drive, objstore.Store) {
	t.Helper()
	store := miniotest.Store(t)
	return newDrive(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, ""), store
}

func at(ctx context.Context, y int) context.Context { return drive.WithSourceTime(ctx, year(y)) }

// exists reports whether p is there as of t.
func exists(t *testing.T, d *Drive, p string, when time.Time) bool {
	t.Helper()
	_, err := d.Stat(system(), drive.ByPath(p), drive.At{Time: when})
	if err != nil && !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("stat %s as of %v: %v", p, when, err)
	}
	return err == nil
}

// TestVersionsInSourceTime: a file written with versions dated 2021, 2022
// and 2023 lists those dates, and the past shows each version from its
// date (and no file before the first), however recently it was stored.
func TestVersionsInSourceTime(t *testing.T) {
	d, _ := sourceDrive(t, "versions")
	for _, v := range []struct {
		year int
		body string
	}{{2021, "first"}, {2022, "second"}, {2023, "third"}} {
		if _, err := d.Write(at(system(), v.year), "/doc.txt", strings.NewReader(v.body), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatalf("write %d: %v", v.year, err)
		}
	}
	versions, err := d.Versions(system(), drive.ByPath("/doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("%d versions: %+v", len(versions), versions)
	}
	for i, y := range []int{2023, 2022, 2021} {
		if v := versions[i]; !v.Time.Equal(year(y)) || v.Stored.Before(year(2026)) {
			t.Fatalf("version %d: time %v, stored %v; want %d, stored now", i, v.Time, v.Stored, y)
		}
	}
	for _, tc := range []struct {
		when time.Time
		want string // "" = no file
	}{
		{year(2020).AddDate(0, 6, 0), ""},
		{year(2022).AddDate(0, 5, 0), "second"},
		{year(2021), "first"},
		{year(2025), "third"},
		{time.Time{}, "third"},
	} {
		if tc.want == "" {
			if exists(t, d, "/doc.txt", tc.when) {
				t.Fatalf("as of %v: the file is there", tc.when)
			}
			continue
		}
		if got := read(t, system(), d, drive.ByPath("/doc.txt"), drive.At{Time: tc.when}); got != tc.want {
			t.Fatalf("as of %v: %q, want %q", tc.when, got, tc.want)
		}
	}
	it, err := d.Stat(system(), drive.ByPath("/doc.txt"), drive.At{})
	if err != nil || !it.Modified.Equal(year(2023)) || !it.Created.Equal(year(2021)) {
		t.Fatalf("now: modified %v, created %v, %v; want 2023 and 2021", it.Modified, it.Created, err)
	}
}

// TestDeleteInSourceTime: a file created in 2021 and deleted in 2024 is
// there in 2023 and gone in mid-2024, in the trash as deleted in 2024,
// and restorable.
func TestDeleteInSourceTime(t *testing.T) {
	d, _ := sourceDrive(t, "deletes")
	if _, err := d.Write(at(system(), 2021), "/Projects/brief.txt", strings.NewReader("brief"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(drive.WithSourceTime(system(), year(2024).AddDate(0, 2, 0)), drive.ByPath("/Projects/brief.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, d, "/Projects/brief.txt", year(2023)) {
		t.Fatal("not there in 2023")
	}
	if exists(t, d, "/Projects/brief.txt", year(2024).AddDate(0, 6, 0)) || exists(t, d, "/Projects/brief.txt", time.Time{}) {
		t.Fatal("still there after its deletion")
	}
	page, err := d.Trash(system(), drive.TrashOptions{})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("trash: %+v, %v", page, err)
	}
	if e := page.Entries[0]; !e.DeletedAt.Equal(year(2024).AddDate(0, 2, 0)) {
		t.Fatalf("deleted at %v, want 2024-03-01", e.DeletedAt)
	}
	if _, err := d.Restore(system(), page.Entries[0].ID, drive.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, system(), d, drive.ByPath("/Projects/brief.txt"), drive.At{}); got != "brief" {
		t.Fatalf("restored: %q", got)
	}
}

// TestMoveInSourceTime: a file moved in 2022 is in its old folder in a
// view of mid-2021 and in its new one in mid-2022.
func TestMoveInSourceTime(t *testing.T) {
	d, _ := sourceDrive(t, "moves")
	ctx := system()
	for _, dir := range []string{"/Old", "/New"} {
		if _, err := d.Mkdir(at(ctx, 2020), dir, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Write(at(ctx, 2021), "/Old/f.txt", strings.NewReader("f"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(at(ctx, 2022), drive.ByPath("/Old/f.txt"), "/New/f.txt", false); err != nil {
		t.Fatal(err)
	}
	mid21, mid22 := year(2021).AddDate(0, 6, 0), year(2022).AddDate(0, 6, 0)
	if !exists(t, d, "/Old/f.txt", mid21) || exists(t, d, "/New/f.txt", mid21) {
		t.Fatal("mid-2021: not in /Old alone")
	}
	if exists(t, d, "/Old/f.txt", mid22) || !exists(t, d, "/New/f.txt", mid22) {
		t.Fatal("mid-2022: not in /New alone")
	}
	if exists(t, d, "/Old", year(2019)) {
		t.Fatal("/Old is there before it was made")
	}
}

// TestSourceTimeInOrder: a change dated before the state it would change
// fails and changes nothing; a source time in the future is refused.
func TestSourceTimeInOrder(t *testing.T) {
	d, _ := sourceDrive(t, "order")
	ctx := system()
	if _, err := d.Write(at(ctx, 2022), "/dir/new.txt", strings.NewReader("new"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(at(ctx, 2021), "/dir/new.txt", strings.NewReader("older"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrOutOfOrder) {
		t.Fatalf("an older version over a newer one: %v; want ErrOutOfOrder", err)
	}
	if _, err := d.Write(at(ctx, 2021), "/dir/old.txt", strings.NewReader("old"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrOutOfOrder) {
		t.Fatalf("a file into a folder changed since: %v; want ErrOutOfOrder", err)
	}
	if exists(t, d, "/dir/old.txt", time.Time{}) {
		t.Fatal("the refused file was made")
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/new.txt"), drive.At{}); got != "new" {
		t.Fatalf("the newer version became %q", got)
	}
	if _, err := d.Write(drive.WithSourceTime(ctx, time.Now().Add(time.Hour)), "/dir/later.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("a source time an hour ahead: %v; want ErrInvalid", err)
	}
	// Without a source time a change is now's, and always in order.
	if _, err := d.Write(ctx, "/dir/old.txt", strings.NewReader("now"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestSourceTimeReachesOtherCaches: a history cache that has seen the past
// forgets it once a change is made under a source time before then, as a
// history app's would.
func TestSourceTimeReachesOtherCaches(t *testing.T) {
	d, store := sourceDrive(t, "caches")
	ctx := system()
	if _, err := d.Write(at(ctx, 2021), "/a.txt", strings.NewReader("a"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	hist, err := objstore.OpenTempHistoryCache(context.Background(), store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	other, err := New(context.Background(), Config{Store: store, Filesystem: "caches", ID: "other", History: hist, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if exists(t, other, "/b.txt", year(2023)) {
		t.Fatal("b.txt before it was written")
	}
	if _, err := d.Write(at(ctx, 2022), "/b.txt", strings.NewReader("b"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	other.views.lookAgain() // its next look would be within 30 s
	if !exists(t, other, "/b.txt", year(2023)) {
		t.Fatal("the other drive's view of 2023 doesn't have b.txt, written since as of 2022")
	}
}

// TestMirrorInSourceTime: a mirror in source time writes a document's
// history as the source has it: created and first saved in 2021, a new
// version in 2022, moved in 2023, deleted in 2024.
func TestMirrorInSourceTime(t *testing.T) {
	d, _ := sourceDrive(t, "mirror")
	m := drive.NewMirror(d, srcDrive).InSourceTime()
	ctx := system()
	src := &source{}
	v1 := src.item("doc", "1", "/Project/brief.txt", "draft", year(2021))
	v1.Created = year(2021)
	actions(t, "create", must(m.Apply(ctx, v1)), nil, drive.Created)
	v2 := src.item("doc", "2", "/Project/brief.txt", "final", year(2022))
	v2.Created = year(2021)
	actions(t, "new version", must(m.Apply(ctx, v2)), nil, drive.Updated)
	moved := v2
	moved.Path, moved.Changed = "/Closed/brief.txt", year(2023)
	actions(t, "move", must(m.Apply(ctx, moved)), nil, drive.Moved)
	if ok, err := m.Remove(drive.WithSourceTime(ctx, year(2024)), "doc"); !ok || err != nil {
		t.Fatalf("remove: %v, %v", ok, err)
	}

	versions, err := d.Versions(ctx, drive.ByID(must(d.Trash(ctx, drive.TrashOptions{})).Entries[0].ItemID))
	if err != nil || len(versions) != 2 || !versions[0].Time.Equal(year(2022)) || !versions[1].Time.Equal(year(2021)) {
		t.Fatalf("versions: %+v, %v; want 2022 and 2021", versions, err)
	}
	for _, tc := range []struct {
		when       time.Time
		path, want string // want "" = not there
	}{
		{year(2020), "/Project/brief.txt", ""},
		{year(2021).AddDate(0, 6, 0), "/Project/brief.txt", "draft"},
		{year(2022).AddDate(0, 6, 0), "/Project/brief.txt", "final"},
		{year(2023).AddDate(0, 6, 0), "/Project/brief.txt", ""},
		{year(2023).AddDate(0, 6, 0), "/Closed/brief.txt", "final"},
		{year(2024).AddDate(0, 6, 0), "/Closed/brief.txt", ""},
	} {
		if tc.want == "" {
			if exists(t, d, tc.path, tc.when) {
				t.Fatalf("as of %v: %s is there", tc.when, tc.path)
			}
			continue
		}
		if got := read(t, ctx, d, drive.ByPath(tc.path), drive.At{Time: tc.when}); got != tc.want {
			t.Fatalf("as of %v: %s is %q, want %q", tc.when, tc.path, got, tc.want)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// names lists dir's entry names in a view of when (now if zero).
func names(t *testing.T, d *Drive, dir string, when time.Time) string {
	t.Helper()
	ctx := system()
	n, err := d.resolve(ctx, d.fs, drive.ByPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := d.at(ctx, drive.At{Time: when})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fsys.ReadDir(ctx, n.key)
	if err != nil {
		t.Fatalf("list %s as of %v: %v", dir, when, err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

// TestReplaceFolderHistory: a file the mirror learned of late (made in
// 2022 in a folder already changed in 2023) can't be written in order;
// once it's there, the caller rewrites the folder's history to show it
// from 2022. The folder spans several blocks, each state is a whole tree,
// and fsck finds nothing wrong after.
func TestReplaceFolderHistory(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "repair", Size: 1 << 30, RootMode: 0o777, Created: year(2015), DirBlockEntries: 4}, "")
	ctx := system()
	if _, err := d.Mkdir(at(ctx, 2020), "/P", false); err != nil {
		t.Fatal(err)
	}
	put := func(ctx context.Context, p string) error {
		_, err := d.Write(ctx, p, strings.NewReader(p), drive.Condition{}, drive.WriteOptions{})
		return err
	}
	for i, y := range []int{2021, 2021, 2021, 2023, 2023, 2023, 2023} {
		if err := put(at(ctx, y), "/P/f"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := put(at(ctx, 2022), "/P/late"); !errors.Is(err, drive.ErrOutOfOrder) {
		t.Fatalf("a 2022 file in a folder changed in 2023: %v; want ErrOutOfOrder", err)
	}
	if err := put(ctx, "/P/late"); err != nil {
		t.Fatal(err)
	}
	h, err := d.FolderHistory(ctx, drive.ByPath("/P"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.States) != 4 || !h.States[0].At.Equal(year(2020)) || len(h.States[0].Entries) != 0 ||
		!h.States[1].At.Equal(year(2021)) || len(h.States[1].Entries) != 3 || !h.States[2].At.Equal(year(2023)) || len(h.States[3].Entries) != 8 {
		t.Fatalf("history: %+v", h)
	}
	cur := h.States[3]
	var late drive.FolderEntry
	for _, e := range cur.Entries {
		if e.Name == "late" {
			late = e
		}
	}
	with := func(s drive.FolderState) drive.FolderState {
		s.Entries = append(append([]drive.FolderEntry(nil), s.Entries...), late)
		return s
	}
	fixed := []drive.FolderState{h.States[0], h.States[1], with(drive.FolderState{At: year(2022), Entries: h.States[1].Entries}), with(h.States[2])}

	// The last state must be the folder now, and the folder as read.
	if err := d.ReplaceFolderHistory(ctx, drive.ByPath("/P"), fixed[:3], drive.Condition{IfMatch: h.Version}); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("a last state that isn't now: %v; want ErrInvalid", err)
	}
	if err := d.ReplaceFolderHistory(ctx, drive.ByPath("/P"), fixed, drive.Condition{IfMatch: "stale"}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("a stale version: %v; want a precondition error", err)
	}
	if err := d.ReplaceFolderHistory(ctx, drive.ByPath("/P"), fixed, drive.Condition{IfMatch: h.Version}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		when time.Time
		want string
	}{
		{year(2020).AddDate(0, 6, 0), ""},
		{year(2021).AddDate(0, 6, 0), "fa,fb,fc"},
		{year(2022).AddDate(0, 6, 0), "fa,fb,fc,late"},
		{year(2023).AddDate(0, 6, 0), "fa,fb,fc,fd,fe,ff,fg,late"},
		{time.Time{}, "fa,fb,fc,fd,fe,ff,fg,late"},
	} {
		if got := names(t, d, "/P", tc.when); got != tc.want {
			t.Fatalf("as of %v: %q, want %q", tc.when, got, tc.want)
		}
	}
	again, err := d.FolderHistory(ctx, drive.ByPath("/P"))
	if err != nil || len(again.States) != 4 || !again.States[2].At.Equal(year(2022)) {
		t.Fatalf("history after: %+v, %v", again, err)
	}
	// In order again: the next change goes on top.
	if err := put(at(ctx, 2024), "/P/next"); err != nil {
		t.Fatal(err)
	}
	report, err := d.Filesystem().Check(context.Background())
	if err != nil || len(report.Problems) != 0 {
		t.Fatalf("fsck: %+v, %v", report.Problems, err)
	}
}
