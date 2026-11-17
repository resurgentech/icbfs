package icbfsdrive

// The final review of T83-1 (2026-11-17, docs/reports/2026-11-17-T83-1-final-review.md
// in interloc), brought into the tree in round 7: its sweep of an
// immediate stop at every request (a named recipe, not ci), its ST-6
// rerun, and its probe of the icbfsat a version's store metadata carries.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// pastSpy sees the past-changed mark's puts, which failStore does not
// show its watch.
type pastSpy struct {
	objstore.Store
	mu    sync.Mutex
	armed bool
	puts  int
}

func (s *pastSpy) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if strings.HasSuffix(key, "-past") {
		s.mu.Lock()
		if s.armed {
			s.puts++
		}
		s.mu.Unlock()
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func (s *pastSpy) arm() {
	s.mu.Lock()
	s.armed, s.puts = true, 0
	s.mu.Unlock()
}

func (s *pastSpy) take() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = false
	return s.puts
}

// TestST9_ImmediateStopAtEveryRequest (ST-1, ST-9, ST-15, LF-17): the
// caller's context canceled at each request of a settle in turn. Before
// the record: no record, nothing written. From the record on: no request
// after the one in flight (the past-changed mark included), the error is
// context.Canceled, the record is held, and a rerun with it from a new
// Drive finishes the settle: the past right, nothing left behind.
// T83_SWEEP=1 runs it; FR_CASE=split runs splitCase (blocks of four) instead of cutCase;
// FR_STRIDE=n cancels at every n-th request only.
func TestST9_ImmediateStopAtEveryRequest(t *testing.T) {
	if os.Getenv("T83_SWEEP") == "" {
		t.Skip("a sweep, one folder per request: just test-settle-review (T83_SWEEP=1)")
	}
	spy := &pastSpy{Store: miniotest.Store(t)}
	store := &failStore{Store: spy}
	cfg := Config{DeferLateFolders: true}
	split := os.Getenv("FR_CASE") == "split"
	stride := 1
	if s, err := strconv.Atoi(os.Getenv("FR_STRIDE")); err == nil && s > 0 {
		stride = s
	}
	mk := func(name string) (*Drive, []drive.LateChange, map[int]string, icbfs.Settings) {
		if split {
			d, ch := splitCase(t, store, name, cfg)
			return d, ch, splitCaseAfter, icbfs.Settings{Name: name}
		}
		d, ch := cutCase(t, store, name, cfg)
		return d, ch, cutCaseSettled, cutCaseSettings(name)
	}
	// The uncut settle's request count.
	d, changes, _, _ := mk("fr9base")
	n := 0
	store.setWatch(func(string, string) error { n++; return nil })
	if _, err := d.SettleFolder(system(), changes, drive.SettleOptions{Path: "/dir"}); err != nil {
		t.Fatal(err)
	}
	store.setWatch(nil)
	total := n
	t.Logf("uncut: %d requests (past mark not counted)", total)
	failed := 0
	for k := 1; k <= total; k += stride {
		k := k
		t.Run(fmt.Sprintf("cancel at request %d of %d", k, total), func(t *testing.T) {
			defer func() {
				if t.Failed() {
					failed++
				}
			}()
			name := fmt.Sprintf("fr9%v%d", split, k)
			d, changes, after, settings := mk(name)
			folder := changes[0].Folder
			before := liveNames(t, d, folder)
			ctx, cancel := context.WithCancel(system())
			defer cancel()
			kp := &keeper{}
			opts := kp.opts()
			opts.Path = "/dir"
			recorded := false
			rec := opts.Record
			opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
			snap := versionSnapshot(t, spy.Store)
			i := 0
			hit, recordedAtHit := "", false
			var afterReq, writes []string
			store.setWatch(func(op, key string) error {
				i++
				if hit != "" {
					afterReq = append(afterReq, op+" "+key)
					return nil
				}
				if isWrite(op) {
					writes = append(writes, op+" "+key)
				}
				if i == k {
					hit, recordedAtHit = op+" "+key, recorded
					cancel()
					spy.arm()
				}
				return nil
			})
			_, err := d.SettleFolder(ctx, changes, opts)
			store.setWatch(nil)
			marks := spy.take()
			if hit == "" {
				t.Skipf("the settle made fewer than %d requests", k)
			}
			// In the reads (before the record) a read paired with the one in
			// flight (a block's .metadata with the block) may still be tried
			// with the canceled context: it reaches nothing (the final review's
			// three read-phase points). Never a write, and nothing after the
			// record.
			readsOnly := !recordedAtHit
			for _, r := range afterReq {
				readsOnly = readsOnly && !isWrite(strings.Fields(r)[0])
			}
			if len(afterReq) > 0 && !readsOnly || marks > 0 {
				t.Errorf("canceled at %s: %d requests after it %v, %d past marks", hit, len(afterReq), afterReq, marks)
			} else if len(afterReq) > 0 {
				t.Logf("canceled in the reads at %s: %d read(s) tried after it with the canceled context: %v", hit, len(afterReq), afterReq)
			}
			if err == nil {
				// Canceled on its last request, after which it checks ctx no
				// more: the settle is done (Done told).
				if kp.done != 1 || kp.held() != nil {
					t.Errorf("canceled at %s: no error, done %d, held %v", hit, kp.done, kp.held() != nil)
				}
				pastIs(t, d, after)
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("canceled at %s: %v, not context.Canceled", hit, err)
			}
			if !recordedAtHit {
				if recorded || len(writes) > 0 {
					t.Errorf("canceled in its reads at %s: record %v, writes %v", hit, recorded, writes)
				}
				if got := versionSnapshot(t, spy.Store); got != snap {
					t.Errorf("canceled in its reads at %s: the store changed", hit)
				}
				return
			}
			if kp.held() == nil || kp.done != 0 {
				t.Fatalf("canceled at %s: record held %v, done %d", hit, kp.held() != nil, kp.done)
			}
			d = newDriveWith(t, store, settings, cfg)
			opts = kp.opts()
			opts.Path = "/dir"
			spy.arm()
			if _, err := d.SettleFolder(system(), changes, opts); err != nil {
				t.Fatalf("canceled at %s; the rerun: %v", hit, err)
			}
			if spy.take() == 0 {
				t.Errorf("canceled at %s; the rerun put no past mark", hit)
			}
			if kp.held() != nil || kp.done != 1 {
				t.Errorf("canceled at %s; after the rerun: held %v, done %d", hit, kp.held() != nil, kp.done)
			}
			if got := liveNames(t, d, folder); got != before {
				t.Errorf("canceled at %s; the folder now %s, want %s", hit, got, before)
			}
			pastIs(t, d, after)
			settleLeftovers(t, d, folder, 1<<20)
		})
	}
	t.Logf("%d cancel points failed", failed)
}

