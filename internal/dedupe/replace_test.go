package dedupe_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/dedupe"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
)

// faultStore is a store that notes every write, and can run something
// before one (before) or fail it, writing nothing (fail): a kill just
// before that write, as far as the store is concerned.
type faultStore struct {
	objstore.Store
	mu     sync.Mutex
	writes []string
	before func(op, key string, meta map[string]string)
	fail   func(op, key string, meta map[string]string) bool
}

var errKilled = errors.New("killed")

func (s *faultStore) hit(op, key string, meta map[string]string) error {
	s.mu.Lock()
	s.writes = append(s.writes, op+" "+key)
	before, fail := s.before, s.fail
	s.mu.Unlock()
	if before != nil {
		before(op, key, meta)
	}
	if fail != nil && fail(op, key, meta) {
		return errKilled
	}
	return nil
}

// reset forgets the writes noted, and the faults.
func (s *faultStore) reset() {
	s.mu.Lock()
	s.writes, s.before, s.fail = nil, nil, nil
	s.mu.Unlock()
}

func (s *faultStore) noted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

func (s *faultStore) Put(ctx context.Context, key string, body io.Reader, meta map[string]string, ifMatch string) (*objstore.Object, error) {
	if err := s.hit("put", key, meta); err != nil {
		return nil, err
	}
	return s.Store.Put(ctx, key, body, meta, ifMatch)
}

func (s *faultStore) PutIfAbsent(ctx context.Context, key string, body io.Reader, meta map[string]string) (*objstore.Object, error) {
	if err := s.hit("put", key, meta); err != nil {
		return nil, err
	}
	return s.Store.PutIfAbsent(ctx, key, body, meta)
}

func (s *faultStore) PutParts(ctx context.Context, key string, parts []objstore.Part, meta map[string]string, ifMatch string) (*objstore.Object, error) {
	if err := s.hit("put", key, meta); err != nil {
		return nil, err
	}
	return s.Store.PutParts(ctx, key, parts, meta, ifMatch)
}

func (s *faultStore) Delete(ctx context.Context, key string) error {
	if err := s.hit("delete", key, nil); err != nil {
		return err
	}
	return s.Store.Delete(ctx, key)
}

func (s *faultStore) Undelete(ctx context.Context, key string) error {
	if err := s.hit("undelete", key, nil); err != nil {
		return err
	}
	return s.Store.Undelete(ctx, key)
}

func (s *faultStore) Erase(ctx context.Context, key, versionID string) error {
	if err := s.hit("erase", key, nil); err != nil {
		return err
	}
	return s.Store.Erase(ctx, key, versionID)
}

func (s *faultStore) EraseAll(ctx context.Context, prefix string) error {
	if err := s.hit("eraseall", prefix, nil); err != nil {
		return err
	}
	return s.Store.EraseAll(ctx, prefix)
}

// faultTree is a tree whose filesystem's store is a faultStore over raw.
type faultTree struct {
	*tree
	fs  *faultStore
	raw objstore.Store
}

func newFaultTree(t *testing.T) *faultTree {
	t.Helper()
	raw := miniotest.Store(t)
	ctx := context.Background()
	if _, err := icbfs.CreateFilesystem(ctx, raw, icbfs.DefaultSettings("t")); err != nil {
		t.Fatal(err)
	}
	fs := &faultStore{Store: raw}
	f := icbfs.New(fs, "t")
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	x := &tree{t: t, ctx: ctx, store: fs, f: f, keys: map[string]string{}, dirs: map[string]string{"/": f.RootKey()}}
	return &faultTree{tree: x, fs: fs, raw: raw}
}

func replace(t *testing.T, x *tree, o dedupe.Options) *dedupe.Result {
	t.Helper()
	r, err := tryReplace(x, o)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	return r
}

func tryReplace(x *tree, o dedupe.Options) (*dedupe.Result, error) {
	if o.MinSize == 0 {
		o.MinSize = minSize
	}
	return dedupe.Replace(x.ctx, x.store, x.f, o)
}

// state is what a file's history and content are, to compare before and
// after a replacement.
type state struct {
	history []icbfs.FileVersion
	data    []byte
	mtime   time.Time
	current *objstore.Object
}

