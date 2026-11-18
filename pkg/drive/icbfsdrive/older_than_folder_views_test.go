package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// The proof that rule 1.6-8 (T83-8) changes no view: the same folders
// built twice from the same inputs, once with the rule off
// (icbfs.SetFirstStateRule(false): the code before it, on its own code
// path, every late row settled as the daemon's sweep does) and once with
// it on, and every view compared at every date any version carries, a
// second before and after each, and now.

// olderOp is one input: a write of a file, a folder made, or an item
// removed, at a source time.
type olderOp struct {
	op   string // "write", "mkdir", "delete"
	path string
	at   time.Time
}

func ymd(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

// olderBuild is one build of a case: its drive, and the rows its writes
// made.
type olderBuild struct {
	d    *Drive
	rows int
}

// buildOlder applies ops to a new deferring drive with rule 1.6-8 on or
// off, then settles every row by folder, as the daemon's sweep at the end
// of a pass does.
func buildOlder(t *testing.T, store objstore.Store, name string, ops []olderOp, rule bool) olderBuild {
	t.Helper()
	was := icbfs.SetFirstStateRule(rule)
	defer icbfs.SetFirstStateRule(was)
	d := newDriveWith(t, store, cutCaseSettings(name), Config{DeferLateFolders: true})
	ctx := system()
	var rows []drive.LateChange
	for _, o := range ops {
		late := &drive.LateChanges{}
		wctx := drive.WithSourceTime(drive.WithLateChanges(ctx, late), o.at)
		var err error
		switch o.op {
		case "write":
			_, err = d.Write(wctx, o.path, strings.NewReader(o.path+o.at.String()), drive.Condition{}, drive.WriteOptions{Parents: true})
		case "mkdir":
			_, err = d.Mkdir(wctx, o.path, true)
		case "delete":
			err = d.Delete(wctx, drive.ByPath(o.path), true, drive.Condition{})
		case "move":
			from, to, _ := strings.Cut(o.path, ">")
			_, err = d.Move(wctx, drive.ByPath(from), to, false)
		}
		if err != nil {
			t.Fatalf("%s %s at %s (rule %v): %v", o.op, o.path, o.at.Format(time.DateOnly), rule, err)
		}
		rows = append(rows, late.Take()...)
	}
	byFolder := map[string][]drive.LateChange{}
	var order []string
	for _, r := range rows {
		if byFolder[r.Folder] == nil {
			order = append(order, r.Folder)
		}
		byFolder[r.Folder] = append(byFolder[r.Folder], r)
	}
	for _, f := range order {
		p := pathOfKey(t, d, f)
		if _, err := d.SettleFolder(ctx, byFolder[f], drive.SettleOptions{Path: p}); err != nil {
			t.Fatalf("settle %s (rule %v): %v", p, rule, err)
		}
	}
	return olderBuild{d: d, rows: len(rows)}
}

// pathOfKey is the path of the folder whose key is key now, found by a
// walk (a test's: the folders are few).
func pathOfKey(t *testing.T, d *Drive, key string) string {
	t.Helper()
	if key == d.rootKey {
		return "/"
	}
	var found string
	var walk func(p string)
	walk = func(p string) {
		page, err := d.List(system(), drive.ByPath(p), drive.ListOptions{})
		if err != nil {
			t.Fatalf("list %s: %v", p, err)
		}
		for _, it := range page.Items {
			if it.Type != drive.TypeDir {
				continue
			}
			cp := strings.TrimSuffix(p, "/") + "/" + it.Name
			if it.ID == key {
				found = cp
			}
			walk(cp)
		}
	}
	walk("/")
	if found == "" {
		t.Fatalf("folder %s: not found", key)
	}
	return found
}

// olderPaths is every path the build holds now or the inputs name, and the
// folders among them, the root first.
func olderPaths(ops []olderOp) (all, folders []string) {
	seen := map[string]bool{"/": true}
	all, folders = []string{"/"}, []string{"/"}
	add := func(p string, dir bool) {
		if !seen[p] {
			seen[p] = true
			all = append(all, p)
			if dir {
				folders = append(folders, p)
			}
		}
	}
	for _, o := range ops {
		if from, to, ok := strings.Cut(o.path, ">"); ok {
			add(from, true)
			add(to, true)
			continue
		}
		parts := strings.Split(strings.TrimPrefix(o.path, "/"), "/")
		for i := 1; i < len(parts); i++ {
			add("/"+strings.Join(parts[:i], "/"), true)
		}
		add(o.path, o.op == "mkdir")
	}
	return all, folders
}

// olderDates is every date a version of the build's items carries.
func olderDates(t *testing.T, b olderBuild, paths []string) map[time.Time]bool {
	t.Helper()
	out := map[time.Time]bool{}
	for _, p := range paths {
		n, err := b.d.resolve(system(), b.d.fs, drive.ByPath(p))
		if err != nil {
			continue // removed
		}
		dates, err := objstore.ReadDates(context.Background(), b.d.cfg.Store, n.key)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range dates {
			out[at.UTC()] = true
		}
	}
	return out
}

// olderView is everything a view of the build as of when shows: each
// folder listed by its path and by its id (the id it has now), each path
// stated, names, types, sizes and times. now: the present.
func olderView(t *testing.T, b olderBuild, all, folders []string, ids map[string]string, when time.Time) string {
	t.Helper()
	ctx := system()
	at := drive.At{Time: when}
	var lines []string
	show := func(what string, err error, ok func() string) {
		switch {
		case err == nil:
			lines = append(lines, what+" "+ok())
		case errors.Is(err, drive.ErrNotFound):
			lines = append(lines, what+" absent")
		case errors.Is(err, drive.ErrNotDir):
			lines = append(lines, what+" not a folder")
		default:
			t.Fatalf("%s as of %v: %v", what, when, err)
		}
	}
	listing := func(page drive.Page) string {
		var out []string
		for _, it := range page.Items {
			out = append(out, fmt.Sprintf("%s:%s:%d:%s", it.Name, it.Type, it.Size, it.Modified.UTC().Format(time.RFC3339)))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	for _, p := range folders {
		page, err := b.d.List(ctx, drive.ByPath(p), drive.ListOptions{At: at})
		show("path "+p, err, func() string { return listing(page) })
		if id := ids[p]; id != "" {
			page, err := b.d.List(ctx, drive.ByID(id), drive.ListOptions{At: at})
			show("id of "+p, err, func() string { return listing(page) })
		}
	}
	for _, p := range all {
		it, err := b.d.Stat(ctx, drive.ByPath(p), at)
		show("stat "+p, err, func() string {
			return fmt.Sprintf("%s:%d:%s:%s", it.Type, it.Size, it.Modified.UTC().Format(time.RFC3339), it.Created.UTC().Format(time.RFC3339))
		})
		if id := ids[p]; id != "" {
			it, err := b.d.Stat(ctx, drive.ByID(id), at)
			show("stat id of "+p, err, func() string { return fmt.Sprintf("%s:%s:%d", it.Path, it.Type, it.Size) })
		}
	}
	return strings.Join(lines, "\n")
}

// olderNothing is olderView of a time before the root's first state:
// every folder, by path and by id, and every item, absent.
func olderNothing(all, folders []string, ids map[string]string) string {
	var lines []string
	for _, p := range folders {
		lines = append(lines, "path "+p+" absent")
		if ids[p] != "" {
			lines = append(lines, "id of "+p+" absent")
		}
	}
	for _, p := range all {
		lines = append(lines, "stat "+p+" absent")
		if ids[p] != "" {
			lines = append(lines, "stat id of "+p+" absent")
		}
	}
	return strings.Join(lines, "\n")
}

// olderIDs is the id each path has now, if it is there.
func olderIDs(b olderBuild, all []string) map[string]string {
	ids := map[string]string{}
	for _, p := range all {
		if it, err := b.d.Stat(system(), drive.ByPath(p), drive.At{}); err == nil {
			ids[p] = it.ID
		}
	}
	return ids
}

// olderCases are the shapes of T83-8's proof.
// olderCase is one shape: its inputs, the rows the rule makes (-1: some,
// a folder with later states: LF-37), and, for a case whose root is
// written before its first state, from when the views must be equal.
type olderCase struct {
	name     string
	ops      []olderOp
	wantRows int
	from     time.Time
}

func olderCases() []olderCase {
	var many, manyLater []olderOp
	for i := 0; i < 130; i++ {
		many = append(many, olderOp{"write", fmt.Sprintf("/F/f%03d.txt", i), ymd(2020, 1, 1)})
		manyLater = append(manyLater, olderOp{"write", fmt.Sprintf("/F/f%03d.txt", i), ymd(2020, 1, 1).AddDate(0, 0, i/26)}) // five states, the later ones splitting blocks
	}
	many = append(many, olderOp{"write", "/F/a-old.txt", ymd(2018, 1, 1)}, olderOp{"write", "/F/z-old.txt", ymd(2017, 1, 1)}, olderOp{"write", "/F/m-old.txt", ymd(2019, 1, 1)})
	manyLater = append(manyLater, olderOp{"write", "/F/a-old.txt", ymd(2018, 1, 1)}, olderOp{"write", "/F/z-old.txt", ymd(2020, 1, 2)}, olderOp{"write", "/F/m-old.txt", ymd(2019, 1, 1)})
	return []olderCase{
		{"a the smallest case", []olderOp{
			{"write", "/F/c.txt", ymd(2020, 1, 1)},
			{"write", "/F/d.txt", ymd(2018, 1, 1)},
		}, 0, time.Time{}},
		{"b a folder older than its parent holding items older than itself", []olderOp{
			{"write", "/P/p.txt", ymd(2020, 1, 1)},
			{"mkdir", "/P/G", ymd(2018, 1, 1)},
			{"write", "/P/G/g1.txt", ymd(2016, 6, 1)},
			{"write", "/P/G/g0.txt", ymd(2017, 1, 1)},
			{"write", "/P/G/g2.txt", ymd(2019, 1, 1)},
		}, 0, time.Time{}},
		{"c a top-level folder older than the root", []olderOp{
			{"mkdir", "/T", ymd(2013, 1, 1)},
			{"write", "/T/t.txt", ymd(2012, 1, 1)},
			{"write", "/U/u.txt", ymd(2016, 1, 1)},
			{"mkdir", "/V", ymd(2014, 1, 1)},
		}, -1, year(2015)},
		{"d an add and a remove both before the first state", []olderOp{
			{"write", "/F/a.txt", ymd(2020, 1, 1)},
			{"write", "/F/x.txt", ymd(2017, 1, 1)},
			{"delete", "/F/x.txt", ymd(2018, 1, 1)},
		}, 0, time.Time{}},
		{"e a folder with later states receives an item older than it", []olderOp{
			{"write", "/F/a.txt", ymd(2020, 1, 1)},
			{"write", "/F/b.txt", ymd(2022, 1, 1)},
			{"write", "/F/old.txt", ymd(2018, 1, 1)},
			{"write", "/F/mid.txt", ymd(2021, 1, 1)},
		}, 2, time.Time{}},
		{"g a folder moved, then an item older than it", []olderOp{
			{"write", "/A/F/f.txt", ymd(2020, 1, 1)},
			{"write", "/A/F/old.txt", ymd(2018, 1, 1)},
			{"mkdir", "/B", ymd(2019, 1, 1)},
			{"move", "/A/F>/B/F", ymd(2022, 1, 1)},
			{"write", "/B/F/older.txt", ymd(2017, 1, 1)},
			{"write", "/A/G.txt", ymd(2016, 1, 1)},
		}, -1, time.Time{}},
		{"f1 more than 128 entries, being built", many, 0, time.Time{}},
		{"f2 more than 128 entries, with later states", manyLater, -1, time.Time{}},
	}
}

// TestT838_NoViewDiffers (rule 1.6-8; LF-36's last sentence; ST-29's
// "the states from the first on are exact"): for each case, both builds'
// views are equal at every date any version of either carries, a second
// before and after it, and now; the build with the rule on makes no row
// unless the case has a folder with later states (e, f2: LF-37; c: the
// root, once /U is in it). One exception, case c: the root's first state
// is the filesystem's making (2015); with the rule off a top-level folder
// of 2013 had its settle write the root at 2013, so the views before 2015
// showed the root and /T before the filesystem existed. With the rule
// they show nothing there; from 2015 on the views are equal.
// STATES: LF-36
func TestT838_NoViewDiffers(t *testing.T) {
	if os.Getenv("ICBFS_PRE_MERGE") == "" {
		t.Skip("eight cases built twice and every view compared at every date (about 90 s without -race): ICBFS_PRE_MERGE=1, just pre-merge")
	}
	store := miniotest.Store(t)
	for i, c := range olderCases() {
		t.Run(c.name, func(t *testing.T) {
			before := buildOlder(t, store, fmt.Sprintf("older%doff", i), c.ops, false)
			after := buildOlder(t, store, fmt.Sprintf("older%don", i), c.ops, true)
			all, folders := olderPaths(c.ops)
			dates := olderDates(t, before, all)
			for at := range olderDates(t, after, all) {
				dates[at] = true
			}
			for _, o := range c.ops {
				dates[o.at] = true
			}
			var times []time.Time
			for at := range dates {
				times = append(times, at.Add(-time.Second), at, at.Add(time.Second))
			}
			sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
			times = append(times, time.Time{}) // now
			ib, ia := olderIDs(before, all), olderIDs(after, all)
			differ, early := 0, 0
			for _, at := range times {
				vb := olderView(t, before, all, folders, ib, at)
				va := olderView(t, after, all, folders, ia, at)
				if !at.IsZero() && at.Before(c.from) {
					// Before the root's first state: every view shows nothing.
					if want := olderNothing(all, folders, ia); va != want {
						t.Errorf("as of %v, before the root's first state, the views are:\n%s\nwant:\n%s", at, va, want)
					}
					if vb != va {
						early++
					}
					continue
				}
				if vb != va {
					differ++
					if differ <= 3 {
						t.Errorf("as of %v the views differ:\nrule off:\n%s\nrule on:\n%s", at, vb, va)
					}
				}
			}
			t.Logf("%d dates compared (%d views each), %d differ; %d before the root's first state, where the rule off showed the root before it existed; rows: rule off %d, rule on %d",
				len(times), 2*len(folders)+2*len(all), differ, early, before.rows, after.rows)
			switch {
			case c.wantRows < 0 && after.rows == 0:
				t.Errorf("a folder with later states: no row with the rule on; want its late rows (LF-37)")
			case c.wantRows >= 0 && after.rows != c.wantRows:
				t.Errorf("rule on: %d rows; want %d", after.rows, c.wantRows)
			}
		})
	}
}

// TestT838_RequestsOfAWrite: rule 1.6-8 adds no request to a write. The
// same writes counted with the rule off (the code before it) and on: an
// on-time write, a late write, and an LF-36 write (an item older than a
// folder being built) on a deferring drive; on a refusing drive an
// on-time write, an LF-39 write (with the rule off: refused, and its
// undo), and a folder's own record set from before its first state.
// On-time and late: equal. LF-36: not more than the same write with the
// rule off, which was late. LF-39: what an on-time write costs (with the
// rule off it was refused, and its undo made fewer). The folder's record:
// not more.
func TestT838_RequestsOfAWrite(t *testing.T) {
	type counts map[string]int
	run := func(rule bool) counts {
		was := icbfs.SetFirstStateRule(rule)
		defer icbfs.SetFirstStateRule(was)
		out := counts{}
		for _, deferring := range []bool{true, false} {
			store := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
			d := newDriveWith(t, store, cutCaseSettings(fmt.Sprintf("req838-%v-%v", rule, deferring)), Config{DeferLateFolders: deferring})
			ctx := system()
			writeAt(t, d, ctx, 2020, "/on/a.txt")
			writeAt(t, d, ctx, 2020, "/late/a.txt")
			writeAt(t, d, ctx, 2022, "/late/b.txt")
			writeAt(t, d, ctx, 2020, "/built/a.txt")
			writeAt(t, d, ctx, 2020, "/rec/a.txt")
			kind := "refusing "
			if deferring {
				kind = "deferring "
			}
			counted := func(what string, f func() error) {
				t.Helper()
				store.take()
				err := f()
				n, by := store.take()
				out[kind+what] = n
				t.Logf("rule %v, %s%s: %d requests (err %v)%s", rule, kind, what, n, err, by)
			}
			late := &drive.LateChanges{}
			wr := func(p string, y int) func() error {
				return func() error {
					_, err := d.Write(at(drive.WithLateChanges(ctx, late), y), p, strings.NewReader(p), drive.Condition{}, drive.WriteOptions{})
					return err
				}
			}
			counted("on time", wr("/on/x.txt", 2021))
			counted("late", wr("/late/x.txt", 2021))
			counted("older than its folder being built", wr("/built/x.txt", 2018))
			counted("a folder's record from before its first state", func() error {
				_, err := d.SetTimes(at(drive.WithLateChanges(ctx, late), 2018), drive.ByPath("/rec"), year(2018), time.Time{})
				return err
			})
		}
		return out
	}
	off, on := run(false), run(true)
	for k, n := range on {
		switch {
		case strings.HasSuffix(k, "on time") || strings.HasSuffix(k, "late"):
			if n != off[k] {
				t.Errorf("%s: %d requests with the rule, %d without", k, n, off[k])
			}
		case strings.HasSuffix(k, "being built"):
			// On time now: what an on-time write costs (on a refusing drive
			// the write without the rule was refused and undone: fewer).
			kind := strings.TrimSuffix(k, "older than its folder being built")
			if n > on[kind+"on time"] || n > off[k] && strings.HasPrefix(k, "deferring") {
				t.Errorf("%s: %d requests with the rule; an on-time write %d, the same write without it %d", k, n, on[kind+"on time"], off[k])
			}
		default:
			if n > off[k] {
				t.Errorf("%s: %d requests with the rule, more than %d without", k, n, off[k])
			}
		}
	}
}
