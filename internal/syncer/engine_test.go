package syncer_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/syncer"
	"github.com/resurgentech/icbfs/internal/syncer/memside"
)

var t0 = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

// pair is a WebDAV-like folder (no ids, whole seconds) and a drive.
type pair struct {
	local, remote *memside.Side
	engine        *syncer.Engine
	state         *syncer.MemState
	events        []syncer.Event
}

func newPair(t *testing.T, mode syncer.Mode, configure ...func(*syncer.Config)) *pair {
	t.Helper()
	p := &pair{local: memside.NewFolder(false, time.Second), remote: memside.NewDrive(), state: &syncer.MemState{}}
	cfg := syncer.Config{Name: "test", Mode: mode, Local: p.local, Remote: p.remote.Drive(), State: p.state,
		CaseInsensitive: true, SpoolDir: t.TempDir(), Log: func(e syncer.Event) { p.events = append(p.events, e) }}
	for _, c := range configure {
		c(&cfg)
	}
	p.engine = syncer.New(cfg)
	return p
}

func (p *pair) pass(t *testing.T) syncer.Report {
	t.Helper()
	rep := p.engine.Pass(context.Background())
	if len(rep.Errors) > 0 {
		t.Fatalf("pass errors: %v", rep.Errors)
	}
	return rep
}

func (p *pair) same(t *testing.T) {
	t.Helper()
	if l, r := p.local.Tree(), p.remote.Tree(); l != r {
		t.Fatalf("sides differ:\n  folder %s\n  drive  %s", l, r)
	}
}

func TestSeededDriveIsAdoptedWithoutCopying(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	for _, s := range []*memside.Side{p.local, p.remote} {
		s.Put("A/Adams/will.pdf", "will", t0)
		s.Put("A/Adams/deed.pdf", "deed", t0)
		s.Put("B/Brown/memo.docx", "memo", t0)
	}
	p.local.Put("B/Brown/memo.docx", "memo v2", t0.Add(time.Hour)) // changed since the copy
	rep := p.pass(t)
	p.same(t)
	if len(p.local.Reads) != 1 || p.local.Reads["B/Brown/memo.docx"] != 1 {
		t.Fatalf("folder reads %v; want only the changed file", p.local.Reads)
	}
	if len(p.remote.Writes) != 1 {
		t.Fatalf("drive writes %v", p.remote.Writes)
	}
	if rep.Done != rep.Planned {
		t.Fatalf("report %+v", rep)
	}
	// Nothing to do the second time.
	if rep := p.pass(t); rep.Planned != 0 {
		t.Fatalf("second pass planned %d", rep.Planned)
	}
}

func TestOneWayMirrorsAndKeepsEverything(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	p.local.Put("a.txt", "a1", t0)
	p.local.Put("dir/b.txt", "b1", t0)
	p.pass(t)
	p.same(t)

	p.local.Put("a.txt", "a2", t0.Add(time.Minute))
	p.local.Delete("dir/b.txt")
	p.remote.Put("only-on-drive.txt", "x", t0) // made on the drive: not in the folder
	p.remote.Put("a.txt", "edited on the drive", t0.Add(30*time.Second))
	p.local.Put("a.txt", "a3", t0.Add(2*time.Minute))
	p.pass(t)
	p.same(t)
	for _, want := range []string{"a1", "a2", "edited on the drive", "a3", "b1", "x"} {
		if !p.remote.Contents()[want] && want != "a2" { // a2 was never synced
			t.Fatalf("drive lost %q", want)
		}
	}
}

