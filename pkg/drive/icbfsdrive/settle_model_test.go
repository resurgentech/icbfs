package icbfsdrive

// A model of a folder written in a source's time, on time and late, and
// settled when the caller says: every case runs a list of steps through a
// deferring drive with LateChanges, settles each folder with the changes
// queued for it at each "settle" step, and checks two invariants
// throughout (round 4 of T83-1, after the design review of 2026-11-17):
//
//   - every view of the past, before and after a settle, in a folder of
//     one block or of many, is a state the folder really had (no mixed
//     tree: no name twice, nothing from a later date shown earlier and
//     nothing missing that the folder had);
//   - no silent repair: after a settle the folder's past is exactly the
//     model's (every change at its own date and no other), its present
//     too, and a settle never fails; a settle that cannot do that must
//     refuse, and here none may.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// mStep is one step of a case: an operation dated at (undated if zero),
// or a settle of every folder's queued changes.
type mStep struct {
	at    time.Time
	op    string // w, rm, mv, mkdir, rmdir, restore, settle
	p, to string
}

func (s mStep) String() string {
	when := "undated"
	if !s.at.IsZero() {
		when = s.at.Format("2006-01-02T15")
	}
	if s.op == "mv" {
		return fmt.Sprintf("%s %s %s -> %s", s.op, when, s.p, s.to)
	}
	return fmt.Sprintf("%s %s %s", s.op, when, s.p)
}

// day is a date: y, plus days.
func day(y, days int) time.Time { return year(y).AddDate(0, 0, days) }

func mw(at time.Time, p string) mStep       { return mStep{at: at, op: "w", p: p} }
func mrm(at time.Time, p string) mStep      { return mStep{at: at, op: "rm", p: p} }
func mmv(at time.Time, p, to string) mStep  { return mStep{at: at, op: "mv", p: p, to: to} }
func mmkdir(at time.Time, p string) mStep   { return mStep{at: at, op: "mkdir", p: p} }
func mrmdir(at time.Time, p string) mStep   { return mStep{at: at, op: "rmdir", p: p} }
func mrestore(at time.Time, p string) mStep { return mStep{at: at, op: "restore", p: p} }
func msettle() mStep                        { return mStep{op: "settle"} }
func mfiles(dir string, ys ...int) []mStep {
	var out []mStep
	for i, y := range ys {
		out = append(out, mw(year(y), fmt.Sprintf("%s/f%d.txt", dir, i)))
	}
	return out
}

