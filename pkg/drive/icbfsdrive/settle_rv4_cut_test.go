package icbfsdrive

// Round-4 reviewer, probe 5: a settle of a folder kept in blocks of 4, with
// three rows of different Until, cut at every request (the request fails;
// or it is made and its answer lost), then rerun with the record from a new
// Drive with an empty cache; some reruns cut again. Variants: the rerun given
// more rows than the cut settle had; a new late write before the rerun (the
// caller breaking the order); two settlers at once.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

type rvMem struct {
	name string
	in   bool
	d, w time.Time
}

func rvMonth(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

// rvCutBase: /dir in blocks of 4: f00..f11 on time in 2011 (monthly), late x1
// (2011-03-15, until 2011-12-01), g on time 2012-06-01, late removal of f05
// (2011-08-15) and late x2 (2011-10-15), both until 2012-06-01.
func rvCutBase(t *testing.T, st objstore.Store, name string) (*Drive, []drive.LateChange, []rvMem) {
	d := newDriveWith(t, st, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: 4}, Config{DeferLateFolders: true})
	var mem []rvMem
	var rows []drive.LateChange
	w := func(p string, at time.Time, rm bool) {
		late := &drive.LateChanges{}
		ctx := drive.WithSourceTime(drive.WithLateChanges(system(), late), at)
		var err error
		if rm {
			err = d.Delete(ctx, drive.ByPath(p), false, drive.Condition{})
		} else {
			_, err = d.Write(ctx, p, strings.NewReader(p), drive.Condition{}, drive.WriteOptions{Parents: true})
		}
		if err != nil {
			t.Fatalf("%s at %s: %v", p, at, err)
		}
		rs := late.Take()
		rows = append(rows, rs...)
		wd := at
		if len(rs) > 0 {
			wd = rs[0].Until
		}
		mem = append(mem, rvMem{p[len("/dir/"):], !rm, at, wd})
	}
	for i := 0; i < 12; i++ {
		w(fmt.Sprintf("/dir/f%02d.txt", i), rvMonth(2011, 1+i, 1), false)
	}
	w("/dir/x1.txt", rvMonth(2011, 3, 15), false)
	w("/dir/g.txt", rvMonth(2012, 6, 1), false)
	w("/dir/f05.txt", rvMonth(2011, 8, 15), true)
	w("/dir/x2.txt", rvMonth(2011, 10, 15), false)
	if len(rows) != 3 {
		t.Fatalf("rows %d, want 3: %+v", len(rows), rows)
	}
	return d, rows, mem
}