func (x *tree) state(path string) state {
	x.t.Helper()
	key := x.keys[path]
	h, _, err := x.f.FileHistory(x.ctx, key)
	if err != nil {
		x.t.Fatal(err)
	}
	data, _, _, err := x.f.ReadFile(x.ctx, key)
	if err != nil {
		x.t.Fatal(err)
	}
	attr, err := x.f.Stat(x.ctx, key, icbfs.TypeFile)
	if err != nil {
		x.t.Fatal(err)
	}
	cur, err := x.store.Head(x.ctx, key)
	if err != nil {
		x.t.Fatal(err)
	}
	return state{history: h, data: data, mtime: attr.Mtime, current: cur}
}

// checkReplaced checks that path, whose state was before, is now the
// same history at the same dates, with its current version a stub naming
// targetPath's version targetVersion; the same bytes and mtime; the old
// version erased, the stub's checksums recorded, and the target's .refs
// entry for it finalized.
func (x *tree) checkReplaced(path string, before state, targetPath, targetVersion string) {
	x.t.Helper()
	after := x.state(path)
	if len(after.history) != len(before.history) {
		x.t.Fatalf("%s: %d versions after, %d before:\n%+v\n%+v", path, len(after.history), len(before.history), after.history, before.history)
	}
	for i := range before.history {
		b, a := before.history[i], after.history[i]
		if !a.At.Equal(b.At) || a.Size != b.Size {
			x.t.Errorf("%s version %d: %v %d bytes, was %v %d bytes", path, i, a.At, a.Size, b.At, b.Size)
		}
		if i > 0 && (a.Ref != nil || a.VersionID != b.VersionID) {
			x.t.Errorf("%s version %d: %+v, want it untouched (%s)", path, i, a, b.VersionID)
		}
	}
	cur := after.history[0]
	if cur.Ref == nil || cur.Ref.Key != x.keys[targetPath] || cur.Ref.VersionID != targetVersion {
		x.t.Errorf("%s: current %+v, want a stub naming %s version %s", path, cur, targetPath, targetVersion)
	}
	if !icbfs.IsRef(after.current) || after.current.Metadata[icbfs.MetaRefKey] != x.keys[targetPath] {
		x.t.Errorf("%s: the store's current version isn't the stub: %+v", path, after.current)
	}
	if !bytes.Equal(after.data, before.data) {
		x.t.Errorf("%s: reads %d bytes %q..., was %q...", path, len(after.data), after.data[:min(10, len(after.data))], before.data[:min(10, len(before.data))])
	}
	if !after.mtime.Equal(before.mtime) {
		x.t.Errorf("%s: mtime %v, was %v", path, after.mtime, before.mtime)
	}
	if _, err := x.store.HeadVersion(x.ctx, x.keys[path], before.history[0].VersionID); !objstore.IsNotFound(err) {
		x.t.Errorf("%s: the old version %s is still there (%v)", path, before.history[0].VersionID, err)
	}
	attr, err := x.f.FileMetadata(x.ctx, x.keys[path])
	if err != nil {
		x.t.Fatal(err)
	}
	if c := attr.Checksums[cur.ETag]; !c.Complete() {
		x.t.Errorf("%s: no checksums recorded for the stub", path)
	}
	counts, pending, err := x.f.ReferrerCounts(x.ctx, x.keys[targetPath])
	if err != nil {
		x.t.Fatal(err)
	}
	if counts[targetVersion] == 0 || len(pending) != 0 {
		x.t.Errorf("%s's .refs: %v, pending %v; want an entry for %s, none pending", targetPath, counts, pending, targetVersion)
	}
}

// checkClean runs fsck: no problems.
func (x *tree) checkClean() icbfs.CheckReport {
	x.t.Helper()
	rep, err := x.f.Check(x.ctx)
	if err != nil {
		x.t.Fatal(err)
	}
	if !rep.OK() {
		x.t.Errorf("fsck: %+v", rep.Problems)
	}
	return rep
}

func skipsOf(r *dedupe.Result) map[string]string {
	out := map[string]string{}
	for _, s := range r.Report.Writes.Skipped {
		out[s.Path] = s.Reason
	}
	return out
}

