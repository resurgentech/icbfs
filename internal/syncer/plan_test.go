package syncer

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

func f(path string, size int64, mod time.Time) Entry {
	return Entry{Path: path, Size: size, ModTime: mod}
}
func d(path string) Entry { return Entry{Path: path, Dir: true} }

// rv is a drive entry with a version.
func rv(path string, size int64, mod time.Time, version string) Entry {
	return Entry{Path: path, Size: size, ModTime: mod, Version: version, ID: "id-" + path}
}

func baseOf(entries ...Entry) map[string]Base {
	m := map[string]Base{}
	for _, e := range entries {
		m[Key(e.Path, true)] = Base{Path: e.Path, Dir: e.Dir, Size: e.Size, ModTime: e.ModTime,
			LocalID: e.ID, RemoteID: "id-" + e.Path, RemoteVersion: e.Version}
	}
	return m
}

// actions renders a plan as "kind path" lines, in order.
func actions(p *Plan) string {
	var out []string
	for _, a := range p.Actions {
		s := a.Kind.String() + " " + a.Path
		if a.From != "" {
			s = a.Kind.String() + " " + a.From + "->" + a.Path
		}
		if len(a.Candidates) > 0 {
			s += " ?" + strings.Join(a.Candidates, ",")
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

func opts(mode Mode) PlanOptions {
	return PlanOptions{Mode: mode, CaseInsensitive: true, Resolution: time.Second}
}

func check(t *testing.T, p *Plan, want string) {
	t.Helper()
	if got := actions(p); got != want {
		t.Fatalf("plan:\n  got  %s\n  want %s", got, want)
	}
}

func TestFirstSyncAdoptsWhatTheSeedAlreadyHas(t *testing.T) {
	local := []Entry{d("A"), f("A/same.pdf", 10, t0), f("A/edited.pdf", 12, t0.Add(time.Hour)), f("A/new.pdf", 3, t0)}
	remote := []Entry{d("A"), rv("A/same.pdf", 10, t0, "v1"), rv("A/edited.pdf", 11, t0, "v1"), rv("A/old.pdf", 5, t0, "v1")}
	p := MakePlan(opts(OneWay), local, remote, nil)
	check(t, p, "adopt A; upload A/edited.pdf; upload A/new.pdf; adopt A/same.pdf; remove-remote A/old.pdf")
}

func TestTimesCompareAtTheCoarserResolution(t *testing.T) {
	// The folder keeps whole seconds; the drive has the same time to the
	// nanosecond.
	local := []Entry{f("a.pdf", 10, t0)}
	remote := []Entry{rv("a.pdf", 10, t0.Add(300*time.Millisecond), "v1")}
	check(t, MakePlan(opts(OneWay), local, remote, nil), "adopt a.pdf")
}

func TestOneWayFolderWins(t *testing.T) {
	base := baseOf(Entry{Path: "a", Size: 1, ModTime: t0, Version: "v1"}, Entry{Path: "b", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "c", Size: 1, ModTime: t0, Version: "v1"}, Entry{Path: "gone", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "lost", Size: 1, ModTime: t0, Version: "v1"})
	local := []Entry{f("a", 2, t0.Add(time.Minute)), f("b", 1, t0), f("c", 3, t0.Add(time.Minute)), f("lost", 1, t0)}
	remote := []Entry{rv("a", 1, t0, "v1"), rv("b", 9, t0.Add(time.Hour), "v2"), rv("c", 4, t0.Add(time.Hour), "v2"), rv("gone", 1, t0, "v1")}
	p := MakePlan(opts(OneWay), local, remote, base)
	// a: edited in the folder. b: edited on the drive, overwritten. c:
	// both, the folder wins. gone: deleted in the folder. lost: deleted
	// on the drive, put back.
	check(t, p, "upload a; upload b; upload c; upload lost; remove-remote gone")
	if p.RemoteRemovals != 1 {
		t.Fatalf("remote removals %d", p.RemoteRemovals)
	}
	for _, a := range p.Actions {
		if a.Path == "b" && !strings.Contains(a.Note, "overwritten") {
			t.Fatalf("b's note: %q", a.Note)
		}
	}
}

func TestUnlistedDriveIsTakenAsTheBaseLeftIt(t *testing.T) {
	base := baseOf(Entry{Path: "a", Size: 1, ModTime: t0, Version: "v1"})
	check(t, MakePlan(opts(OneWay), []Entry{f("a", 1, t0)}, nil, base), "")
	check(t, MakePlan(opts(OneWay), []Entry{f("a", 2, t0)}, nil, base), "upload a")
	check(t, MakePlan(opts(OneWay), nil, nil, base), "remove-remote a")
}

func TestRenameCandidatesBySize(t *testing.T) {
	base := baseOf(Entry{Path: "x/old.pdf", Size: 100, ModTime: t0, Version: "v1"},
		Entry{Path: "x/other.pdf", Size: 7, ModTime: t0, Version: "v1"}, Entry{Path: "x", Dir: true})
	local := []Entry{d("x"), f("x/new.pdf", 100, t0), f("x/unrelated.pdf", 55, t0)}
	p := MakePlan(opts(OneWay), local, nil, base)
	check(t, p, "upload x/new.pdf ?x/old.pdf; upload x/unrelated.pdf; remove-remote x/old.pdf; remove-remote x/other.pdf")
}

func TestEmptyFilesAreNeverRenameCandidates(t *testing.T) {
	base := baseOf(Entry{Path: "old", Size: 0, ModTime: t0, Version: "v1"})
	check(t, MakePlan(opts(OneWay), []Entry{f("new", 0, t0)}, nil, base), "upload new; remove-remote old")
}

func TestRenameByTheFoldersIDs(t *testing.T) {
	o := opts(OneWay)
	o.LocalIDs = true
	base := baseOf(Entry{Path: "old.pdf", Size: 5, ModTime: t0, Version: "v1", ID: "L1"})
	local := []Entry{{Path: "new.pdf", Size: 5, ModTime: t0, ID: "L1"}}
	check(t, MakePlan(o, local, nil, base), "move-remote old.pdf->new.pdf")
	// Renamed and edited: moved, then uploaded.
	local[0].Size = 6
	check(t, MakePlan(o, local, nil, base), "move-remote old.pdf->new.pdf; upload new.pdf")
}

func TestReplacedUnderTheSameNameIsAChange(t *testing.T) {
	o := opts(OneWay)
	o.LocalIDs = true
	base := baseOf(Entry{Path: "a.docx", Size: 5, ModTime: t0, Version: "v1", ID: "L1"})
	local := []Entry{{Path: "a.docx", Size: 5, ModTime: t0, ID: "L2"}}
	check(t, MakePlan(o, local, nil, base), "upload a.docx")
}

func TestDirectoryRenameByItsContents(t *testing.T) {
	base := baseOf(Entry{Path: "Smith", Dir: true}, Entry{Path: "Smith/a.pdf", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "Smith/sub", Dir: true}, Entry{Path: "Smith/sub/b.pdf", Size: 2, ModTime: t0, Version: "v1"})
	local := []Entry{d("Smith, John"), f("Smith, John/a.pdf", 1, t0), d("Smith, John/sub"), f("Smith, John/sub/b.pdf", 2, t0)}
	p := MakePlan(opts(OneWay), local, nil, base)
	check(t, p, "move-remote Smith->Smith, John")
	if _, ok := p.Base["smith, john/sub/b.pdf"]; !ok {
		t.Fatalf("base not moved: %v", p.Base)
	}
	// a.pdf changed, so the directory is not moved whole; its unchanged
	// subdirectory still is, into the new directory (Move makes parents).
	local[1].Size = 9
	check(t, MakePlan(opts(OneWay), local, nil, base),
		"move-remote Smith/sub->Smith, John/sub; mkdir-remote Smith, John; upload Smith, John/a.pdf; remove-remote Smith/a.pdf; remove-remote Smith")
}

func TestCaseOnlyRename(t *testing.T) {
	base := baseOf(Entry{Path: "report.PDF", Size: 1, ModTime: t0, Version: "v1"})
	check(t, MakePlan(opts(OneWay), []Entry{f("Report.pdf", 1, t0)}, nil, base), "move-remote report.PDF->Report.pdf")
}

func TestTwoWay(t *testing.T) {
	base := baseOf(Entry{Path: "edited-there", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "deleted-there", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "deleted-there-edited-here", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "deleted-here-edited-there", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "renamed-there", Size: 1, ModTime: t0, Version: "v1"},
		Entry{Path: "both", Size: 1, ModTime: t0, Version: "v1"})
	local := []Entry{f("edited-there", 1, t0), f("deleted-there", 1, t0), f("deleted-there-edited-here", 2, t0.Add(time.Hour)),
		f("renamed-there", 1, t0), f("both", 3, t0.Add(time.Hour))}
	remote := []Entry{rv("edited-there", 5, t0.Add(time.Hour), "v2"), rv("deleted-here-edited-there", 6, t0.Add(time.Hour), "v2"),
		{Path: "renamed-now", Size: 1, ModTime: t0, Version: "v1", ID: "id-renamed-there"}, rv("both", 4, t0.Add(time.Hour), "v2"),
		rv("new-there", 8, t0, "v1")}
	p := MakePlan(opts(TwoWay), local, remote, base)
	check(t, p, "move-local renamed-there->renamed-now; upload both; download deleted-here-edited-there; upload deleted-there-edited-here; download edited-there; download new-there; remove-local deleted-there")
}

func TestOrderingDirectoriesAndRemovals(t *testing.T) {
	base := baseOf(Entry{Path: "old", Dir: true}, Entry{Path: "old/deep", Dir: true}, Entry{Path: "old/deep/f", Size: 1, ModTime: t0, Version: "v1"})
	local := []Entry{d("new"), d("new/deeper"), f("new/deeper/g", 2, t0)}
	check(t, MakePlan(opts(OneWay), local, nil, base),
		"mkdir-remote new; mkdir-remote new/deeper; upload new/deeper/g; remove-remote old/deep/f; remove-remote old/deep; remove-remote old")
}

func TestACLChangeIsCopied(t *testing.T) {
	o := opts(OneWay)
	o.LocalACLs = true
	base := baseOf(Entry{Path: "a", Size: 1, ModTime: t0, Version: "v1"})
	b := base["a"]
	b.ACLHash = HashACL("D:(A;;FA;;;SY)")
	base["a"] = b
	l := f("a", 1, t0)
	l.ACL = "D:(A;;FA;;;SY)"
	check(t, MakePlan(o, []Entry{l}, nil, base), "")
	l.ACL = "D:(A;;FA;;;SY)(A;;FR;;;BU)"
	check(t, MakePlan(o, []Entry{l}, nil, base), "set-acl-remote a")
}

func TestGoneFromBothSidesIsForgotten(t *testing.T) {
	base := baseOf(Entry{Path: "a", Size: 1, ModTime: t0, Version: "v1"})
	check(t, MakePlan(opts(TwoWay), nil, []Entry{}, base), "forget a")
}