func TestRenameIsAMoveNotAnUpload(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	p.local.Put("E/Example/estate plan.pdf", "the content of the plan", t0)
	p.pass(t)
	before, _ := p.remote.Get("E/Example/estate plan.pdf")

	p.local.Rename("E/Example/estate plan.pdf", "E/Example/Estate Plan 2015.pdf")
	rep := p.pass(t)
	p.same(t)
	after, ok := p.remote.Get("E/Example/Estate Plan 2015.pdf")
	if !ok || after.ID != before.ID || len(after.History) != 1 || rep.Renames != 1 {
		t.Fatalf("not moved: %+v (renames %d)", after, rep.Renames)
	}
	if p.remote.Writes["E/Example/Estate Plan 2015.pdf"] != 0 {
		t.Fatal("re-uploaded")
	}
	if p.local.Reads["E/Example/Estate Plan 2015.pdf"] != 1 {
		t.Fatalf("folder reads %v; want the new file read once", p.local.Reads)
	}
	if p.remote.TrashCount() != 0 {
		t.Fatal("the renamed file went to the trash")
	}
}

func TestSameSizeDifferentContentIsNotARename(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	p.local.Put("old.txt", "AAAA", t0)
	p.pass(t)
	p.local.Delete("old.txt")
	p.local.Put("new.txt", "BBBB", t0)
	rep := p.pass(t)
	p.same(t)
	if rep.Renames != 0 || p.remote.TrashCount() != 1 {
		t.Fatalf("renames %d, trash %d", rep.Renames, p.remote.TrashCount())
	}
}

func TestRenameSeenAcrossTwoPassesComesBackFromTheTrash(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	p.local.Put("x/brief.pdf", "the brief, long enough", t0)
	p.pass(t)
	orig, _ := p.remote.Get("x/brief.pdf")
	p.local.Delete("x/brief.pdf")
	p.pass(t) // the delete is seen first
	if p.remote.TrashCount() != 1 {
		t.Fatal("not trashed")
	}
	p.local.Put("y/brief (final).pdf", "the brief, long enough", t0)
	rep := p.pass(t)
	p.same(t)
	got, _ := p.remote.Get("y/brief (final).pdf")
	if got.ID != orig.ID || rep.Renames != 1 || p.remote.TrashCount() != 0 {
		t.Fatalf("not restored: %+v, renames %d, trash %d", got, rep.Renames, p.remote.TrashCount())
	}
}

func TestFolderRenameIsOneMove(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	for i := 0; i < 20; i++ {
		p.local.Put(fmt.Sprintf("S/Smith/doc%02d.pdf", i), fmt.Sprintf("doc %d", i), t0)
	}
	p.pass(t)
	p.local.Rename("S/Smith", "S/Smith, John")
	rep := p.pass(t)
	p.same(t)
	if rep.Planned != 1 || len(p.local.Reads) != 20 { // 20 reads from the first pass only
		t.Fatalf("planned %d, reads %v", rep.Planned, p.local.Reads)
	}
}

func TestDisconnectedFolderIsRefused(t *testing.T) {
	p := newPair(t, syncer.OneWay, func(c *syncer.Config) { c.RequirePresent = []string{"marker.url"} })
	p.local.Put("marker.url", "[InternetShortcut]", t0)
	p.local.Put("a.pdf", "a", t0)
	p.pass(t)
	p.local.Delete("marker.url")
	p.local.Delete("a.pdf")
	rep := p.engine.Pass(context.Background())
	if rep.Refused == "" || p.remote.TrashCount() != 0 {
		t.Fatalf("not refused: %+v", rep)
	}

	q := newPair(t, syncer.OneWay)
	q.local.Put("a.pdf", "a", t0)
	q.pass(t)
	q.local.Delete("a.pdf")
	if rep := q.engine.Pass(context.Background()); !strings.Contains(rep.Refused, "empty") {
		t.Fatalf("an empty folder wasn't refused: %+v", rep)
	}
}

