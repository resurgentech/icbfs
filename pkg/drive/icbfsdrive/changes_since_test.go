package icbfsdrive

import (
	"context"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// sinceSource is the mirror's source drive in TestChangesSinceSyncPoint.
const sinceSource = "clio:test"

// sinceItem is a source item at one version, with its times as the
// source would give them (zero: not given).
func sinceItem(id, version, p, body string, mod time.Time) drive.SourceItem {
	return drive.SourceItem{
		Origin: drive.Origin{ID: id, Version: version, Path: "/src" + p},
		Path:   p, Type: drive.TypeFile, ModTime: mod, Created: mod, ContentType: "text/plain",
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(body)), nil
		},
	}
}

func sinceDir(id, p string, created time.Time) drive.SourceItem {
	return drive.SourceItem{Origin: drive.Origin{ID: id, Path: "/src" + p}, Path: p, Type: drive.TypeDir, Created: created}
}

// sinceFields are the stored times a walk of the mirror and a listing of
// its trash give, by name: each one compared with the sync point alone.
var sinceFields = []struct {
	name string
	of   func(drive.Item) time.Time
}{
	{"Modified", func(it drive.Item) time.Time { return it.Modified }},
	{"Changed", func(it drive.Item) time.Time { return it.Changed }},
	{"Created", func(it drive.Item) time.Time { return it.Created }},
	{"Version.Time", func(it drive.Item) time.Time {
		if it.Version == nil {
			return time.Time{}
		}
		return it.Version.Time
	}},
	{"Version.Stored", func(it drive.Item) time.Time {
		if it.Version == nil {
			return time.Time{}
		}
		return it.Version.Stored
	}},
	{"Origin.Copied", func(it drive.Item) time.Time {
		if it.Origin == nil {
			return time.Time{}
		}
		return it.Origin.Copied
	}},
}

// sinceSeen is what one walk and one trash listing say changed after the
// point: by source id, the fields that show it.
type sinceSeen struct {
	files, folders map[string][]string
}