// modelAt is dir's names as of when (the present if zero), by the steps'
// dates, steps of one date in their order: the folder's true history.
func modelAt(steps []mStep, dir string, when time.Time) string {
	var evs []mStep
	for _, s := range steps {
		if s.op != "settle" && (when.IsZero() || !s.at.After(when)) {
			evs = append(evs, s)
		}
	}
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].at.Before(evs[b].at) })
	set := map[string]bool{}
	for _, s := range evs {
		switch s.op {
		case "w", "mkdir", "restore":
			if path.Dir(s.p) == dir {
				set[path.Base(s.p)] = true
			}
		case "rm", "rmdir":
			if path.Dir(s.p) == dir {
				delete(set, path.Base(s.p))
			}
		case "mv":
			if path.Dir(s.p) == dir {
				delete(set, path.Base(s.p))
			}
			if path.Dir(s.to) == dir {
				set[path.Base(s.to)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// mCase is a case run: its drive, the steps applied, what is queued.
type mCase struct {
	t       *testing.T
	d       *Drive
	store   objstore.Store
	dirs    []string
	steps   []mStep // applied so far
	queue   map[string][]drive.LateChange
	order   []string
	keys    map[string]*keeper
	ids     map[string]string // a path's item, for a restore
	settles int
	// sample, if set, is how many of the steps' dates the view checks
	// look at (the latest, and an even spread of the rest).
	sample int
}

// newModelCase is a case on a fresh store: a filesystem from 2010 of
// entries per block (0: the default), a drive that defers late folders.
func newModelCase(t *testing.T, store objstore.Store, name string, entries uint32, dirs ...string) *mCase {
	t.Helper()
	if len(dirs) == 0 {
		dirs = []string{"/dir"}
	}
	d := newDriveWith(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries},
		Config{DeferLateFolders: true})
	return &mCase{t: t, d: d, store: store, dirs: dirs, queue: map[string][]drive.LateChange{}, keys: map[string]*keeper{}, ids: map[string]string{}}
}

// run applies steps, checking the invariants: before and after every
// settle, every view is a state the folder really had; after it, the
// folder's past is exact; after every step, its present is.
func (c *mCase) run(steps ...mStep) {
	c.t.Helper()
	for _, s := range steps {
		if s.op == "settle" {
			c.checkReal("before settle " + fmt.Sprint(c.settles+1))
			c.settle()
			c.checkReal("after settle " + fmt.Sprint(c.settles))
			c.checkExact("after settle " + fmt.Sprint(c.settles))
			continue
		}
		c.apply(s)
		for _, dir := range c.dirs {
			if got, want := c.namesAt(dir, time.Time{}), modelAt(c.steps, dir, time.Time{}); got != want {
				c.t.Fatalf("after %v: %s now %q, want %q", s, dir, got, want)
			}
		}
	}
}

// finish settles what is left, and checks the folder whole: views, past,
// present, the filesystem.
func (c *mCase) finish() {
	c.t.Helper()
	c.run(msettle())
	rep, err := c.d.fs.Check(context.Background())
	if err != nil {
		c.t.Fatal(err)
	}
	for _, p := range rep.Problems {
		c.t.Errorf("fs check: %+v", p)
	}
}

func (c *mCase) apply(s mStep) {
	c.t.Helper()
	late := &drive.LateChanges{}
	ctx := drive.WithLateChanges(system(), late)
	if !s.at.IsZero() {
		ctx = drive.WithSourceTime(ctx, s.at)
	}
	var it drive.Item
	var err error
	switch s.op {
	case "w":
		it, err = c.d.Write(ctx, s.p, strings.NewReader(s.p), drive.Condition{}, drive.WriteOptions{Parents: true})
		c.ids[s.p] = it.ID
	case "mkdir":
		it, err = c.d.Mkdir(ctx, s.p, true)
		c.ids[s.p] = it.ID
	case "rm", "rmdir":
		err = c.d.Delete(ctx, drive.ByPath(s.p), s.op == "rmdir", drive.Condition{})
	case "mv":
		it, err = c.d.Move(ctx, drive.ByPath(s.p), s.to, false)
		c.ids[s.to] = it.ID
	case "restore":
		page, terr := c.d.Trash(system(), drive.TrashOptions{})
		if terr != nil {
			c.t.Fatal(terr)
		}
		entry := ""
		for _, e := range page.Entries {
			if e.ItemID == c.ids[s.p] {
				entry = e.ID
			}
		}
		_, err = c.d.Restore(ctx, entry, drive.RestoreOptions{})
	default:
		c.t.Fatalf("step %v: no such op", s)
	}
	if err != nil {
		c.t.Fatalf("step %d, %v: %v", len(c.steps), s, err)
	}
	c.steps = append(c.steps, s)
	for _, ch := range late.Take() {
		if _, ok := c.queue[ch.Folder]; !ok {
			c.order = append(c.order, ch.Folder)
		}
		c.queue[ch.Folder] = append(c.queue[ch.Folder], ch)
	}
}

// settle settles every folder's queued changes, as a daemon does at the
// end of a pass: all of a folder's changes in one settle, with its
// record kept from Record to Done.
func (c *mCase) settle() {
	c.t.Helper()
	c.settles++
	for _, folder := range c.order {
		k := c.keys[folder]
		if k == nil {
			k = &keeper{}
			c.keys[folder] = k
		}
		if _, err := settleAt(c.d, system(), c.queue[folder], k.opts()); err != nil {
			c.t.Fatalf("settle %d of folder %s (%d changes): %v", c.settles, folder, len(c.queue[folder]), err)
		}
		if k.held() != nil {
			c.t.Fatalf("settle %d of folder %s: the record is held after it", c.settles, folder)
		}
	}
	c.queue, c.order = map[string][]drive.LateChange{}, nil
}

// namesAt is dir's names as of when (now if zero), sorted; "" if it
// didn't exist then.
func (c *mCase) namesAt(dir string, when time.Time) string {
	c.t.Helper()
	ctx := system()
	n, err := c.d.resolve(ctx, c.d.fs, drive.ByPath(dir))
	if err != nil {
		return "" // not there now (never made, in this case)
	}
	fsys := c.d.fs
	if !when.IsZero() {
		if fsys, err = c.d.at(ctx, drive.At{Time: when}); err != nil {
			c.t.Fatal(err)
		}
	}
	entries, err := fsys.ReadDir(ctx, n.key)
	if err != nil {
		if when.IsZero() {
			c.t.Fatalf("%s now: %v", dir, err)
		}
		return "" // the folder didn't exist then
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	for i := 1; i < len(out); i++ {
		if out[i] == out[i-1] {
			return strings.Join(out, ",") + " (a name twice)"
		}
	}
	return strings.Join(out, ",")
}

// times is when the views are looked at: mid-year 2010 to 2026, and
// around each step's date (a second before, at, an hour after), or a
// sample of them.
func (c *mCase) times() []time.Time {
	var out []time.Time
	for y := 2010; y <= 2026; y++ {
		out = append(out, midYear(y))
	}
	var dated []time.Time
	for _, s := range c.steps {
		if !s.at.IsZero() {
			dated = append(dated, s.at)
		}
	}
	if c.sample > 0 && len(dated) > c.sample {
		pick := dated[len(dated)-c.sample/2:]
		every := (len(dated) - c.sample/2) / (c.sample / 2)
		for i := 0; i < len(dated)-c.sample/2; i += max(every, 1) {
			pick = append(pick, dated[i])
		}
		dated = pick
	}
	for _, t := range dated {
		out = append(out, t.Add(-time.Second), t, t.Add(time.Hour))
	}
	return out
}

// realAt is every state dir really had as of t: as of t by any prefix
// of the steps applied (the history known when that step was written;
// what a view of t could show then without showing a state the folder
// never had), and none.
func (c *mCase) realAt(dir string, t time.Time) map[string]bool {
	type last struct {
		at time.Time
		in bool
	}
	cur := map[string]last{}
	real := map[string]bool{"": true}
	emit := func() {
		var names []string
		for n, l := range cur {
			if l.in {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		real[strings.Join(names, ",")] = true
	}
	for _, s := range c.steps {
		if s.op == "settle" || s.at.After(t) {
			continue
		}
		set := func(p string, in bool) {
			if path.Dir(p) != dir {
				return
			}
			n := path.Base(p)
			if l, ok := cur[n]; !ok || !s.at.Before(l.at) {
				cur[n] = last{s.at, in}
			}
		}
		switch s.op {
		case "w", "mkdir", "restore":
			set(s.p, true)
		case "rm", "rmdir":
			set(s.p, false)
		case "mv":
			set(s.p, false)
			set(s.to, true)
		}
		emit()
	}
	emit()
	return real
}

// checkReal: every view of every folder is a state it really had then.
func (c *mCase) checkReal(when string) {
	c.t.Helper()
	for _, dir := range c.dirs {
		for _, t := range c.times() {
			if got := c.namesAt(dir, t); !c.realAt(dir, t)[got] {
				c.t.Errorf("%s: %s as of %s is %q: no state it ever had then (model now: %q)",
					when, dir, t.Format(time.RFC3339), got, modelAt(c.steps, dir, t))
			}
		}
	}
}

// checkExact: every folder's past and present are the model's.
func (c *mCase) checkExact(when string) {
	c.t.Helper()
	for _, dir := range c.dirs {
		for _, t := range append(c.times(), time.Time{}) {
			if got, want := c.namesAt(dir, t), modelAt(c.steps, dir, t); got != want {
				at := "now"
				if !t.IsZero() {
					at = t.Format(time.RFC3339)
				}
				c.t.Errorf("%s: %s as of %s is %q, want %q", when, dir, at, got, want)
			}
		}
	}
}

// newBuckets is one MinIO for a test's cases, a fresh bucket each.
func newBuckets(t *testing.T) func(t *testing.T) objstore.Store {
	t.Helper()
	return miniotest.Buckets(t)
}

// envSet reports whether the environment variable name is set and not
// empty.
func envSet(name string) bool { return os.Getenv(name) != "" }

// --- The design review's cases (2026-11-17), brought in ---

// splitBase: /dir in blocks of 4: f0..f4 by 2015, f5 in 2017, so the
// right block is full; then x late at 2016 into it (it splits).
func splitBase() []mStep {
	return append(mfiles("/dir", 2011, 2012, 2013, 2014, 2015, 2017), mw(year(2016), "/dir/x.txt"))
}

// TestModelSplitByLateWrite is the round-3 KNOWN case: a late write
// that splits a full block of a folder kept in blocks.
func TestModelSplitByLateWrite(t *testing.T) {
	t.Parallel()
	bucket := newBuckets(t)
	for _, entries := range []uint32{0, 4} {
		t.Run(fmt.Sprintf("blocks of %d", entries), func(t *testing.T) {
			t.Parallel()
			c := newModelCase(t, bucket(t), fmt.Sprintf("split%d", entries), entries)
			c.run(splitBase()...)
			c.finish()
		})
	}
}

// TestModelOnTimeAfterLate is the review's B1 to B6: on-time writes after
// a late one, in a folder split in blocks of 4, before the settle.
func TestModelOnTimeAfterLate(t *testing.T) {
	t.Parallel()
	bucket := newBuckets(t)
	for _, tc := range []struct {
		name string
		more []mStep
	}{
		{"B1 on time into the new right half", []mStep{mw(year(2018), "/dir/y.txt")}},
		{"B2 on time into the left half", []mStep{mw(year(2018), "/dir/f3a.txt")}},
		{"B3 on time splits again", []mStep{mw(year(2018), "/dir/y.txt"), mw(year(2019), "/dir/z.txt")}},
		{"B4 on-time removal in the new half", []mStep{mrm(year(2018), "/dir/f5.txt")}},
		{"B5 two late, nothing between", []mStep{mw(year(2016), "/dir/f3b.txt"), mw(year(2014), "/dir/f4z.txt")}},
		{"B6 late, on time, late", []mStep{mw(year(2018), "/dir/y.txt"), mw(year(2016), "/dir/f3b.txt")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newModelCase(t, bucket(t), "b"+strings.ToLower(tc.name[:2]), 4)
			c.run(splitBase()...)
			c.run(tc.more...)
			c.finish()
		})
	}
}

// modelMatrix is each kind of late write (dated 2016, behind the
// folder's 2017 state): add, remove, rename within (staying in its block,
// and moving to another), move in, move out, mkdir, rmdir, trash and
// restore, one that splits the root.
func modelMatrix() map[string][]mStep {
	base := func() []mStep { return mfiles("/dir", 2011, 2012, 2013, 2014, 2015, 2017) }
	other := mw(day(2011, 100), "/other/m.txt")
	return map[string][]mStep{
		"add":       append(base(), mw(year(2016), "/dir/x.txt")),
		"remove":    append(base(), mrm(year(2016), "/dir/f1.txt")),
		"rename":    append(base(), mmv(year(2016), "/dir/f1.txt", "/dir/g1.txt")),
		"renameK":   append(base(), mmv(year(2016), "/dir/f1.txt", "/dir/z1.txt")),
		"movein":    append(append([]mStep{other}, base()...), mmv(year(2016), "/other/m.txt", "/dir/m.txt")),
		"moveout":   append(append([]mStep{other}, base()...), mmv(year(2016), "/dir/f3.txt", "/other/f3.txt")),
		"mkdir":     append(base(), mmkdir(year(2016), "/dir/sub")),
		"rmdir":     append(append(base(), mmkdir(day(2012, 10), "/dir/sub")), mrmdir(year(2016), "/dir/sub")),
		"trashrst":  append(base(), mrm(year(2016), "/dir/f4.txt"), mrestore(day(2016, 30), "/dir/f4.txt")),
		"rootsplit": append(mfiles("/dir", 2011, 2012, 2013, 2017), mw(year(2016), "/dir/x.txt")),
	}
}

// TestModelMatrix: the 40 cases: each kind of late write, in a folder of
// one block and one kept in blocks of 4, alone and followed by three
// on-time writes before the settle. `just ci` runs the 20 followed by
// on-time writes (the harder half); T83_MODEL set, all 40.
func TestModelMatrix(t *testing.T) {
	t.Parallel()
	bucket := newBuckets(t)
	m := modelMatrix()
	kinds := make([]string, 0, len(m))
	for k := range m {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		for _, entries := range []uint32{0, 4} {
			for _, then := range []bool{false, true} {
				if !then && !envSet("T83_MODEL") {
					continue // `just ci` runs the 20 with on-time writes after; the lab all 40
				}
				name := fmt.Sprintf("%s blocks of %d on time after %v", kind, entries, then)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					c := newModelCase(t, bucket(t), fmt.Sprintf("mx%s%d%v", strings.ToLower(kind), entries, then), entries, "/dir", "/other")
					c.run(m[kind]...)
					if then {
						c.run(mw(year(2018), "/dir/y.txt"), mw(day(2018, 1), "/dir/a0.txt"), mw(day(2018, 2), "/dir/f2b.txt"))
					}
					c.finish()
				})
			}
		}
	}
}

// TestModelTwoLevels: 20 entries in blocks of 4 (the root and internal
// nodes split), late writes around the tree, a late removal, on-time
// writes between, settled once.
func TestModelTwoLevels(t *testing.T) {
	if os.Getenv("ICBFS_PRE_MERGE") == "" {
		t.Skip("about 60 s under -race: set ICBFS_PRE_MERGE=1 (just pre-merge); in ci, TestSettleSplitByTheLateWrite and TestModelMatrix settle split folders")
	}
	t.Parallel()
	c := newModelCase(t, newBuckets(t)(t), "twolevels", 4)
	for i := 0; i < 20; i++ {
		c.run(mw(day(2011, i*5), fmt.Sprintf("/dir/f%02d.txt", i)))
	}
	c.run(mw(year(2017), "/dir/f99.txt"))
	for i, n := range []string{"f00a", "f05a", "f05b", "f11a", "f11b", "f11c", "f17a"} {
		c.run(mw(day(2016, i), "/dir/"+n+".txt"))
	}
	c.run(mw(year(2018), "/dir/f06a.txt"), mw(day(2018, 1), "/dir/f06b.txt"), mw(day(2018, 2), "/dir/f06c.txt"),
		mrm(day(2016, 50), "/dir/f12.txt"))
	c.finish()
}

// randomSteps is the review's model run: n0 on-time creates, then n ops
// (creates and removals, about half late, the rest on time and moving
// forward), a settle after every g late ones. Dates in whole days from
// 2011.
func randomSteps(seed int64, n0, n, g int) []mStep {
	r := rand.New(rand.NewSource(seed))
	var steps []mStep
	type ent struct {
		name string
		h    int
	}
	var names []ent
	hour := 0
	at := func(h int) time.Time { return year(2011).Add(time.Duration(h) * time.Hour) }
	for i := 0; i < n0; i++ {
		hour += 24 + r.Intn(24*5)
		hour -= hour % 24
		name := fmt.Sprintf("/dir/%08x.txt", r.Uint32())
		names = append(names, ent{name, hour})
		steps = append(steps, mw(at(hour), name))
	}
	late := 0
	for i := 0; i < n; i++ {
		isLate := r.Intn(2) == 0
		h := hour
		if isLate {
			h = 24 + r.Intn(hour)
			h -= h % 24
		} else {
			hour += 24 + r.Intn(24*5)
			hour -= hour % 24
			h = hour
		}
		if r.Intn(4) == 0 && len(names) > 4 {
			// A removal, on time or late, of an entry made before it.
			j := r.Intn(len(names))
			if names[j].h < h {
				steps = append(steps, mrm(at(h), names[j].name))
				names = append(names[:j], names[j+1:]...)
			} else {
				isLate = false
			}
		} else {
			name := fmt.Sprintf("/dir/%08x.txt", r.Uint32())
			names = append(names, ent{name, h})
			steps = append(steps, mw(at(h), name))
		}
		if isLate {
			late++
			if late%g == 0 {
				steps = append(steps, msettle())
			}
		}
	}
	return steps
}

// TestModelRandom: the review's random model at 4 entries a block: 30
// on-time entries, then 30 ops about half late (late removals too), a
// settle every 3 late ones, seeds 2001 to 2008. Long under -race: run
// when T83_RANDOM or T83_MODEL is set (the lab).
func TestModelRandom(t *testing.T) {
	t.Parallel()
	if !envSet("T83_RANDOM") && !envSet("T83_MODEL") {
		t.Skip("T83_RANDOM unset: the random model (10 to 16 minutes a seed under -race) runs on the lab")
	}
	bucket := newBuckets(t)
	for seed := int64(2001); seed <= 2008; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			steps := randomSteps(seed, 30, 30, 3)
			c := newModelCase(t, bucket(t), fmt.Sprintf("rand%d", seed), 4)
			c.run(steps...)
			c.finish()
			t.Logf("seed %d: %d steps, %d settles", seed, len(steps), c.settles)
		})
	}
}

// TestModelRandom128 is the round-3 review's B1 shape, at the default 128
// entries a block: 300 on-time entries (three blocks and more), then 120
// ops about half late, a settle every 5 late ones, seeds 3001 to 3004.
// Long: run when T83_RANDOM128 is set (the lab), views sampled.
func TestModelRandom128(t *testing.T) {
	if !envSet("T83_RANDOM128") {
		t.Skip("T83_RANDOM128 unset: the 128-entry random model runs on the lab")
	}
	bucket := newBuckets(t)
	for seed := int64(3001); seed <= 3004; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			steps := randomSteps(seed, 300, 120, 5)
			c := newModelCase(t, bucket(t), fmt.Sprintf("big%d", seed), 0)
			c.sample = 60
			c.run(steps...)
			c.finish()
			t.Logf("seed %d: %d steps, %d settles", seed, len(steps), c.settles)
		})
	}
}