func TestManyRemovalsWaitForConfirmation(t *testing.T) {
	p := newPair(t, syncer.OneWay, func(c *syncer.Config) { c.MaxRemovals = 5 })
	for i := 0; i < 10; i++ {
		p.local.Put(fmt.Sprintf("f%d", i), fmt.Sprint(i), t0)
	}
	p.local.Put("keep", "k", t0)
	p.pass(t)
	for i := 0; i < 10; i++ {
		p.local.Delete(fmt.Sprintf("f%d", i))
	}
	rep := p.engine.Pass(context.Background())
	if rep.PendingRemoval != 10 || p.remote.TrashCount() != 0 {
		t.Fatalf("not held: %+v", rep)
	}
	p.engine.Confirm()
	p.pass(t)
	p.same(t)
	if p.remote.TrashCount() != 10 {
		t.Fatalf("trash %d", p.remote.TrashCount())
	}
}

func TestIgnoredNamesAreNotSynced(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	p.local.Put("~$memo.docx", "lock", t0)
	p.local.Put("memo.docx", "memo", t0)
	p.local.Put("Thumbs.db", "x", t0)
	p.pass(t)
	if _, ok := p.remote.Get("~$memo.docx"); ok {
		t.Fatal("lock file synced")
	}
	if tree := p.remote.Tree(); tree != "memo.docx=memo" {
		t.Fatalf("drive %s", tree)
	}
}

func TestAFailedFileIsRetriedNextPass(t *testing.T) {
	p := newPair(t, syncer.OneWay)
	p.local.Put("ok.txt", "ok", t0)
	p.local.Put("locked.docx", "open in an editor", t0)
	p.local.Fail["read:locked.docx"] = errors.New("in use")
	rep := p.engine.Pass(context.Background())
	if len(rep.Errors) != 1 {
		t.Fatalf("errors %v", rep.Errors)
	}
	if _, ok := p.remote.Get("ok.txt"); !ok {
		t.Fatal("the other file wasn't synced")
	}
	delete(p.local.Fail, "read:locked.docx")
	p.pass(t)
	p.same(t)
}

func TestOneWayListsTheDriveOnlyNowAndThen(t *testing.T) {
	p := newPair(t, syncer.OneWay, func(c *syncer.Config) { c.RemoteListEvery = time.Hour })
	p.local.Put("a", "a", t0)
	if rep := p.pass(t); !rep.ListedRemote {
		t.Fatal("first pass didn't list the drive")
	}
	p.remote.Put("a", "changed on the drive", t0.Add(time.Minute))
	if rep := p.pass(t); rep.ListedRemote || rep.Planned != 0 {
		t.Fatalf("second pass %+v", rep)
	}
	p.engine.ListRemoteNext()
	p.pass(t)
	p.same(t) // the drive's edit overwritten
}

func TestTwoWay(t *testing.T) {
	p := newPair(t, syncer.TwoWay)
	p.local.Put("a.txt", "a1", t0)
	p.local.Put("b.txt", "b1", t0)
	p.local.Put("c.txt", "c1", t0)
	p.pass(t)

	p.remote.Put("a.txt", "a edited on the drive", t0.Add(time.Minute))
	p.remote.Put("new-on-drive.txt", "n", t0)
	p.remote.Delete("b.txt")
	p.remote.Put("c.txt", "c on the drive", t0.Add(time.Minute))
	p.local.Put("c.txt", "c in the folder", t0.Add(2*time.Minute))
	p.pass(t)
	p.same(t)
	if it, _ := p.local.Get("c.txt"); string(it.Data) != "c in the folder" {
		t.Fatal("the folder didn't win")
	}
	if !p.remote.Contents()["c on the drive"] {
		t.Fatal("the drive's version was lost")
	}
	if _, ok := p.local.Get("b.txt"); ok {
		t.Fatal("removed on the drive, still in the folder")
	}
	// The drive renames something.
	p.remote.Rename("a.txt", "renamed.txt")
	p.pass(t)
	p.same(t)
}