// TestST6_RerunAfterAnUndatedWriteInTheWindow (ST-6 then LF-19): an
// undated outside write lands inside the window (before its third put);
// the settle stops with its record held; the rerun with the record. The
// doc: "the settle stops without overwriting".
func TestST6_RerunAfterAnUndatedWriteInTheWindow(t *testing.T) {
	raw := miniotest.Store(t)
	store := &failStore{Store: raw}
	name := "fr6"
	cfg := Config{DeferLateFolders: true}
	d, changes := cutCase(t, store, name, cfg)
	if _, err := d.Write(at(system(), 2024), "/other/o.txt", strings.NewReader("o"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
		t.Fatal(err)
	}
	folder := changes[0].Folder
	puts, moved := 0, false
	recorded := false
	kp := &keeper{}
	opts := kp.opts()
	opts.Path = "/dir"
	rec := opts.Record
	opts.Record = func(r drive.SettleRecord) error { recorded = true; return rec(r) }
	store.setWatch(func(op, key string) error {
		if recorded && !moved && op == "PUT" && (key == folder || key == folder+".metadata") {
			if puts++; puts == 3 {
				moved = true
				outsideMove(t, raw, name, time.Time{})
			}
		}
		return nil
	})
	_, err := d.SettleFolder(system(), changes, opts)
	store.setWatch(nil)
	if !moved || !errors.Is(err, drive.ErrSettleStopped) || kp.held() == nil {
		t.Fatalf("moved %v, %v, held %v", moved, err, kp.held() != nil)
	}
	d = newDriveWith(t, store, cutCaseSettings(name), cfg)
	opts = kp.opts()
	opts.Path = "/dir"
	_, err = d.SettleFolder(system(), changes, opts)
	dirNow, otherNow := liveNames(t, d, folder), sortedAt(t, d, "/other", time.Time{})
	t.Logf("the rerun: %v; /dir now %s; /other now %s", err, dirNow, otherNow)
	if err == nil && strings.Contains(dirNow, "a.txt") && strings.Contains(otherNow, "a.txt") {
		t.Errorf("the rerun took the outside move's version for its own and erased it: a.txt is in /dir and in /other")
	}
}

// TestFR_Icbfsat (the probe on round 5's icbfsat): an ordinary write
// passes the version it read's store metadata on to its own new version
// (writeBack, putBlock). The folder's root block, its .metadata and the
// leaf a write changed must carry that write's own declared time, or none
// if it had none: never an earlier write's. Then a write killed after its
// root put, before its dating, is listed by the next write at its own
// date.
func TestFR_Icbfsat(t *testing.T) {
	raw := miniotest.Store(t)
	store := &failStore{Store: raw}
	settings := icbfs.Settings{Name: "frat", Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: 4}
	d := newDriveWith(t, store, settings, Config{DeferLateFolders: true})
	for i := 0; i < 6; i++ {
		r5Write(t, d, fmt.Sprintf("/dir/f%d.txt", i), rvMonth(2011, 1+i, 1))
	}
	n, err := d.resolve(system(), d.fs, drive.ByPath("/dir"))
	if err != nil {
		t.Fatal(err)
	}
	folder := n.key
	ctx := context.Background()
	// putAts is the icbfsat of the folder's root, .metadata and each block
	// below, current versions, by key.
	putAts := func() map[string]string {
		t.Helper()
		_, keys, err := d.fs.ReadDirKeys(system(), folder)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, k := range append([]string{folder, folder + ".metadata"}, keys...) {
			h, err := raw.Head(ctx, k)
			if err != nil {
				t.Fatal(err)
			}
			if v, err := raw.HeadVersion(ctx, k, h.VersionID); err == nil {
				h = v
			}
			if at, ok := objstore.PutAt(h); ok {
				out[k] = at.UTC().Format(time.RFC3339)
			} else {
				out[k] = "-"
			}
		}
		return out
	}
	changed := func(before, after map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range after {
			if before[k] != v {
				out[k] = v
			}
		}
		return out
	}
	p0 := putAts()
	t.Logf("after six dated writes: %v", p0)
	// An on-time dated write.
	x1 := rvMonth(2011, 10, 1)
	r5Write(t, d, "/dir/x1.txt", x1)
	p1 := putAts()
	for k, v := range changed(p0, p1) {
		if v != x1.Format(time.RFC3339) {
			t.Errorf("x1 (on time, %s): %s now carries icbfsat %s", x1.Format(time.RFC3339), k, v)
		}
	}
	if p1[folder] != x1.Format(time.RFC3339) || p1[folder+".metadata"] != x1.Format(time.RFC3339) {
		t.Errorf("x1: root %s, .metadata %s; want both %s", p1[folder], p1[folder+".metadata"], x1.Format(time.RFC3339))
	}
	// A late write: dated at the folder's newest state (x1's), not its own.
	rows := r5Write(t, d, "/dir/y.txt", rvMonth(2011, 2, 15))
	p2 := putAts()
	for k, v := range changed(p1, p2) {
		if v != x1.Format(time.RFC3339) {
			t.Errorf("y (late, Until %s): %s now carries icbfsat %s", x1.Format(time.RFC3339), k, v)
		}
	}
	if len(rows) == 0 || !rows[0].Until.Equal(x1) {
		t.Errorf("y's rows: %s", rowsUntil(rows))
	}
	// An undated write: what it writes carries no icbfsat, though it read
	// versions that did.
	if _, err := d.Write(system(), "/dir/u.txt", strings.NewReader("u"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	p3 := putAts()
	ch3 := changed(p2, p3)
	for k, v := range ch3 {
		if v != "-" {
			t.Errorf("u (undated): %s carries icbfsat %s, an earlier write's", k, v)
		}
	}
	if p3[folder] != "-" || p3[folder+".metadata"] != "-" {
		t.Errorf("u (undated): root %s, .metadata %s; want none", p3[folder], p3[folder+".metadata"])
	}
	t.Logf("u changed %v", ch3)
	// A dated write on top: u is listed at its own (store) time, so a view
	// between x1 and that time does not show it.
	x2 := rvMonth(2011, 11, 1)
	rows2 := r5Write(t, d, "/dir/x2.txt", x2)
	t.Logf("x2's rows (u's store second makes it late): %s", rowsUntil(rows2))
	if got := sortedAt(t, d, "/dir", rvMonth(2011, 10, 15)); strings.Contains(got, "u.txt") {
		t.Errorf("as of 2011-10-15: %s; u.txt, written undated in %d, dated at an earlier write's time", got, time.Now().Year())
	}
	// A dated write killed after its root block put, before its dating
	// (every request from the one after that put fails, the undo's too).
	x3 := rvMonth(2011, 12, 1)
	dates0, err := objstore.ReadDates(ctx, raw, folder)
	if err != nil {
		t.Fatal(err)
	}
	put := false
	store.setWatch(func(op, key string) error {
		if put {
			return errFail
		}
		if op == "PUT" && key == folder {
			put = true
		}
		return nil
	})
	late := &drive.LateChanges{}
	_, err = d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), x3), "/dir/x3.txt", strings.NewReader("x3"), drive.Condition{}, drive.WriteOptions{})
	store.setWatch(nil)
	if err == nil || !put {
		t.Fatalf("x3 not killed after its root put: %v", err)
	}
	h, err := raw.Head(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if hv, err := raw.HeadVersion(ctx, folder, h.VersionID); err == nil {
		h = hv
	}
	at3, ok := objstore.PutAt(h)
	t.Logf("x3's leftover root: icbfsat %v (%v); x3's rows %s", at3, ok, rowsUntil(late.Take()))
	// The next write lists the leftover at the date its own put carried.
	d = newDriveWith(t, store, icbfs.Settings{Name: "frat"}, Config{DeferLateFolders: true})
	r5Write(t, d, "/dir/x4.txt", rvMonth(2012, 1, 1))
	dates1, err := objstore.ReadDates(ctx, raw, folder)
	if err != nil {
		t.Fatal(err)
	}
	if _, was := dates0[h.VersionID]; was {
		t.Fatalf("the leftover was listed before")
	}
	got, listed := dates1[h.VersionID]
	t.Logf("the leftover root listed %v at %v", listed, got)
	if ok && listed && !got.Equal(at3) {
		t.Errorf("x3's leftover root listed at %v, its own put said %v", got, at3)
	}
}