// --- The owner's case, and the same instant ---

// TestModelLateFileThatNeverReachedThePresent is the owner's case: "What
// if X is a late file that didn't exist in present? Say it only existed
// between 2022 and 2023." A folder with states at 2020 and 2024; the
// daemon applies, late, an add of x dated 2022 and then its removal dated
// 2023. Both are written into the present dated 2024 (the add, then the
// removal), so the present ends without x; after the settle x is in the
// past from 2022 to 2023 and at no other date, and its content reads
// through a view of 2022. In a folder of one block and of many, the two
// changes in one settle and in two.
// STATES: CH-3
func TestModelLateFileThatNeverReachedThePresent(t *testing.T) {
	t.Parallel()
	bucket := newBuckets(t)
	for _, entries := range []uint32{0, 4} {
		for _, two := range []bool{false, true} {
			t.Run(fmt.Sprintf("blocks of %d, two settles %v", entries, two), func(t *testing.T) {
				t.Parallel()
				c := newModelCase(t, bucket(t), fmt.Sprintf("owner%d%v", entries, two), entries)
				// Many blocks: enough files that the folder is split.
				var base []mStep
				base = append(base, mw(year(2020), "/dir/a.txt"))
				if entries > 0 {
					for i := 0; i < 9; i++ {
						base = append(base, mw(day(2020, i+1), fmt.Sprintf("/dir/a%d.txt", i)))
					}
				}
				base = append(base, mw(year(2024), "/dir/b.txt"))
				c.run(base...)
				present := c.namesAt("/dir", time.Time{})
				atTn := c.namesAt("/dir", year(2024))

				c.run(mw(year(2022), "/dir/x.txt"))
				// Between the two writes: the present has x; a view at
				// exactly the folder's newest state shows the present (with
				// x, as the folder really was then: the daemon had applied
				// it); a view before shows the folder as it was.
				if got := c.namesAt("/dir", year(2024)); !strings.Contains(got, "x.txt") {
					t.Errorf("between the writes, at 2024: %q, want the present with x", got)
				}
				if got := c.namesAt("/dir", midYear(2022)); strings.Contains(got, "x.txt") {
					t.Errorf("between the writes, mid-2022: %q: x shown before it is settled", got)
				}
				if two {
					c.run(msettle())
					if got := read(t, system(), c.d, drive.ByPath("/dir/x.txt"), drive.At{Time: midYear(2022)}); got != "/dir/x.txt" {
						t.Errorf("x as of mid-2022 after the first settle: %q", got)
					}
				}
				c.run(mrm(year(2023), "/dir/x.txt"))
				// After both writes: at the newest state, the folder as it
				// was before x (a state it really had: x was gone by then).
				if got := c.namesAt("/dir", year(2024)); got != atTn {
					t.Errorf("after both writes, at 2024: %q, want %q", got, atTn)
				}
				if got := c.namesAt("/dir", time.Time{}); got != present {
					t.Errorf("after both writes, now: %q, want %q", got, present)
				}
				c.finish()
				for _, when := range []time.Time{year(2021), day(2022, -1), year(2022), midYear(2022), day(2023, -1), year(2023), midYear(2023), year(2024), time.Now()} {
					want := strings.Contains(modelAt(c.steps, "/dir", when), "x.txt")
					if got := strings.Contains(c.namesAt("/dir", when), "x.txt"); got != want {
						t.Errorf("x as of %s: %v, want %v", when.Format(time.RFC3339), got, want)
					}
				}
				if got := read(t, system(), c.d, drive.ByPath("/dir/x.txt"), drive.At{Time: midYear(2022)}); got != "/dir/x.txt" {
					t.Errorf("x's content as of mid-2022: %q", got)
				}
				// Its item after the removal: in the trash, deleted at its
				// source date (2023).
				page, err := c.d.Trash(system(), drive.TrashOptions{})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, e := range page.Entries {
					if e.ItemID == c.ids["/dir/x.txt"] {
						found = true
						if !e.DeletedAt.Equal(year(2023)) {
							t.Errorf("x's trash entry is dated %s, want 2023", e.DeletedAt.Format(time.RFC3339))
						}
					}
				}
				if !found {
					t.Errorf("x isn't in the trash")
				}
			})
		}
	}
}