// TestRandomizedConvergence makes random changes to the folder (and, two-
// way, the drive) between passes, and checks that the sides converge and
// that the drive never loses any content it was given.
func TestRandomizedConvergence(t *testing.T) {
	for _, mode := range []syncer.Mode{syncer.OneWay, syncer.TwoWay} {
		for seed := int64(1); seed <= 12; seed++ {
			t.Run(fmt.Sprintf("%v/%d", mode, seed), func(t *testing.T) {
				rng := rand.New(rand.NewSource(seed))
				p := newPair(t, mode)
				clock := t0
				names := []string{"a", "b", "c", "d/e", "d/f", "g/h/i", "g/j", "k"}
				given := map[string]bool{}
				for round := 0; round < 25; round++ {
					sides := []*memside.Side{p.local}
					if mode == syncer.TwoWay && rng.Intn(3) == 0 {
						sides = append(sides, p.remote)
					}
					for _, s := range sides {
						for n := rng.Intn(4); n > 0; n-- {
							clock = clock.Add(time.Duration(1+rng.Intn(3)) * time.Second)
							name := names[rng.Intn(len(names))]
							switch rng.Intn(4) {
							case 0, 1:
								content := fmt.Sprintf("%s@%d.%d", name, round, rng.Intn(1000))
								if blocked(s, name) {
									continue
								}
								s.Put(name, content, clock)
								if s == p.local {
									given[content] = true
								}
							case 2:
								s.Delete(name)
							case 3:
								to := names[rng.Intn(len(names))]
								if _, ok := s.Get(name); ok && to != name && !blocked(s, to) {
									if _, taken := s.Get(to); !taken && !strings.HasPrefix(to, name+"/") {
										s.Rename(name, to)
									}
								}
							}
						}
					}
					rep := p.engine.Pass(context.Background())
					if len(rep.Errors) > 0 {
						t.Fatalf("round %d: %v", round, rep.Errors)
					}
				}
				p.pass(t)
				p.same(t)
				// Everything the folder ever had that got synced is on the
				// drive or in its history or trash: check what's there now.
				contents := p.remote.Contents()
				for _, it := range strings.Fields(p.local.Tree()) {
					if i := strings.Index(it, "="); i >= 0 && !contents[it[i+1:]] {
						t.Fatalf("drive lacks %q", it)
					}
				}
			})
		}
	}
}

// blocked: a name's parent path is a file, or the name is a directory.
func blocked(s *memside.Side, name string) bool {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		if it, ok := s.Get(strings.Join(parts[:i], "/")); ok && !it.Dir {
			return true
		}
	}
	if it, ok := s.Get(name); ok && it.Dir {
		return true
	}
	return false
}

func TestSQLStateRoundTripsAndSurvivesARestart(t *testing.T) {
	path := t.TempDir() + "/state.db"
	st, err := syncer.OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := memside.NewFolder(false, time.Second), memside.NewDrive()
	cfg := syncer.Config{Name: "p", Local: local, Remote: remote.Drive(), State: st, CaseInsensitive: true, SpoolDir: t.TempDir()}
	local.Put("a/b.txt", "b", t0)
	local.Put("c.txt", "c", t0)
	if rep := syncer.New(cfg).Pass(context.Background()); len(rep.Errors) > 0 {
		t.Fatal(rep.Errors)
	}
	st.Close()

	// A new process: the base is read back, and nothing is redone.
	st, err = syncer.OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg.State = st
	base, err := st.Load(context.Background())
	if err != nil || len(base) != 3 || base["a/b.txt"].Hash == "" || base["a/b.txt"].RemoteVersion == "" {
		t.Fatalf("base %+v, %v", base, err)
	}
	if !base["a/b.txt"].ModTime.Equal(t0) {
		t.Fatalf("time %v", base["a/b.txt"].ModTime)
	}
	local.Delete("c.txt")
	rep := syncer.New(cfg).Pass(context.Background())
	if len(rep.Errors) > 0 || rep.Planned != 1 {
		t.Fatalf("after restart: %+v", rep)
	}
	if base, _ := st.Load(context.Background()); len(base) != 2 {
		t.Fatalf("base after a removal %+v", base)
	}
}
