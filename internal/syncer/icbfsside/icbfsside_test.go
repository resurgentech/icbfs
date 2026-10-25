package icbfsside_test

import (
	"context"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/syncer"
	"github.com/resurgentech/icbfs/internal/syncer/icbfsside"
	"github.com/resurgentech/icbfs/internal/syncer/memside"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
)

var t0 = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

const rootSDDL = "O:SYG:SYD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

func open(t *testing.T) (*icbfsside.Side, *icbfs.Filesystem) {
	t.Helper()
	ctx := context.Background()
	store := miniotest.Store(t)
	if _, err := icbfs.CreateFilesystem(ctx, store, icbfs.Settings{Name: "mirror", Size: 1 << 30, PrimaryWindows: true, RootSDDL: rootSDDL}); err != nil {
		t.Fatal(err)
	}
	s, err := icbfsside.Open(ctx, store, "mirror", "", "test-sync", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	fs := icbfs.New(store, "mirror")
	if err := fs.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	return s, fs
}

func treeOf(t *testing.T, s *icbfsside.Side) string {
	t.Helper()
	es, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, e := range es {
		if e.Dir {
			rows = append(rows, e.Path+"/")
			continue
		}
		rc, err := s.Read(context.Background(), e.Path)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		rows = append(rows, e.Path+"="+string(data))
	}
	return sortJoin(rows)
}

func sortJoin(rows []string) string {
	sort.Strings(rows)
	return strings.Join(rows, " ")
}

// TestSyncToICBFS runs the engine from an in-memory WebDAV-like folder to
// a real icbfs drive: times kept, hashes stored, renames moved (keeping
// the item and its history), deletes trashed, a delayed rename restored
// from the trash, ACLs not touched (the folder has none).
func TestSyncToICBFS(t *testing.T) {
	ctx := context.Background()
	remote, fs := open(t)
	local := memside.NewFolder(false, time.Second)
	state := &syncer.MemState{}
	e := syncer.New(syncer.Config{Name: "t", Local: local, Remote: remote, State: state, CaseInsensitive: true, SpoolDir: t.TempDir()})
	pass := func() syncer.Report {
		t.Helper()
		rep := e.Pass(ctx)
		if len(rep.Errors) > 0 {
			t.Fatalf("pass: %v", rep.Errors)
		}
		return rep
	}

	local.Put("A/Apollo/plan.pdf", "the plan for Apollo", t0)
	local.Put("A/Apollo/budget.pdf", "a budget", t0.Add(time.Second))
	local.Put("B/memo.docx", "memo", t0.Add(2*time.Second))
	pass()
	if got, want := treeOf(t, remote), local.Tree(); got != want {
		t.Fatalf("drive %s\nfolder %s", got, want)
	}
	es, _ := remote.List(ctx)
	for _, x := range es {
		if x.Path == "A/Apollo/plan.pdf" && !x.ModTime.Equal(t0) {
			t.Fatalf("time not kept: %v", x.ModTime)
		}
	}
	if h, ok := remote.StoredHash(ctx, "A/Apollo/plan.pdf"); !ok || len(h) != 64 {
		t.Fatalf("no stored hash: %q", h)
	}
	if rep := pass(); rep.Planned != 0 {
		t.Fatalf("an unchanged second pass planned %d", rep.Planned)
	}

	// Rename: the same item, its history kept, no trash.
	before, _ := remote.Stat(ctx, "A/Apollo/plan.pdf")
	local.Rename("A/Apollo/plan.pdf", "A/Apollo/Final Plan.pdf")
	if rep := pass(); rep.Renames != 1 {
		t.Fatalf("renames %d", rep.Renames)
	}
	after, err := remote.Stat(ctx, "A/Apollo/Final Plan.pdf")
	if err != nil || after.ID != before.ID || after.Version != before.Version {
		t.Fatalf("rename lost the item: %+v vs %+v (%v)", after, before, err)
	}

	// A delete, seen alone, then the file again under another name: it
	// comes back from the trash.
	local.Delete("A/Apollo/budget.pdf")
	pass()
	trash, _ := remote.TrashedFiles(ctx, int64(len("a budget")))
	if len(trash) != 1 {
		t.Fatalf("trash %+v", trash)
	}
	deed, _ := fs.ListTrash(ctx, icbfs.TrashOptions{})
	local.Put("C/deed (copy).pdf", "a budget", t0.Add(time.Second))
	if rep := pass(); rep.Renames != 1 {
		t.Fatalf("not restored: renames %d", rep.Renames)
	}
	restored, err := remote.Stat(ctx, "C/deed (copy).pdf")
	if err != nil || restored.ID != deed[0].ItemID {
		t.Fatalf("restored %+v, want item %s (%v)", restored, deed[0].ItemID, err)
	}

	// An edit: a new version, the old kept.
	local.Put("B/memo.docx", "memo, revised", t0.Add(time.Hour))
	pass()
	if got, want := treeOf(t, remote), local.Tree(); got != want {
		t.Fatalf("drive %s\nfolder %s", got, want)
	}
	it, _ := remote.Stat(ctx, "B/memo.docx")
	versions := countVersions(t, fs, it.ID)
	if versions != 2 {
		t.Fatalf("memo has %d versions", versions)
	}
}

func countVersions(t *testing.T, fs *icbfs.Filesystem, id string) int {
	t.Helper()
	vs, err := fs.Store().History(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range vs {
		if !v.Deleted {
			n++
		}
	}
	return n
}

// TestACLsCarryFromAFolderThatHasThem: an NTFS-like folder's ACLs go onto
// the drive's files.
func TestACLsCarryFromAFolderThatHasThem(t *testing.T) {
	ctx := context.Background()
	remote, fs := open(t)
	local := memside.NewFolder(true, 100*time.Nanosecond).WithACLs()
	it := local.Put("secret.docx", "s", t0)
	it.ACL = "O:BAG:BAD:PAI(A;;FA;;;BA)"
	e := syncer.New(syncer.Config{Name: "t", Local: local, Remote: remote, State: &syncer.MemState{}, CaseInsensitive: true, SpoolDir: t.TempDir()})
	if rep := e.Pass(ctx); len(rep.Errors) > 0 {
		t.Fatal(rep.Errors)
	}
	st, err := remote.Stat(ctx, "secret.docx")
	if err != nil {
		t.Fatal(err)
	}
	attr, _ := fs.Stat(ctx, st.ID, icbfs.TypeFile)
	if got, _ := attr.WindowsACL(); string(got) != it.ACL {
		t.Fatalf("ACL %q", got)
	}
	// Changed later: copied again, without re-uploading.
	it.ACL = "O:BAG:BAD:PAI(A;;FA;;;BA)(A;;FR;;;BU)"
	if rep := e.Pass(ctx); len(rep.Errors) > 0 || rep.Planned != 1 {
		t.Fatalf("%+v", rep)
	}
	attr, _ = fs.Stat(ctx, st.ID, icbfs.TypeFile)
	if got, _ := attr.WindowsACL(); string(got) != it.ACL {
		t.Fatalf("ACL after change %q", got)
	}
	if n := countVersions(t, fs, st.ID); n != 1 {
		t.Fatalf("an ACL change re-uploaded: %d versions", n)
	}
}

// TestCreationTimesCarryToICBFS: a folder's creation times are the
// drive's, a file's and a directory's, as the mounts see them; a change
// to one alone is set with no new version.
func TestCreationTimesCarryToICBFS(t *testing.T) {
	ctx := context.Background()
	remote, fs := open(t)
	local := memside.NewFolder(true, 100*time.Nanosecond).WithCreated()
	born := time.Date(2019, 3, 4, 5, 6, 7, 800, time.UTC)
	it := local.Put("Reports/brief.docx", "b", t0)
	it.Created = born
	dir, _ := local.Get("Reports")
	dir.Created = born.Add(-time.Hour)
	e := syncer.New(syncer.Config{Name: "t", Local: local, Remote: remote, State: &syncer.MemState{}, CaseInsensitive: true, SpoolDir: t.TempDir()})
	if rep := e.Pass(ctx); len(rep.Errors) > 0 {
		t.Fatal(rep.Errors)
	}
	btime := func(p string, typ icbfs.EntryType) time.Time {
		t.Helper()
		st, err := remote.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		attr, err := fs.Stat(ctx, st.ID, typ)
		if err != nil {
			t.Fatal(err)
		}
		return attr.Btime
	}
	if got := btime("Reports/brief.docx", icbfs.TypeFile); !got.Equal(born) {
		t.Fatalf("file created %v, want %v", got, born)
	}
	if got := btime("Reports", icbfs.TypeDir); !got.Equal(born.Add(-time.Hour)) {
		t.Fatalf("folder created %v", got)
	}
	if rep := e.Pass(ctx); len(rep.Errors) > 0 || rep.Planned != 0 {
		t.Fatalf("second pass %+v", rep)
	}
	it.Created = born.AddDate(-1, 0, 0)
	if rep := e.Pass(ctx); len(rep.Errors) > 0 || rep.Planned != 1 {
		t.Fatalf("%+v", rep)
	}
	if got := btime("Reports/brief.docx", icbfs.TypeFile); !got.Equal(it.Created) {
		t.Fatalf("file created after a change %v", got)
	}
	st, _ := remote.Stat(ctx, "Reports/brief.docx")
	if n := countVersions(t, fs, st.ID); n != 1 {
		t.Fatalf("a creation time change re-uploaded: %d versions", n)
	}
}