// TestModelSameInstant: two and three late files at one instant, in one
// settle and in several; a late file dated exactly at a state the folder
// has; in a folder of one block and of many.
func TestModelSameInstant(t *testing.T) {
	t.Parallel()
	bucket := newBuckets(t)
	when := day(2021, 37)
	for _, entries := range []uint32{0, 4} {
		base := func() []mStep {
			var b []mStep
			for i := 0; i < 7; i++ {
				b = append(b, mw(day(2020, i), fmt.Sprintf("/dir/a%d.txt", i)))
			}
			return append(b, mw(year(2022), "/dir/b.txt"), mw(year(2024), "/dir/c.txt"))
		}
		for i, tc := range []struct {
			name  string
			steps []mStep
		}{
			{"two in one settle", []mStep{mw(when, "/dir/l1.txt"), mw(when, "/dir/l2.txt")}},
			{"two in two settles", []mStep{mw(when, "/dir/l1.txt"), msettle(), mw(when, "/dir/l2.txt")}},
			{"three in one settle", []mStep{mw(when, "/dir/l1.txt"), mw(when, "/dir/l2.txt"), mw(when, "/dir/l3.txt")}},
			{"three in two settles, 2+1", []mStep{mw(when, "/dir/l1.txt"), mw(when, "/dir/l2.txt"), msettle(), mw(when, "/dir/l3.txt")}},
			{"three in three settles", []mStep{mw(when, "/dir/l1.txt"), msettle(), mw(when, "/dir/l2.txt"), msettle(), mw(when, "/dir/l3.txt")}},
			{"at a state's own time", []mStep{mw(year(2022), "/dir/l1.txt"), mw(year(2022), "/dir/l2.txt")}},
			{"add and remove at one instant", []mStep{mw(when, "/dir/l1.txt"), mrm(when, "/dir/a3.txt"), msettle(), mrm(when, "/dir/l1.txt")}},
		} {
			t.Run(fmt.Sprintf("%s, blocks of %d", tc.name, entries), func(t *testing.T) {
				t.Parallel()
				c := newModelCase(t, bucket(t), fmt.Sprintf("inst%d-%d", entries, i), entries)
				c.run(base()...)
				c.run(tc.steps...)
				c.finish()
			})
		}
	}
}
