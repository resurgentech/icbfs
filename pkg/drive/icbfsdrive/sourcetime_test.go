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

// TestSourceTimeAnyOrder: a change dated before the item's current state
// goes into the past at its date and leaves the present as it was: an
// older version of a file written after a newer one, and a file written
// into a folder that had changed since. A source time in the future is
// refused.
func TestSourceTimeAnyOrder(t *testing.T) {
	d, _ := sourceDrive(t, "order")
	ctx := system()
	if _, err := d.Mkdir(at(ctx, 2020), "/dir", false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(at(ctx, 2022), "/dir/new.txt", strings.NewReader("new"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// An older version, late: the present keeps the newer one.
	if _, err := d.Write(at(ctx, 2021), "/dir/new.txt", strings.NewReader("older"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/new.txt"), drive.At{}); got != "new" {
		t.Fatalf("the newer version became %q", got)
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/new.txt"), drive.At{Time: year(2022).AddDate(0, 6, 0)}); got != "new" {
		t.Fatalf("mid-2022: %q, want the newer version", got)
	}
	// Two versions: the 2022 one, current again (put back on top, the
	// displaced copy gone), and the late 2021 one, with its checksums.
	versions, err := d.Versions(ctx, drive.ByPath("/dir/new.txt"))
	if err != nil || len(versions) != 2 || !versions[0].Time.Equal(year(2022)) || !versions[0].Current || !versions[1].Time.Equal(year(2021)) {
		t.Fatalf("versions: %+v, %v; want the 2022 one current, then the 2021 one", versions, err)
	}
	if versions[1].Checksums == nil || *versions[1].Checksums != sumsOf([]byte("older")) {
		t.Fatalf("the late version's checksums: %+v", versions[1].Checksums)
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/new.txt"), drive.At{Version: versions[1].ID}); got != "older" {
		t.Fatalf("the 2021 version: %q, want the older content", got)
	}
	// The file's name went into /dir in 2022: a view of 2021 doesn't reach
	// the late version by path (the mirror creates a file at its Created
	// time, so its name is there from the start).
	if exists(t, d, "/dir/new.txt", year(2021).AddDate(0, 6, 0)) {
		t.Fatal("mid-2021: the file is reachable by a name it didn't have yet")
	}
	it, err := d.Stat(ctx, drive.ByPath("/dir/new.txt"), drive.At{})
	if err != nil || !it.Modified.Equal(year(2022)) {
		t.Fatalf("now: modified %v, %v; want 2022", it.Modified, err)
	}

	// A file into a folder that changed since: there now, and from 2021.
	if _, err := d.Write(at(ctx, 2021), "/dir/old.txt", strings.NewReader("old"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		when time.Time
		want string
	}{
		{year(2020).AddDate(0, 6, 0), ""},
		{year(2021).AddDate(0, 6, 0), "old.txt"},
		{year(2022).AddDate(0, 6, 0), "new.txt,old.txt"},
		{time.Time{}, "new.txt,old.txt"},
	} {
		if got := names(t, d, "/dir", tc.when); got != tc.want {
			t.Fatalf("/dir as of %v: %q, want %q", tc.when, got, tc.want)
		}
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/old.txt"), drive.At{Time: year(2021).AddDate(0, 6, 0)}); got != "old" {
		t.Fatalf("the late file as of mid-2021: %q", got)
	}

	if _, err := d.Write(drive.WithSourceTime(ctx, time.Now().Add(time.Hour)), "/dir/later.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("a source time an hour ahead: %v; want ErrInvalid", err)
	}
	// An ordinary write on top of it all is now's.
	if _, err := d.Write(ctx, "/dir/old.txt", strings.NewReader("now"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, d, drive.ByPath("/dir/old.txt"), drive.At{}); got != "now" {
		t.Fatalf("after an undated write: %q", got)
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
	// A late file: the drive puts it into the folder's history at its
	// date, every later copy included, with no help.
	if err := put(at(ctx, 2022), "/P/late"); err != nil {
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
			t.Fatalf("after the late file, as of %v: %q, want %q", tc.when, got, tc.want)
		}
	}
	if got := read(t, ctx, d, drive.ByPath("/P/late"), drive.At{Time: year(2022).AddDate(0, 6, 0)}); got != "/P/late" {
		t.Fatalf("the late file as of mid-2022: %q", got)
	}
	h, err := d.FolderHistory(ctx, drive.ByPath("/P"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.States) != 4 || !h.States[0].At.Equal(year(2020)) || len(h.States[0].Entries) != 0 ||
		!h.States[1].At.Equal(year(2021)) || len(h.States[1].Entries) != 3 || !h.States[2].At.Equal(year(2022)) || len(h.States[2].Entries) != 4 ||
		!h.States[3].At.Equal(year(2023)) || len(h.States[3].Entries) != 8 {
		t.Fatalf("history: %+v", h)
	}

	// A correction: the source says "late" was really created in June
	// 2022, and "fa" was deleted in 2024 (so the present changes too).
	without := func(s drive.FolderState, name string) drive.FolderState {
		s.Entries = nil
		for _, e := range h.States[3].Entries {
			if e.Name != name {
				s.Entries = append(s.Entries, e)
			}
		}
		return s
	}
	fixed := []drive.FolderState{h.States[0], h.States[1], {At: year(2022).AddDate(0, 5, 0), Entries: h.States[2].Entries}, h.States[3],
		without(drive.FolderState{At: year(2024)}, "fa")}
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
		{year(2021).AddDate(0, 6, 0), "fa,fb,fc"},
		{year(2022).AddDate(0, 3, 0), "fa,fb,fc"},
		{year(2022).AddDate(0, 9, 0), "fa,fb,fc,late"},
		{year(2023).AddDate(0, 6, 0), "fa,fb,fc,fd,fe,ff,fg,late"},
		{time.Time{}, "fb,fc,fd,fe,ff,fg,late"},
	} {
		if got := names(t, d, "/P", tc.when); got != tc.want {
			t.Fatalf("after the correction, as of %v: %q, want %q", tc.when, got, tc.want)
		}
	}
	again, err := d.FolderHistory(ctx, drive.ByPath("/P"))
	if err != nil || len(again.States) != 5 || !again.States[2].At.Equal(year(2022).AddDate(0, 5, 0)) {
		t.Fatalf("history after: %+v, %v", again, err)
	}
	if st, err := d.Stat(ctx, drive.ByPath("/P"), drive.At{}); err != nil || !st.Modified.Equal(year(2024)) {
		t.Fatalf("the folder's mtime after: %v, %v; want 2024", st.Modified, err)
	}
	// In order again: the next change goes on top.
	if err := put(at(ctx, 2025), "/P/next"); err != nil {
		t.Fatal(err)
	}
	report, err := d.Filesystem().Check(context.Background())
	if err != nil || len(report.Problems) != 0 {
		t.Fatalf("fsck: %+v, %v", report.Problems, err)
	}
}

// TestReplaceFileHistory: a file's history, corrected: versions
// re-dated, one dropped, one added with new content, the record of each
// state kept; conditional on the file's version; the present changes as
// the last state says; nothing downloaded to keep a version.
func TestReplaceFileHistory(t *testing.T) {
	store := &contentReads{Store: miniotest.Store(t)}
	d := newDrive(t, store, icbfs.Settings{Name: "filefix", Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, "")
	ctx := system()
	var it drive.Item
	for _, v := range []struct {
		year int
		body string
	}{{2021, "one"}, {2022, "two"}, {2023, "three"}} {
		var err error
		if it, err = d.Write(at(ctx, v.year), "/f.txt", strings.NewReader(v.body), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := d.FileHistory(ctx, drive.ByPath("/f.txt"))
	if err != nil || len(h.Versions) != 3 || !h.Versions[0].Time.Equal(year(2023)) || !h.Versions[2].Time.Equal(year(2021)) || h.Version != h.Versions[0].ID {
		t.Fatalf("history: %+v, %v", h, err)
	}
	store.key.Store(it.ID)
	// The source says: "one" was really 2020, "two" never existed, and a
	// "four" was saved in 2024 (so the present changes).
	states := []drive.FileState{
		{At: year(2020), Version: h.Versions[2].ID},
		{At: year(2023), Version: h.Versions[0].ID, Info: drive.ItemInfo{App: map[string][]byte{"mirror.version": []byte("3")}}},
		{At: year(2024), Content: strings.NewReader("four"), Info: drive.ItemInfo{App: map[string][]byte{"mirror.version": []byte("4")}}},
	}
	if err := d.ReplaceFileHistory(ctx, drive.ByPath("/f.txt"), states, drive.Condition{IfMatch: "stale"}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("a stale version: %v; want a precondition error", err)
	}
	reads := store.n.Load()
	if err := d.ReplaceFileHistory(ctx, drive.ByPath("/f.txt"), states, drive.Condition{IfMatch: h.Version}); err != nil {
		t.Fatal(err)
	}
	if n := store.n.Load() - reads; n != 0 {
		t.Fatalf("keeping versions read their content %d times; want 0", n)
	}
	after, err := d.FileHistory(ctx, drive.ByPath("/f.txt"))
	if err != nil || len(after.Versions) != 3 || !after.Versions[0].Time.Equal(year(2024)) || !after.Versions[1].Time.Equal(year(2023)) || !after.Versions[2].Time.Equal(year(2020)) {
		t.Fatalf("history after: %+v, %v", after, err)
	}
	// By path from 2021, when its name went into the folder (the folder's
	// history is its own); the 2020 version by id.
	if got := read(t, ctx, d, drive.ByPath("/f.txt"), drive.At{Version: after.Versions[2].ID}); got != "one" {
		t.Fatalf("the 2020 version: %q", got)
	}
	if c := after.Versions[2].Checksums; c == nil || *c != sumsOf([]byte("one")) {
		t.Fatalf("the kept version's checksums: %+v", c)
	}
	for _, tc := range []struct {
		when time.Time
		want string
	}{
		{year(2019), ""},
		{year(2021).AddDate(0, 6, 0), "one"},
		{year(2022).AddDate(0, 6, 0), "one"},
		{year(2023).AddDate(0, 6, 0), "three"},
		{time.Time{}, "four"},
	} {
		if tc.want == "" {
			if exists(t, d, "/f.txt", tc.when) {
				t.Fatalf("as of %v: the file is there", tc.when)
			}
			continue
		}
		if got := read(t, ctx, d, drive.ByPath("/f.txt"), drive.At{Time: tc.when}); got != tc.want {
			t.Fatalf("as of %v: %q, want %q", tc.when, got, tc.want)
		}
	}
	now, err := d.Stat(ctx, drive.ByPath("/f.txt"), drive.At{})
	if err != nil || string(now.App["mirror.version"]) != "4" || !now.Modified.Equal(year(2024)) {
		t.Fatalf("now: app %q, modified %v, %v", now.App, now.Modified, err)
	}
	then, err := d.Stat(ctx, drive.ByPath("/f.txt"), drive.At{Time: year(2023).AddDate(0, 6, 0)})
	if err != nil || string(then.App["mirror.version"]) != "3" || !then.Modified.Equal(year(2023)) {
		t.Fatalf("mid-2023: app %q, modified %v, %v", then.App, then.Modified, err)
	}
	report, err := d.Filesystem().Check(context.Background())
	if err != nil || len(report.Problems) != 0 {
		t.Fatalf("fsck: %+v, %v", report.Problems, err)
	}
}