func (s sinceSeen) ids(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// changedSince walks the whole drive and lists its trash, and keeps each
// item whose stored times (any of sinceFields), or trash entry whose
// DeletedAt or Stored, is after point. Nothing but the drive is consulted: no
// table kept from an earlier pass.
func changedSince(t *testing.T, ctx context.Context, d drive.Drive, point time.Time) sinceSeen {
	t.Helper()
	seen := sinceSeen{files: map[string][]string{}, folders: map[string][]string{}}
	err := drive.Walk(ctx, d, drive.ByPath("/"), drive.WalkOptions{}, func(_ drive.ItemRef, items []drive.Item) error {
		for _, it := range items {
			id := it.Path
			if it.Origin != nil {
				id = it.Origin.ID
			}
			into := seen.files
			if it.Type == drive.TypeDir {
				into = seen.folders
			}
			for _, f := range sinceFields {
				if v := f.of(it); v.After(point) {
					into[id] = append(into[id], f.name)
				}
			}
			t.Logf("  %-4s %-8s %-22s mod %s chg %s crt %s ver %s stored %s copied %s", it.Type, id, it.Path,
				stamp(it.Modified), stamp(it.Changed), stamp(it.Created), stamp(sinceFields[3].of(it)),
				stamp(sinceFields[4].of(it)), stamp(sinceFields[5].of(it)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for cursor := ""; ; {
		page, err := d.Trash(ctx, drive.TrashOptions{Cursor: cursor})
		if err != nil {
			t.Fatalf("trash: %v", err)
		}
		for _, e := range page.Entries {
			id := e.Path
			if e.Origin != nil {
				id = e.Origin.ID
			}
			t.Logf("  trash %-8s %-22s deleted %s stored %s", id, e.Path, stamp(e.DeletedAt), stamp(e.Stored))
			into := seen.files
			if e.Type == drive.TypeDir {
				into = seen.folders
			}
			if e.DeletedAt.After(point) {
				into[id] = append(into[id], "Trash.DeletedAt")
			}
			if e.Stored.After(point) {
				into[id] = append(into[id], "Trash.Stored")
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	return seen
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02T15:04:05.000")
}

// TestChangesSinceSyncPoint: whether the changes a Mirror made after a
// sync point can be read from the mirror alone. After the point, the
// mirror writes a new version of f1, moves f2 to another folder, renames
// f3 in its folder, removes f4 (to the trash), and restores f5 (removed
// before the point). A walk of the whole drive and a listing of its
// trash, comparing each item's stored times and each trash entry's with
// the point, must list exactly those five files and none of the
// untouched ones. The folders whose entries changed (a and b: names
// came and went in them) show too, by their own times; the untouched
// folders (c, c/d) must not.
//
// The point is the store's clock (StoreTime, whole seconds), taken a
// second after the last setup write and a second before the first
// change, so the store's one-second Last-Modified and this machine's
// clock (a ctime) both fall on the right side of it.
//
// Three ways the mirror dates its writes:
//   - store time: a plain Mirror, every time the moment of the write.
//   - source time, dated now: InSourceTime, each change dated by the
//     source at the moment it is made (after the point).
//   - source time, dated before the point: InSourceTime, each change
//     dated by the source in 2021, before the point (a change the
//     source made before the last pass but that pass didn't see: a
//     listing that came back late, a source clock behind the store's).
//
// T80-1 found the answer yes in the first two, and no in the third, for
// one change: a removal was dated only by its trash entry's DeletedAt,
// which in source time is the source's time. A trash entry now says when
// it was stored (TrashEntry.Stored, the store's time of its record), so
// f4 shows by it in all three, and its DeletedAt in the third is still
// the source's.
func TestChangesSinceSyncPoint(t *testing.T) {
	store := miniotest.Store(t)
	for _, mode := range []struct {
		name       string
		sourceTime bool
		late       bool
	}{
		{"store time", false, false},
		{"source time, dated now", true, false},
		{"source time, dated before the point", true, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			name := strings.NewReplacer(" ", "-", ",", "").Replace(mode.name)
			d := newDrive(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2015)}, "")
			ctx := system()
			m := drive.NewMirror(d, sinceSource)
			if mode.sourceTime {
				m = m.InSourceTime()
			}
			born := year(2020)
			if !mode.sourceTime {
				born = time.Time{}
			}
			apply := func(what string, src drive.SourceItem, want ...drive.Action) {
				t.Helper()
				src.Origin.Drive = sinceSource
				a, err := m.Apply(ctx, src)
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
			}

			// The tree: three folders and one below, five files to change
			// and four to leave alone.
			for _, dir := range []drive.SourceItem{
				sinceDir("a", "/a", born), sinceDir("b", "/b", born), sinceDir("c", "/c", born), sinceDir("cd", "/c/d", born),
			} {
				apply("make "+dir.Path, dir, drive.Created)
			}
			files := map[string]drive.SourceItem{
				"f1": sinceItem("f1", "1", "/a/f1.txt", "f1 one", born),
				"f2": sinceItem("f2", "1", "/a/f2.txt", "f2 one", born),
				"f3": sinceItem("f3", "1", "/a/f3.txt", "f3 one", born),
				"f4": sinceItem("f4", "1", "/b/f4.txt", "f4 one", born),
				"f5": sinceItem("f5", "1", "/b/f5.txt", "f5 one", born),
				"u1": sinceItem("u1", "1", "/a/u1.txt", "u1 one", born),
				"u2": sinceItem("u2", "1", "/b/u2.txt", "u2 one", born),
				"u3": sinceItem("u3", "1", "/c/u3.txt", "u3 one", born),
				"u4": sinceItem("u4", "1", "/c/d/u4.txt", "u4 one", born),
			}
			for _, id := range []string{"f1", "f2", "f3", "f4", "f5", "u1", "u2", "u3", "u4"} {
				apply("copy "+id, files[id], drive.Created)
			}
			// f5 goes before the point; it comes back after.
			removed := ctx
			if mode.sourceTime {
				removed = drive.WithSourceTime(ctx, year(2020).Add(time.Hour))
			}
			if ok, err := m.Remove(removed, "f5"); err != nil || !ok {
				t.Fatalf("remove f5 before the point: %v, %v", ok, err)
			}

			// The point.
			time.Sleep(1100 * time.Millisecond)
			point, err := d.StoreTime(ctx)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond)
			t.Logf("sync point %s (store time)", stamp(point))

			// When the source says each change happened.
			when := func() time.Time {
				switch {
				case !mode.sourceTime:
					return time.Time{}
				case mode.late:
					return year(2021)
				}
				return time.Now().UTC()
			}

			// A new version of f1.
			f1 := sinceItem("f1", "2", "/a/f1.txt", "f1 two", when())
			f1.Created = born
			apply("new version of f1", f1, drive.Updated)
			// f2 moved to b.
			f2 := files["f2"]
			f2.Path, f2.Origin.Path, f2.Changed = "/b/f2.txt", "/src/b/f2.txt", when()
			apply("move f2", f2, drive.Moved)
			// f3 renamed in a.
			f3 := files["f3"]
			f3.Path, f3.Origin.Path, f3.Changed = "/a/f3-renamed.txt", "/src/a/f3-renamed.txt", when()
			apply("rename f3", f3, drive.Moved)
			// f4 removed.
			rctx := ctx
			if mode.sourceTime {
				rctx = drive.WithSourceTime(ctx, when())
			}
			if ok, err := m.Remove(rctx, "f4"); err != nil || !ok {
				t.Fatalf("remove f4: %v, %v", ok, err)
			}
			// f5 restored.
			f5 := files["f5"]
			f5.Changed = when()
			apply("restore f5", f5, drive.Restored)

			seen := changedSince(t, ctx, d, point)
			for _, id := range seen.ids(seen.files) {
				t.Logf("file %s shows by %v", id, seen.files[id])
			}
			for _, id := range seen.ids(seen.folders) {
				t.Logf("folder %s shows by %v", id, seen.folders[id])
			}
			if got, want := strings.Join(seen.ids(seen.files), ","), "f1,f2,f3,f4,f5"; got != want {
				t.Errorf("files changed since the point, by stored times: %s; want exactly %s", got, want)
			}
			// f4's removal shows by when its trash entry was stored,
			// whatever the entry's DeletedAt says; the same through a
			// lookup by origin as through the listing.
			if !slices.Contains(seen.files["f4"], "Trash.Stored") {
				t.Errorf("f4, removed after the point, doesn't show by its trash entry's Stored: %v", seen.files["f4"])
			}
			page, err := d.Trash(ctx, drive.TrashOptions{OriginDrive: sinceSource, OriginID: "f4"})
			if err != nil || len(page.Entries) != 1 {
				t.Fatalf("the trash entry of f4: %+v, %v", page.Entries, err)
			}
			if mode.late {
				if e := page.Entries[0]; !e.DeletedAt.Equal(year(2021)) {
					t.Errorf("f4's trash entry is dated %s; want the source's time, %s", stamp(e.DeletedAt), stamp(year(2021)))
				}
			}
			_, found, err := d.FindByOrigin(ctx, sinceSource, "f4")
			if err != nil || found == nil {
				t.Fatalf("f4 by its origin: %+v, %v", found, err)
			}
			if !found.Stored.Equal(page.Entries[0].Stored) || !found.Stored.After(point) {
				t.Errorf("f4's trash entry by its origin is stored %s, in the listing %s; want the same, after the point %s",
					stamp(found.Stored), stamp(page.Entries[0].Stored), stamp(point))
			}
			if !slices.Contains(seen.folders["b"], "Changed") {
				t.Errorf("folder b, which f4 left, doesn't show by its Changed: %v", seen.folders["b"])
			}
			if got, want := strings.Join(seen.ids(seen.folders), ","), "a,b"; got != want {
				t.Errorf("folders changed since the point, by stored times: %s; want exactly %s (the folders names came and went in)", got, want)
			}
		})
	}
}
