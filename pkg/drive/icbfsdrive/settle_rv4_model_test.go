package icbfsdrive

// Round-4 reviewer's model (2026-11-17). Independent of settle_model_test.go's
// oracle: the reference is per item, not per folder. Every operation is a set
// of membership changes (folder, name, item, in/out) carrying the source date D
// and, per folder, the date the folder was written at (W: D when on time, the
// folder's newest state by this model when late). What a view of folder F at t
// must show:
//   - before the change is settled: the changes ordered by (W, seq), those with
//     W <= t applied (strict equality, not "some prefix");
//   - once settled: ordered by (D, seq), those with D <= t applied.
// Every reported row is checked against the model too (op, name, item, At = D,
// Until = the model's newest state of the folder before the write).

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

type rvItem struct {
	id, uuid, folder, name string
	last                   time.Time
	dir, trashed           bool
	trashFolder            string
}

type rvChange struct {
	folder, name, uuid string
	in                 bool
	d, w               time.Time
	seq                int
	late               bool
	settled            bool
}

type rvRow struct {
	op, name, item string
	at, until      time.Time
}

type rvModel struct {
	t        *testing.T
	r        *rand.Rand
	d        *Drive
	folders  []string
	keys     map[string]string
	created  map[string]time.Time
	newest   map[string]time.Time
	states   map[string][]time.Time // each folder's written dates
	changes  []rvChange
	items    []*rvItem
	queue    map[string][]drive.LateChange // by folder key
	keepers  map[string]*keeper
	seq      int
	now      time.Time // the on-time cursor
	lastLate time.Time
	settles  int
	k        int
	lateN    int
	counts   map[string]int
	fails    []string
	sample   int
	names    int
}

func (m *rvModel) fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	m.fails = append(m.fails, msg)
	m.t.Error(msg)
	if len(m.fails) > 30 {
		m.t.Fatalf("too many failures")
	}
}

func rvCtx(late *drive.LateChanges, at time.Time) context.Context {
	ctx := drive.WithLateChanges(system(), late)
	if !at.IsZero() {
		ctx = drive.WithSourceTime(ctx, at)
	}
	return ctx
}

// view is folder f's entries (name -> uuid) as of t (present if zero); ok
// false if the folder had no state then.
func (m *rvModel) view(f string, t time.Time) (map[string]string, bool, string) {
	ctx := system()
	fsys := m.d.fs
	var err error
	if !t.IsZero() {
		if fsys, err = m.d.at(ctx, drive.At{Time: t}); err != nil {
			m.t.Fatalf("view at %s: %v", t, err)
		}
	}
	entries, err := fsys.ReadDir(ctx, m.keys[f])
	if err != nil {
		if t.IsZero() {
			m.t.Fatalf("%s now: %v", f, err)
		}
		return nil, false, err.Error()
	}
	out := map[string]string{}
	dup := ""
	for _, e := range entries {
		if _, ok := out[e.Name]; ok {
			dup += " twice:" + e.Name
		}
		out[e.Name] = e.UUID
	}
	return out, true, dup
}