func currentVersion(t *testing.T, x *tree, path string) string {
	t.Helper()
	obj, err := x.store.Head(x.ctx, x.keys[path])
	if err != nil {
		t.Fatal(err)
	}
	return obj.VersionID
}

func pastWrites(writes []string) int {
	n := 0
	for _, w := range writes {
		if strings.HasSuffix(w, "-past") {
			n++
		}
	}
	return n
}

// TestReplace: each current version that isn't canonical is replaced by
// a stub naming the canonical one: the same history at the same dates,
// one of them now a stub, the same bytes; the old version erased; -past
// written once; a stub already there skipped and reported; fsck clean.
// A rerun writes nothing.
func TestReplace(t *testing.T) {
	ft := newFaultTree(t)
	x := ft.tree
	X, W := body('x', 1000), body('w', 500)
	x.write("/a/orig", X)
	twin := x.write("/b/twin", X)
	x.write("/d/mount", W)
	x.write("/d/mount2", W)
	x.write("/e/stub", "placeholder")
	if _, err := x.f.PutRef(x.ctx, x.keys["/e/stub"], x.keys["/b/twin"], twin.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	x.write("/f/solo", body('s', 600))
	mount := currentVersion(t, x, "/d/mount")
	orig, mount2, solo := x.state("/a/orig"), x.state("/d/mount2"), x.state("/f/solo")

	ft.fs.reset()
	r := replace(t, x, dedupe.Options{})
	w := r.Report.Writes
	if w == nil {
		t.Fatal("no writes reported")
	}
	if w.Replaced.Versions != 2 || w.Replaced.Bytes != 1500 || w.Finished != 0 {
		t.Errorf("writes %+v, want 2 versions, 1500 bytes replaced", w)
	}
	if s := skipsOf(r); len(s) != 1 || s["/e/stub"] != dedupe.SkipStub {
		t.Errorf("skipped %+v, want /e/stub only, as a stub", w.Skipped)
	}
	if n := pastWrites(ft.fs.noted()); n != 1 {
		t.Errorf("-past written %d times, want once", n)
	}
	x.checkReplaced("/a/orig", orig, "/b/twin", twin.VersionID)
	x.checkReplaced("/d/mount2", mount2, "/d/mount", mount)
	if s := x.state("/f/solo"); s.current.VersionID != solo.current.VersionID {
		t.Errorf("/f/solo, the only copy, changed")
	}
	x.checkClean()

	// A rerun: nothing to do, nothing written.
	ft.fs.reset()
	r = replace(t, x, dedupe.Options{})
	if got := ft.fs.noted(); len(got) != 0 {
		t.Errorf("a rerun wrote %v", got)
	}
	if w := r.Report.Writes; w.Replaced.Versions != 0 || w.Finished != 0 || len(w.Skipped) != 3 {
		t.Errorf("rerun: %+v, want nothing replaced, the 3 stubs skipped", w)
	}
}

// TestReplaceSourceTime: in a filesystem written in a source's time, the
// stub is dated as the version it replaces was, the old version's line
// leaves the .version record, and the .metadata writes keep their dates.
func TestReplaceSourceTime(t *testing.T) {
	x := newTree(t)
	if err := x.f.MarkSourceTime(x.ctx); err != nil {
		t.Fatal(err)
	}
	X := body('x', 1000)
	dated := func(y int, path string) {
		t.Helper()
		rec := &objstore.Recorder{}
		dctx := objstore.WithRecorder(objstore.WithDeclaredTime(x.ctx, time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)), rec)
		key, _, _, err := x.f.Create(dctx, x.f.RootKey(), path[1:], 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		x.keys[path] = key
		if _, err := x.f.WriteFile(dctx, key, []byte(X), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := x.f.RecordDates(dctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	dated(2021, "/a")
	dated(2022, "/b")
	target := currentVersion(t, x, "/a")
	before := x.state("/b")
	if before.history[0].At.Year() != 2022 {
		t.Fatalf("/b dated %v", before.history[0].At)
	}

	r := replace(t, x, dedupe.Options{})
	if r.Report.Writes.Replaced.Versions != 1 {
		t.Fatalf("writes %+v, want 1 replaced", r.Report.Writes)
	}
	x.checkReplaced("/b", before, "/a", target)
	dates, err := objstore.ReadDates(x.ctx, x.store, x.keys["/b"])
	if err != nil {
		t.Fatal(err)
	}
	stub := currentVersion(t, x, "/b")
	if at, ok := dates[stub]; !ok || at.Year() != 2022 {
		t.Errorf("the stub dated %v (%v), want 2022", at, ok)
	}
	if _, ok := dates[before.history[0].VersionID]; ok {
		t.Errorf("the erased version's line is still in .version")
	}
	meta, err := x.store.History(x.ctx, x.keys["/b"]+".metadata")
	if err != nil {
		t.Fatal(err)
	}
	for i := range meta {
		if d := dates.Time(&meta[i]); d.Year() > 2022 {
			t.Errorf(".metadata version %s dated %v: a write moved its date", meta[i].VersionID, d)
		}
	}
	x.checkClean()
}

// TestReplaceSkipsReferenced: a duplicate whose current version stubs
// already name is left, and reported.
func TestReplaceSkipsReferenced(t *testing.T) {
	x := newTree(t)
	X := body('x', 1000)
	a, b := x.write("/a", X), x.write("/b", X)
	x.write("/c", X)
	for i, target := range []struct {
		path string
		v    *objstore.Object
	}{{"/a", a}, {"/a", a}, {"/b", b}} {
		p := "/r" + string(rune('1'+i))
		x.write(p, "placeholder")
		if _, err := x.f.PutRef(x.ctx, x.keys[p], x.keys[target.path], target.v.VersionID, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	bBefore, cBefore := x.state("/b"), x.state("/c")
	r := replace(t, x, dedupe.Options{})
	if s := skipsOf(r); s["/b"] != dedupe.SkipReferrers {
		t.Errorf("skipped %+v, want /b for its referrers", r.Report.Writes.Skipped)
	}
	if r.Report.Writes.Replaced.Versions != 1 {
		t.Errorf("writes %+v, want /c replaced", r.Report.Writes)
	}
	x.checkReplaced("/c", cBefore, "/a", a.VersionID)
	if s := x.state("/b"); s.current.VersionID != bBefore.current.VersionID || icbfs.IsRef(s.current) {
		t.Errorf("/b changed: %+v", s.current)
	}
	x.checkClean()
}

// TestReplaceReferencedSinceScan: a stub naming the duplicate's version
// written after the scan saw none still makes the write skip it.
func TestReplaceReferencedSinceScan(t *testing.T) {
	x := newTree(t)
	X := body('x', 1000)
	x.write("/a", X)
	b := x.write("/b", X)
	x.write("/r", "placeholder")
	defer dedupe.SetBeforeReplace(func(r dedupe.Record) {
		if r.Path == "/b" {
			if _, err := x.f.PutRef(x.ctx, x.keys["/r"], x.keys["/b"], b.VersionID, nil, ""); err != nil {
				t.Error(err)
			}
		}
	})()
	before := x.state("/b")
	r := replace(t, x, dedupe.Options{})
	if s := skipsOf(r); s["/b"] != dedupe.SkipReferrers {
		t.Errorf("skipped %+v, want /b for its referrers", r.Report.Writes.Skipped)
	}
	if s := x.state("/b"); s.current.VersionID != before.current.VersionID {
		t.Errorf("/b changed")
	}
	x.checkClean()
}

// TestReplaceConcurrentWriter: a write to the item after the scan (its
// version isn't current any more) or just before the stub (the If-Match
// is lost) makes the run skip it, the writer's version current, and
// leaves no entry in the target's .refs.
func TestReplaceConcurrentWriter(t *testing.T) {
	check := func(t *testing.T, x *tree, r *dedupe.Result, want string) {
		t.Helper()
		if s := skipsOf(r); s["/b"] != dedupe.SkipChanged {
			t.Errorf("skipped %+v, want /b as changed", r.Report.Writes.Skipped)
		}
		if r.Report.Writes.Replaced.Versions != 0 {
			t.Errorf("writes %+v, want none replaced", r.Report.Writes)
		}
		data, _, _, err := x.f.ReadFile(x.ctx, x.keys["/b"])
		if err != nil || string(data) != want {
			t.Errorf("/b reads %q, %v; want the writer's", data, err)
		}
		counts, pending, err := x.f.ReferrerCounts(x.ctx, x.keys["/a"])
		if err != nil || len(counts) != 0 || len(pending) != 0 {
			t.Errorf("/a's .refs: %v, %v, %v; want no entries", counts, pending, err)
		}
		x.checkClean()
	}
	t.Run("after the scan", func(t *testing.T) {
		x := newTree(t)
		X := body('x', 1000)
		x.write("/a", X)
		x.write("/b", X)
		defer dedupe.SetBeforeReplace(func(r dedupe.Record) {
			if r.Path == "/b" {
				x.write("/b", "the writer's")
			}
		})()
		check(t, x, replace(t, x, dedupe.Options{}), "the writer's")
	})
	t.Run("at the stub", func(t *testing.T) {
		ft := newFaultTree(t)
		x := ft.tree
		X := body('x', 1000)
		x.write("/a", X)
		x.write("/b", X)
		key := x.keys["/b"]
		ft.fs.before = func(op, k string, meta map[string]string) {
			if op == "put" && k == key && meta[icbfs.MetaRef] == "1" {
				if _, err := ft.raw.Put(x.ctx, key, strings.NewReader("the writer's"), nil, ""); err != nil {
					t.Error(err)
				}
			}
		}
		r := replace(t, x, dedupe.Options{})
		ft.fs.reset()
		check(t, x, r, "the writer's")
	})
}

// TestReplaceResume: a run killed at any point between its first write
// for an item and its last is finished by a rerun, to the state a run
// that wasn't killed leaves; and a run after that writes nothing.
func TestReplaceResume(t *testing.T) {
	for _, c := range []struct {
		name     string
		fail     func(a, b string, seen func(string) bool) func(op, key string, meta map[string]string) bool
		finished bool // the rerun finishes the replacement (else it redoes it)
	}{
		{"after the entry", func(a, b string, seen func(string) bool) func(string, string, map[string]string) bool {
			return func(op, key string, meta map[string]string) bool {
				return op == "put" && key == b && meta[icbfs.MetaRef] == "1"
			}
		}, false},
		{"after the stub", func(a, b string, seen func(string) bool) func(string, string, map[string]string) bool {
			return func(op, key string, meta map[string]string) bool {
				return op == "put" && key == b+".metadata" && seen("put "+b)
			}
		}, true},
		{"after the checksums", func(a, b string, seen func(string) bool) func(string, string, map[string]string) bool {
			return func(op, key string, meta map[string]string) bool { return op == "erase" && key == b }
		}, true},
		{"after the erase", func(a, b string, seen func(string) bool) func(string, string, map[string]string) bool {
			return func(op, key string, meta map[string]string) bool {
				return op == "put" && key == a+".refs" && seen("erase "+b)
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ft := newFaultTree(t)
			x := ft.tree
			X := body('x', 1000)
			target := x.write("/a", X)
			x.write("/b", X)
			before := x.state("/b")
			a, b := x.keys["/a"], x.keys["/b"]
			seen := func(w string) bool {
				for _, n := range ft.fs.noted() {
					if n == w {
						return true
					}
				}
				return false
			}
			ft.fs.reset()
			ft.fs.fail = c.fail(a, b, seen)
			r, err := tryReplace(x, dedupe.Options{})
			ft.fs.reset()
			if c.name == "after the erase" {
				// The entry's finalizing is best effort, as PutRef's: the
				// run goes on, and leaves the entry pending.
				if err != nil {
					t.Fatalf("replace: %v", err)
				}
				if _, pending, _ := x.f.ReferrerCounts(x.ctx, a); len(pending) != 1 {
					t.Fatalf("pending %v, want the entry left pending", pending)
				}
			} else if !errors.Is(err, errKilled) {
				t.Fatalf("replace: %v, %+v; want it killed", err, r)
			}

			r = replace(t, x, dedupe.Options{})
			w := r.Report.Writes
			if c.finished && (w.Finished != 1 || w.Replaced.Versions != 0) || !c.finished && (w.Finished != 0 || w.Replaced.Versions != 1) {
				t.Errorf("rerun: %+v, want finished %v", w, c.finished)
			}
			x.checkReplaced("/b", before, "/a", target.VersionID)
			x.checkClean()

			ft.fs.reset()
			replace(t, x, dedupe.Options{})
			if got := ft.fs.noted(); len(got) != 0 {
				t.Errorf("a run after the rerun wrote %v", got)
			}
		})
	}
}

// TestReplaceRetiredHolder: an entry a killed run left, in the .refs of
// a target purged since, goes through dropRef: the retired holder it
// alone kept is erased.
func TestReplaceRetiredHolder(t *testing.T) {
	ft := newFaultTree(t)
	x := ft.tree
	X := body('x', 1000)
	x.write("/a/one", X)
	x.write("/b/two", X)
	a, b := x.keys["/a/one"], x.keys["/b/two"]
	before := x.state("/b/two")
	ft.fs.reset()
	ft.fs.fail = func(op, key string, meta map[string]string) bool {
		return op == "put" && key == b && meta[icbfs.MetaRef] == "1"
	}
	if _, err := tryReplace(x, dedupe.Options{}); !errors.Is(err, errKilled) {
		t.Fatalf("replace: %v; want it killed", err)
	}
	ft.fs.reset()
	if err := x.f.Purge(x.ctx, x.dirs["/a"], "one", false); err != nil {
		t.Fatal(err)
	}
	rep, err := x.f.Check(x.ctx)
	if err != nil || rep.RetiredHolders != 1 {
		t.Fatalf("after the purge: %d retired holders, %v; want the pending entry to keep one", rep.RetiredHolders, err)
	}

	replace(t, x, dedupe.Options{})
	if rep := x.checkClean(); rep.RetiredHolders != 0 {
		t.Errorf("%d retired holders left, want the holder erased", rep.RetiredHolders)
	}
	left, err := x.store.HistoryUnder(x.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range left {
		if o.Key == a || o.Key == a+".refs" {
			t.Errorf("the holder's %s is left", o.Key)
		}
	}
	if s := x.state("/b/two"); s.current.VersionID != before.current.VersionID || !bytes.Equal(s.data, before.data) {
		t.Errorf("/b/two changed")
	}
}

// TestReplaceOpenDirty: a mount holding the item open, with an edit not
// yet flushed, while the run replaces it, flushes the edit onto the same
// bytes: a lost race (one part) or a lost base version (parts copied from
// the erased version) rebases onto the stub's target (T51).
func TestReplaceOpenDirty(t *testing.T) {
	for _, c := range []struct {
		name string
		size int
	}{{"one part", 1000}, {"parts", 9 << 20}} {
		t.Run(c.name, func(t *testing.T) {
			ft := newFaultTree(t)
			x := ft.tree
			X := body('x', c.size)
			x.write("/a", X)
			x.write("/b", X)
			mount := icbfs.New(ft.raw, "t")
			if err := mount.Attach(x.ctx); err != nil {
				t.Fatal(err)
			}
			o, err := mount.Open(x.ctx, x.keys["/b"])
			if err != nil {
				t.Fatal(err)
			}
			if err := o.WriteAt([]byte("Z"), 5); err != nil {
				t.Fatal(err)
			}
			r := replace(t, x, dedupe.Options{})
			if r.Report.Writes.Replaced.Versions != 1 {
				t.Fatalf("writes %+v, want /b replaced", r.Report.Writes)
			}
			if _, err := o.Flush(x.ctx); err != nil {
				t.Fatalf("flush: %v", err)
			}
			o.Close()
			want := []byte(X)
			want[5] = 'Z'
			data, _, _, err := x.f.ReadFile(x.ctx, x.keys["/b"])
			if err != nil || !bytes.Equal(data, want) {
				t.Fatalf("after the flush: %d bytes %q..., %v", len(data), data[:min(10, len(data))], err)
			}
			x.checkClean()
		})
	}
}