// rvState is the folder by mem at t: settled, every change at its date;
// else the late ones at their written date.
func rvState(mem []rvMem, t time.Time, settled bool) string {
	type ev struct {
		at time.Time
		i  int
		m  rvMem
	}
	var evs []ev
	for i, m := range mem {
		at := m.w
		if settled {
			at = m.d
		}
		evs = append(evs, ev{at, i, m})
	}
	sort.SliceStable(evs, func(a, b int) bool {
		if !evs[a].at.Equal(evs[b].at) {
			return evs[a].at.Before(evs[b].at)
		}
		return evs[a].i < evs[b].i
	})
	set := map[string]bool{}
	for _, e := range evs {
		if !t.IsZero() && e.at.After(t) {
			break
		}
		if e.m.in {
			set[e.m.name] = true
		} else {
			delete(set, e.m.name)
		}
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func rvNamesAt(t *testing.T, d *Drive, p string, at time.Time) string {
	n, err := d.resolve(system(), d.fs, drive.ByPath(p))
	if err != nil {
		t.Fatal(err)
	}
	fsys := d.fs
	if !at.IsZero() {
		if fsys, err = d.at(system(), drive.At{Time: at}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := fsys.ReadDir(system(), n.key)
	if err != nil {
		return "(" + errShort(err) + ")"
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func rvCutDates() []time.Time {
	var out []time.Time
	for m := 0; m < 20; m++ {
		out = append(out, rvMonth(2011, 1+m, 20))
	}
	for _, d := range []time.Time{rvMonth(2011, 3, 15), rvMonth(2011, 8, 15), rvMonth(2011, 10, 15), rvMonth(2011, 12, 1), rvMonth(2012, 6, 1)} {
		out = append(out, d, d.Add(-time.Second))
	}
	return out
}

// rvCheck: every view is the settled model (exact) or, if !exact, the
// settled or the unsettled one at that date (a real state).
func rvCheck(t *testing.T, d *Drive, mem []rvMem, label string, exact bool) int {
	bad := 0
	for _, at := range append(rvCutDates(), time.Time{}) {
		got := rvNamesAt(t, d, "/dir", at)
		after := rvState(mem, at, true)
		before := rvState(mem, at, false)
		if got == after || !exact && got == before {
			continue
		}
		if !exact && at.IsZero() {
			// The present during a cut window is a past state: any state.
			continue
		}
		bad++
		if bad <= 3 {
			t.Errorf("%s: /dir as of %s: %q, want %q (before the settle: %q)", label, fmtT(at), got, after, before)
		}
	}
	return bad
}

func rvSettleClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, drive.ErrSettleRefused):
		return "refused"
	case errors.Is(err, drive.ErrSettleStopped):
		return "stopped"
	case errors.Is(err, errRVCut):
		return "cut"
	}
	return "error"
}

func TestRV4Cut(t *testing.T) {
	if os.Getenv("RV4_CUT") == "" {
		t.Skip("RV4_CUT unset")
	}
	bucket := newBuckets(t)
	// Uncut: how many requests.
	st0 := newRVStore(bucket(t))
	d, rows, mem := rvCutBase(t, st0, "cut0")
	st0.take()
	if _, err := settleAt(d, system(), rows, (&keeper{}).opts()); err != nil {
		t.Fatal(err)
	}
	total, _, _, ops := st0.take()
	rvCheck(t, d, mem, "uncut", true)
	t.Logf("CUT uncut settle: %d requests %v", total, ops)

	var mu sync.Mutex
	outcomes := map[string]int{}
	note := func(k string) { mu.Lock(); outcomes[k]++; mu.Unlock() }
	variant := os.Getenv("RV4_CUT")
	for _, mode := range []string{"cut", "lost"} {
		for cut := 1; cut <= total+1; cut++ {
			t.Run(fmt.Sprintf("%s-%s-%d", variant, mode, cut), func(t *testing.T) {
				t.Parallel()
				st := newRVStore(bucket(t))
				name := fmt.Sprintf("c%s%d", mode[:1], cut)
				d, rows, mem := rvCutBase(t, st, name)
				first := rows
				if variant == "morerows" {
					first = rows[:2]
				}
				k := &keeper{}
				st.setFail(cut, mode)
				_, err := settleAt(d, system(), first, k.opts())
				st.setFail(0, "")
				if cut > total && variant != "morerows" && err != nil {
					t.Fatalf("not cut, and %v", err)
				}
				rvCheck(t, d, mem, "between the cut and the rerun", false)
				if variant == "newlate" && k.held() != nil {
					// The caller breaks the order: a late write into the folder
					// with a record open.
					late := &drive.LateChanges{}
					ctx := drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 5, 15))
					d2 := newDriveWith(t, st, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
					if _, err := d2.Write(ctx, "/dir/y.txt", strings.NewReader("y"), drive.Condition{}, drive.WriteOptions{}); err != nil {
						t.Fatalf("the late write with a record open: %v", err)
					}
					ys := late.Take()
					rows = append(rows, ys...)
					wd := rvMonth(2011, 5, 15)
					if len(ys) > 0 {
						wd = ys[0].Until
						t.Logf("y's row: until %s", wd.Format(time.RFC3339Nano))
					}
					mem = append(mem, rvMem{"y.txt", true, rvMonth(2011, 5, 15), wd})
				}
				// Reruns from new Drives; for some cuts the rerun itself cut.
				seconds := []int{0}
				if cut%5 == 2 {
					seconds = []int{1, total / 3, 2 * total / 3, 0}
				}
				var res []string
				for _, second := range seconds {
					d = newDriveWith(t, st, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
					st.setFail(second, mode)
					_, err = settleAt(d, system(), rows, k.opts())
					st.setFail(0, "")
					res = append(res, rvSettleClass(err))
					if second != 0 {
						rvCheck(t, d, mem, fmt.Sprintf("after a second cut at %d", second), false)
					}
				}
				if err != nil {
					note("rerun " + rvSettleClass(err))
					t.Errorf("the last rerun: %v (present %q)", err, rvNamesAt(t, d, "/dir", time.Time{}))
					return
				}
				if k.held() != nil {
					t.Errorf("record held after the rerun")
				}
				bad := rvCheck(t, d, mem, "after the rerun", true)
				rep, err := d.fs.Check(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range rep.Problems {
					t.Errorf("fs check: %+v", p)
				}
				if bad == 0 && len(rep.Problems) == 0 {
					note("right")
				} else {
					note("WRONG")
				}
				t.Logf("CUTCASE %s %s at %d: reruns %v, wrong views %d, unreachable %d", variant, mode, cut, res, bad, rep.Unreachable)
			})
		}
	}
	t.Cleanup(func() { t.Logf("CUT %s outcomes: %v", variant, outcomes) })
}

// TestRV4TwoSettlers: two drives settle the same rows of a one-block folder
// at once (two daemons during a deploy: outside the lock).
func TestRV4TwoSettlers(t *testing.T) {
	if os.Getenv("RV4_CUT") == "" {
		t.Skip("RV4_CUT unset")
	}
	bucket := newBuckets(t)
	outcomes := map[string]int{}
	for i := 0; i < rvEnvInt("RV4_TWO_N", 20); i++ {
		st := bucket(t)
		name := fmt.Sprintf("two%d", i)
		d := newDriveWith(t, st, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010)}, Config{DeferLateFolders: true})
		var mem []rvMem
		var rows []drive.LateChange
		for j := 0; j < 8; j++ {
			at := rvMonth(2011, 1+j, 1)
			if _, err := d.Write(drive.WithSourceTime(system(), at), fmt.Sprintf("/dir/f%d.txt", j), strings.NewReader("f"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
				t.Fatal(err)
			}
			mem = append(mem, rvMem{fmt.Sprintf("f%d.txt", j), true, at, at})
		}
		for j := 0; j < 3; j++ {
			late := &drive.LateChanges{}
			at := rvMonth(2011, 2+j, 10)
			if _, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), at), fmt.Sprintf("/dir/x%d.txt", j), strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			rs := late.Take()
			rows = append(rows, rs...)
			mem = append(mem, rvMem{fmt.Sprintf("x%d.txt", j), true, at, rs[0].Until})
		}
		a := newDriveWith(t, st, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
		b := newDriveWith(t, st, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for k, dd := range []*Drive{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[k] = settleAt(dd, system(), rows, (&keeper{}).opts())
			}()
		}
		wg.Wait()
		c := newDriveWith(t, st, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
		bad := 0
		for _, at := range append(rvCutDates(), time.Time{}) {
			if got, want := rvNamesAt(t, c, "/dir", at), rvState(mem, at, true); got != want {
				bad++
				if bad <= 3 {
					t.Logf("TWO %d wrong: as of %s: %q, want %q", i, fmtT(at), got, want)
				}
			}
		}
		rep, _ := c.fs.Check(context.Background())
		key := fmt.Sprintf("%s+%s wrong=%v problems=%d", rvSettleClass(errs[0]), rvSettleClass(errs[1]), bad > 0, len(rep.Problems))
		outcomes[key]++
		t.Logf("TWO %d: %s (%v; %v)", i, key, errShort(errs[0]), errShort(errs[1]))
	}
	t.Logf("TWO outcomes: %v", outcomes)
}