// expect is folder f as of t (present if zero) by the model; exact: every
// change at its source date (as after a settle of everything).
func (m *rvModel) expect(f string, t time.Time, exact bool) map[string]string {
	var cs []rvChange
	for _, c := range m.changes {
		if c.folder == f {
			cs = append(cs, c)
		}
	}
	eff := func(c rvChange) time.Time {
		if c.late && !c.settled && !exact {
			return c.w
		}
		return c.d
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := eff(cs[i]), eff(cs[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return cs[i].seq < cs[j].seq
	})
	out := map[string]string{}
	for _, c := range cs {
		if !t.IsZero() && eff(c).After(t) {
			break
		}
		if c.in {
			out[c.name] = c.uuid
		} else if out[c.name] == c.uuid {
			delete(out, c.name)
		}
	}
	return out
}

func fmtSet(s map[string]string) string {
	var out []string
	for n := range s {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func sameSet(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// times is when views are checked: mid-year 2010..2026, and a second
// before, at and after each date the model knows (sampled to m.sample).
func (m *rvModel) times(all bool) []time.Time {
	seen := map[time.Time]bool{}
	var dates []time.Time
	add := func(t time.Time) {
		if !t.IsZero() && !seen[t] {
			seen[t] = true
			dates = append(dates, t)
		}
	}
	for _, c := range m.changes {
		add(c.d)
		add(c.w)
	}
	for _, f := range m.folders {
		add(m.created[f])
		for _, s := range m.states[f] {
			add(s)
		}
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	if !all && m.sample > 0 && len(dates) > m.sample {
		keep := append([]time.Time(nil), dates[len(dates)-m.sample/3:]...)
		rest := dates[:len(dates)-m.sample/3]
		perm := m.r.Perm(len(rest))
		for i := 0; i < m.sample-m.sample/3 && i < len(perm); i++ {
			keep = append(keep, rest[perm[i]])
		}
		dates = keep
	}
	var out []time.Time
	for y := 2010; y <= 2026; y++ {
		out = append(out, time.Date(y, 7, 1, 0, 0, 0, 0, time.UTC))
	}
	for _, t := range dates {
		out = append(out, t.Add(-time.Second), t, t.Add(time.Second))
	}
	return out
}

func (m *rvModel) check(when string, exact, all bool) {
	m.t.Helper()
	for _, f := range m.folders {
		for _, t := range append(m.times(all), time.Time{}) {
			got, ok, dup := m.view(f, t)
			want := m.expect(f, t, exact)
			m.counts["views"]++
			if !t.IsZero() && t.Before(m.created[f]) {
				// Before the folder was made: by key, not found, or (after a
				// settle of a change dated before it) what the changes say.
				if !ok || (len(got) == 0 && len(want) == 0) || sameSet(got, want) && dup == "" {
					if ok && len(got) > 0 {
						m.counts["before creation: shown by key"]++
					}
					continue
				}
			}
			if !ok {
				if len(want) == 0 {
					continue
				}
				m.fail("%s: %s as of %s: no state (%s), want %q", when, f, t.Format(time.RFC3339), dup, fmtSet(want))
				continue
			}
			if dup != "" || !sameSet(got, want) {
				m.fail("%s: %s as of %s: %q%s, want %q (exact %q)", when, f, fmtT(t), fmtSet(got), dup, fmtSet(want), fmtSet(m.expect(f, t, true)))
			}
		}
	}
}

func fmtT(t time.Time) string {
	if t.IsZero() {
		return "now"
	}
	return t.Format(time.RFC3339)
}

// write: per folder, whether the change at d is late and the date the folder
// is written at; advances the model's newest.
func (m *rvModel) written(f string, d time.Time) (bool, time.Time) {
	n := m.newest[f]
	if d.Before(n) {
		return true, n
	}
	return false, d
}

type rvMember struct {
	folder, name string
	in           bool
}

// apply runs fn at d (undated if zero), with the folder membership changes
// mem for item it; checks the rows reported against the model.
func (m *rvModel) apply(desc string, d time.Time, it *rvItem, mem []rvMember, fn func(ctx context.Context) error) {
	m.t.Helper()
	late := &drive.LateChanges{}
	ctx := rvCtx(late, d)
	before := time.Now()
	err := fn(ctx)
	if err != nil {
		m.t.Fatalf("op %d %s at %s: %v", m.seq, desc, fmtT(d), err)
	}
	rows := late.Take()
	m.seq++
	if os.Getenv("RV4_TRACE") != "" {
		m.t.Logf("TRACE op %d %s at %s: %d rows", m.seq, desc, fmtT(d), len(rows))
	}
	// The uuid of a new item.
	for _, mb := range mem {
		if mb.in && it.uuid == "" {
			got, _, _ := m.view(mb.folder, time.Time{})
			it.uuid = got[mb.name]
			if it.uuid == "" {
				m.t.Fatalf("op %s: %s not in %s now", desc, mb.name, mb.folder)
			}
		}
	}
	want := map[string][]rvRow{}
	folderW := map[string]time.Time{}
	folderLate := map[string]bool{}
	for _, mb := range mem {
		if _, done := folderW[mb.folder]; done {
			continue
		}
		var isLate bool
		var w time.Time
		if d.IsZero() {
			// Undated: the store's clock; the drive says when.
			lt, err := m.d.LatestState(system(), drive.ByPath(mb.folder))
			if err != nil {
				m.t.Fatal(err)
			}
			w = lt
			if w.Before(before.Add(-5*time.Second)) || w.After(time.Now().Add(5*time.Second)) {
				m.fail("undated op %s: %s's newest state %s, not the store's now", desc, mb.folder, w)
			}
		} else {
			isLate, w = m.written(mb.folder, d)
		}
		folderW[mb.folder], folderLate[mb.folder] = w, isLate
		if w.After(m.newest[mb.folder]) {
			m.newest[mb.folder] = w
		}
		m.states[mb.folder] = append(m.states[mb.folder], w)
	}
	for _, mb := range mem {
		dd := d
		if d.IsZero() {
			dd = folderW[mb.folder]
		}
		c := rvChange{folder: mb.folder, name: mb.name, uuid: it.uuid, in: mb.in, d: dd, w: folderW[mb.folder], seq: m.seq, late: folderLate[mb.folder]}
		m.changes = append(m.changes, c)
		if c.late {
			op := drive.LateRemove
			if mb.in {
				op = drive.LateAdd
			}
			want[m.keys[mb.folder]] = append(want[m.keys[mb.folder]], rvRow{op, mb.name, it.uuid, d, folderW[mb.folder]})
			m.counts["late changes"]++
		} else {
			m.counts["on-time changes"]++
		}
	}
	got := map[string][]rvRow{}
	for _, r := range rows {
		got[r.Folder] = append(got[r.Folder], rvRow{r.Op, r.Name, r.Item, r.At, r.Until})
		if _, ok := m.queue[r.Folder]; !ok {
			m.queue[r.Folder] = nil
		}
		m.queue[r.Folder] = append(m.queue[r.Folder], r)
	}
	norm := func(rs []rvRow) string {
		var s []string
		for _, r := range rs {
			s = append(s, fmt.Sprintf("%s %s %s %s..%s", r.op, r.name, r.item, r.at.Format(time.RFC3339Nano), r.until.Format(time.RFC3339Nano)))
		}
		sort.Strings(s)
		return strings.Join(s, "; ")
	}
	for f, rs := range want {
		if norm(rs) != norm(got[f]) {
			m.fail("op %d %s at %s: rows for %s:\n got  %s\n want %s", m.seq, desc, fmtT(d), f, norm(got[f]), norm(rs))
		}
	}
	for f, rs := range got {
		if _, ok := want[f]; !ok {
			m.fail("op %d %s at %s: unexpected rows for %s: %s", m.seq, desc, fmtT(d), f, norm(rs))
		}
	}
	if len(rows) > 0 {
		m.lateN++
		m.lastLate = d
	}
	// The present after every op.
	for _, f := range m.folders {
		got, _, dup := m.view(f, time.Time{})
		if want := m.expect(f, time.Time{}, false); dup != "" || !sameSet(got, want) {
			m.t.Fatalf("after op %d %s at %s: %s now %q%s, want %q", m.seq, desc, fmtT(d), f, fmtSet(got), dup, fmtSet(want))
		}
	}
	if !d.IsZero() && it != nil {
		it.last = d
	}
	if len(rows) > 0 && m.k > 0 && m.lateN%m.k == 0 {
		m.settle()
	}
}

func (m *rvModel) settle() {
	m.t.Helper()
	m.check(fmt.Sprintf("before settle %d", m.settles+1), false, false)
	m.settles++
	var fs []string
	for f := range m.queue {
		fs = append(fs, f)
	}
	sort.Strings(fs)
	m.r.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] })
	for _, f := range fs {
		k := m.keepers[f]
		if k == nil {
			k = &keeper{}
			m.keepers[f] = k
		}
		s, err := m.d.SettleFolder(system(), m.queue[f], k.opts())
		m.counts["settles"]++
		if err != nil {
			m.fail("settle %d of %s (%d rows): %v", m.settles, f, len(m.queue[f]), err)
			if strings.Contains(err.Error(), "refused") {
				m.counts["refused"]++
			}
			continue
		}
		m.counts["states written"] += s.Written
		if k.held() != nil {
			m.fail("settle %d of %s: record held after it", m.settles, f)
		}
		for i := range m.changes {
			if m.keys[m.changes[i].folder] == f {
				m.changes[i].settled = true
			}
		}
		delete(m.queue, f)
	}
	// A settle with nothing queued.
	if s, err := m.d.SettleFolder(system(), nil, drive.SettleOptions{}); err != nil || s != (drive.Settled{}) {
		m.fail("a settle with nothing queued: %+v, %v", s, err)
	}
	m.check(fmt.Sprintf("after settle %d", m.settles), false, false)
}

func (m *rvModel) newName(prefix string) string {
	m.names++
	return fmt.Sprintf("%s%04d", prefix, m.names)
}

// lateDate is a date behind folder f's newest state, after lb, by one of the
// modes; zero if none fits.
func (m *rvModel) lateDate(f string, lb time.Time, isNew bool) (time.Time, string) {
	n := m.newest[f]
	pick := m.r.Intn(100)
	switch {
	case pick < 15:
		// exactly at one of the folder's existing states
		var cand []time.Time
		for _, s := range m.states[f] {
			if s.After(lb) && s.Before(n) {
				cand = append(cand, s)
			}
		}
		if len(cand) > 0 {
			return cand[m.r.Intn(len(cand))], "at a state"
		}
	case pick < 25:
		if n.After(lb) {
			return n, "at the newest state"
		}
	case pick < 40:
		if !m.lastLate.IsZero() && m.lastLate.After(lb) && m.lastLate.Before(n) {
			return m.lastLate, "same instant"
		}
	case pick < 48:
		if isNew {
			return time.Date(2010, time.Month(3+m.r.Intn(9)), 1+m.r.Intn(27), m.r.Intn(24), 0, 0, 0, time.UTC), "before the folder"
		}
	}
	lo := lb
	if lo.IsZero() || lo.Before(time.Date(2011, 1, 1, 0, 0, 0, 0, time.UTC)) {
		lo = time.Date(2011, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	hours := int(n.Sub(lo) / time.Hour)
	if hours < 2 {
		return time.Time{}, ""
	}
	return lo.Add(time.Duration(1+m.r.Intn(hours-1)) * time.Hour), "behind"
}

func (m *rvModel) onTime() time.Time {
	if m.r.Intn(10) > 0 {
		m.now = m.now.Add(time.Duration(1+m.r.Intn(72)) * time.Hour)
	}
	for _, f := range m.folders {
		if m.newest[f].After(m.now) && m.newest[f].Year() < 2020 {
			m.now = m.newest[f]
		}
	}
	return m.now
}

func (m *rvModel) live(f string, dir *bool) []*rvItem {
	var out []*rvItem
	for _, it := range m.items {
		if !it.trashed && it.folder == f && (dir == nil || it.dir == *dir) {
			out = append(out, it)
		}
	}
	return out
}

// step makes one random operation.
func (m *rvModel) step(allowUndated bool) {
	f := m.folders[m.r.Intn(len(m.folders))]
	g := m.folders[0]
	if f == g {
		g = m.folders[1]
	}
	isLate := m.r.Intn(100) < 45
	when := func(it *rvItem, folders ...string) (time.Time, string) {
		if isLate {
			lb := time.Time{}
			if it != nil {
				lb = it.last
			}
			d, mode := m.lateDate(folders[0], lb, it == nil)
			if !d.IsZero() && (it == nil || d.After(it.last)) {
				m.counts["late mode: "+mode]++
				return d, "late(" + mode + ")"
			}
		}
		d := m.onTime()
		if it != nil && !d.After(it.last) {
			return time.Time{}, "skip"
		}
		return d, "on time"
	}
	no := false
	yes := true
	op := m.r.Intn(100)
	switch {
	case allowUndated && op < 2 && f == "/d":
		// An undated write (the store's clock): T83-0's "write stamped now".
		it := &rvItem{folder: f, name: m.newName("u")}
		m.apply("undated add "+f+"/"+it.name, time.Time{}, it, []rvMember{{f, it.name, true}}, func(ctx context.Context) error {
			x, err := m.d.Write(ctx, f+"/"+it.name, strings.NewReader(it.name), drive.Condition{}, drive.WriteOptions{})
			it.id = x.ID
			return err
		})
		it.last = m.newest[f]
		m.items = append(m.items, it)
		m.counts["undated"]++
	case op < 30:
		it := &rvItem{folder: f, name: m.newName("n")}
		d, how := when(nil, f)
		if how == "skip" {
			return
		}
		m.apply("add "+f+"/"+it.name+" "+how, d, it, []rvMember{{f, it.name, true}}, func(ctx context.Context) error {
			x, err := m.d.Write(ctx, f+"/"+it.name, strings.NewReader(it.name), drive.Condition{}, drive.WriteOptions{})
			it.id = x.ID
			return err
		})
		m.items = append(m.items, it)
	case op < 34:
		// The owner's case: a file added late and removed late.
		it := &rvItem{folder: f, name: m.newName("o")}
		isLate = true
		d, how := when(nil, f)
		if how == "skip" {
			return
		}
		if !strings.HasPrefix(how, "late") {
			return
		}
		m.apply("owner add "+f+"/"+it.name+" "+how, d, it, []rvMember{{f, it.name, true}}, func(ctx context.Context) error {
			x, err := m.d.Write(ctx, f+"/"+it.name, strings.NewReader(it.name), drive.Condition{}, drive.WriteOptions{})
			it.id = x.ID
			return err
		})
		m.items = append(m.items, it)
		d2, how2 := when(it, f)
		if how2 == "skip" {
			return
		}
		if !strings.HasPrefix(how2, "late") {
			return
		}
		m.apply("owner remove "+f+"/"+it.name+" "+how2, d2, it, []rvMember{{f, it.name, false}}, func(ctx context.Context) error {
			return m.d.Delete(ctx, drive.ByPath(f+"/"+it.name), false, drive.Condition{})
		})
		it.trashed, it.trashFolder = true, f
		m.counts["owner case"]++
	case op < 46:
		files := m.live(f, &no)
		if len(files) == 0 {
			return
		}
		it := files[m.r.Intn(len(files))]
		d, how := when(it, f)
		if how == "skip" {
			return
		}
		m.apply("remove "+f+"/"+it.name+" "+how, d, it, []rvMember{{f, it.name, false}}, func(ctx context.Context) error {
			return m.d.Delete(ctx, drive.ByPath(f+"/"+it.name), false, drive.Condition{})
		})
		it.trashed, it.trashFolder = true, f
	case op < 56:
		files := m.live(f, &no)
		if len(files) == 0 {
			return
		}
		it := files[m.r.Intn(len(files))]
		d, how := when(it, f)
		if how == "skip" {
			return
		}
		nn := m.newName("r")
		m.apply("rename "+f+"/"+it.name+" -> "+nn+" "+how, d, it, []rvMember{{f, it.name, false}, {f, nn, true}}, func(ctx context.Context) error {
			_, err := m.d.Move(ctx, drive.ByPath(f+"/"+it.name), f+"/"+nn, false)
			return err
		})
		it.name = nn
	case op < 70:
		files := m.live(f, &no)
		if len(files) == 0 {
			return
		}
		it := files[m.r.Intn(len(files))]
		// late against the source, or the destination, chosen at random
		ref := f
		if m.r.Intn(2) == 0 {
			ref = g
		}
		d, how := when(it, ref)
		if how == "skip" {
			return
		}
		m.apply("move "+f+"/"+it.name+" -> "+g+" "+how, d, it, []rvMember{{f, it.name, false}, {g, it.name, true}}, func(ctx context.Context) error {
			_, err := m.d.Move(ctx, drive.ByPath(f+"/"+it.name), g+"/"+it.name, false)
			return err
		})
		it.folder = g
	case op < 76:
		it := &rvItem{folder: f, name: m.newName("c"), dir: true}
		d, how := when(nil, f)
		if how == "skip" {
			return
		}
		m.apply("mkdir "+f+"/"+it.name+" "+how, d, it, []rvMember{{f, it.name, true}}, func(ctx context.Context) error {
			x, err := m.d.Mkdir(ctx, f+"/"+it.name, false)
			it.id = x.ID
			return err
		})
		m.items = append(m.items, it)
	case op < 81:
		dirs := m.live(f, &yes)
		if len(dirs) == 0 {
			return
		}
		it := dirs[m.r.Intn(len(dirs))]
		d, how := when(it, f)
		if how == "skip" {
			return
		}
		m.apply("rmdir "+f+"/"+it.name+" "+how, d, it, []rvMember{{f, it.name, false}}, func(ctx context.Context) error {
			return m.d.Delete(ctx, drive.ByPath(f+"/"+it.name), true, drive.Condition{})
		})
		it.trashed, it.trashFolder = true, f
	case op < 90:
		var tr []*rvItem
		for _, it := range m.items {
			if it.trashed && !it.dir {
				tr = append(tr, it)
			}
		}
		if len(tr) == 0 {
			return
		}
		it := tr[m.r.Intn(len(tr))]
		tf := it.trashFolder
		d, how := when(it, tf)
		if how == "skip" {
			return
		}
		m.apply("restore "+tf+"/"+it.name+" "+how, d, it, []rvMember{{tf, it.name, true}}, func(ctx context.Context) error {
			entry := ""
			cursor := ""
			for {
				page, err := m.d.Trash(system(), drive.TrashOptions{Cursor: cursor})
				if err != nil {
					return err
				}
				for _, e := range page.Entries {
					if e.ItemID == it.id {
						entry = e.ID
					}
				}
				if page.Next == "" {
					break
				}
				cursor = page.Next
			}
			if entry == "" {
				return fmt.Errorf("%s not in the trash", it.name)
			}
			_, err := m.d.Restore(ctx, entry, drive.RestoreOptions{})
			return err
		})
		it.trashed, it.folder = false, tf
	default:
		if os.Getenv("RV4_NOSETINFO") != "" {
			return
		}
		// The folder's own record.
		d, how := when(nil, f)
		if how == "skip" {
			return
		}
		late := &drive.LateChanges{}
		ctx := rvCtx(late, d)
		// The opposite of the folder's mode now, so the write changes it.
		cur, err := m.d.Stat(system(), drive.ByPath(f), drive.At{})
		if err != nil {
			m.t.Fatal(err)
		}
		mode := uint32(0o750)
		if cur.Mode&0o777 == 0o750 {
			mode = 0o755
		}
		if _, err := m.d.SetInfo(ctx, drive.ByPath(f), drive.ItemInfo{Mode: &mode}); err != nil {
			m.fail("setinfo %s %s at %s: %v", f, how, fmtT(d), err)
			return
		}
		if rows := late.Take(); len(rows) > 0 {
			m.fail("setinfo %s: rows %v", f, rows)
		}
		m.seq++
		if !d.Before(m.newest[f]) {
			m.newest[f] = d
			m.states[f] = append(m.states[f], d)
		}
		m.counts["setinfo "+how[:4]]++
		if os.Getenv("RV4_TRACE") != "" {
			lt, _ := m.d.LatestState(system(), drive.ByPath(f))
			m.t.Logf("TRACE op %d setinfo %s %s mode %o at %s; drive's newest now %s, model's %s", m.seq, f, how, mode, fmtT(d), fmtT(lt), fmtT(m.newest[f]))
		}
	}
}

func newRVModel(t *testing.T, seed int64, entries uint32, k int) *rvModel {
	store := newBuckets(t)(t)
	name := fmt.Sprintf("rv%d", seed)
	d := newDriveWith(t, store, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC), DirBlockEntries: entries},
		Config{DeferLateFolders: true})
	m := &rvModel{t: t, r: rand.New(rand.NewSource(seed)), d: d, folders: []string{"/d", "/o"}, keys: map[string]string{},
		created: map[string]time.Time{}, newest: map[string]time.Time{}, states: map[string][]time.Time{},
		queue: map[string][]drive.LateChange{}, keepers: map[string]*keeper{}, k: k, counts: map[string]int{}}
	for i, f := range m.folders {
		at := time.Date(2011, 1, 1+i, 0, 0, 0, 0, time.UTC)
		it, err := d.Mkdir(drive.WithSourceTime(system(), at), f, false)
		if err != nil {
			t.Fatal(err)
		}
		m.keys[f] = it.ID
		m.created[f], m.newest[f] = at, at
		m.states[f] = []time.Time{at}
	}
	m.now = time.Date(2011, 1, 3, 0, 0, 0, 0, time.UTC)
	return m
}

func rvEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// TestRV4Model: RV4_SEEDS="a-b", RV4_ENTRIES (0 = default 128), RV4_N0 on-time
// creates first, RV4_OPS random ops, RV4_K settle every k late changes,
// RV4_UNDATED=1 allows undated writes into /d late in the run.
func TestRV4Model(t *testing.T) {
	spec := os.Getenv("RV4_SEEDS")
	if spec == "" {
		t.Skip("RV4_SEEDS unset")
	}
	a, b, _ := strings.Cut(spec, "-")
	lo, _ := strconv.ParseInt(a, 10, 64)
	hi := lo
	if b != "" {
		hi, _ = strconv.ParseInt(b, 10, 64)
	}
	entries := uint32(rvEnvInt("RV4_ENTRIES", 4))
	n0, ops := rvEnvInt("RV4_N0", 12), rvEnvInt("RV4_OPS", 60)
	ks := strings.Split(os.Getenv("RV4_K"), ",")
	if os.Getenv("RV4_K") == "" {
		ks = []string{"1", "2", "3", "5"}
	}
	for seed := lo; seed <= hi; seed++ {
		for _, ksz := range ks {
			k, _ := strconv.Atoi(ksz)
			t.Run(fmt.Sprintf("seed%d-k%d-e%d", seed, k, entries), func(t *testing.T) {
				t.Parallel()
				m := newRVModel(t, seed*100+int64(k), entries, k)
				m.sample = rvEnvInt("RV4_SAMPLE", 40)
				undated := os.Getenv("RV4_UNDATED") != "" && seed%2 == 0
				for i := 0; i < n0; i++ {
					f := m.folders[i%2]
					if i%3 != 0 {
						f = "/d"
					}
					it := &rvItem{folder: f, name: m.newName("i")}
					m.apply("initial add "+f+"/"+it.name, m.onTime(), it, []rvMember{{f, it.name, true}}, func(ctx context.Context) error {
						x, err := m.d.Write(ctx, f+"/"+it.name, strings.NewReader(it.name), drive.Condition{}, drive.WriteOptions{})
						it.id = x.ID
						return err
					})
					m.items = append(m.items, it)
				}
				for i := 0; i < ops; i++ {
					m.step(undated && i > ops*6/10)
				}
				m.settle()
				m.check("final, every date", true, true)
				rep, err := m.d.fs.Check(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range rep.Problems {
					m.fail("fs check: %+v", p)
				}
				var keys []string
				for k := range m.counts {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				var b strings.Builder
				for _, k := range keys {
					fmt.Fprintf(&b, " %s=%d", k, m.counts[k])
				}
				t.Logf("RESULT seed=%d k=%d entries=%d ops=%d settles=%d fails=%d:%s", seed, k, entries, m.seq, m.settles, len(m.fails), b.String())
			})
		}
	}
}
