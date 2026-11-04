package dedupe_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/dedupe"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// versionBytes is each version's bytes of the file at path, newest first
// by date, as FileHistory lists them, with the history.
func (x *tree) versionBytes(path string) ([]icbfs.FileVersion, [][]byte) {
	x.t.Helper()
	key := x.keys[path]
	h, _, err := x.f.FileHistory(x.ctx, key)
	if err != nil {
		x.t.Fatal(err)
	}
	out := make([][]byte, len(h))
	for i, v := range h {
		body, _, err := icbfs.GetContentVersion(x.ctx, x.store, key, v.VersionID)
		if err != nil {
			x.t.Fatalf("%s version %s: %v", path, v.VersionID, err)
		}
		out[i], err = io.ReadAll(body)
		body.Close()
		if err != nil {
			x.t.Fatal(err)
		}
	}
	return h, out
}

// checkSameHistory checks that path's history is now as long as before,
// each version at the same date with the same bytes, and that the
// versions at the indexes stubs (newest first) are stubs naming
// targetPath (its key; "" for any), the others not.
func (x *tree) checkSameHistory(path string, beforeH []icbfs.FileVersion, beforeB [][]byte, targetPath string, stubs ...int) []icbfs.FileVersion {
	x.t.Helper()
	h, b := x.versionBytes(path)
	if len(h) != len(beforeH) {
		x.t.Fatalf("%s: %d versions, was %d:\n%+v\n%+v", path, len(h), len(beforeH), h, beforeH)
	}
	isStub := map[int]bool{}
	for _, i := range stubs {
		isStub[i] = true
	}
	for i := range h {
		// Of versions at one date, the older move back a nanosecond each
		// (ReplaceFileHistory takes strictly increasing dates).
		tied := i > 0 && beforeH[i].At.Equal(beforeH[i-1].At)
		if d := beforeH[i].At.Sub(h[i].At); !(d == 0 || tied && d > 0 && d < time.Microsecond) || h[i].Size != beforeH[i].Size {
			x.t.Errorf("%s version %d: %v %d bytes, was %v %d", path, i, h[i].At, h[i].Size, beforeH[i].At, beforeH[i].Size)
		}
		if !bytes.Equal(b[i], beforeB[i]) {
			x.t.Errorf("%s version %d: reads %d bytes, other than before (%d)", path, i, len(b[i]), len(beforeB[i]))
		}
		switch {
		case isStub[i] && h[i].Ref == nil:
			x.t.Fatalf("%s version %d: not a stub", path, i)
		case isStub[i] && targetPath != "" && h[i].Ref.Key != x.keys[targetPath]:
			x.t.Errorf("%s version %d: a stub naming %s, want %s", path, i, h[i].Ref.Key, targetPath)
		case !isStub[i] && h[i].Ref != nil && beforeH[i].Ref == nil:
			x.t.Errorf("%s version %d: a stub, want it kept", path, i)
		}
	}
	return h
}

func historyRun(t *testing.T, x *tree) *dedupe.Result {
	t.Helper()
	return replace(t, x, dedupe.Options{History: true})
}

// TestHistoryMiddle: a duplicate in the middle of a file's history
// becomes a stub naming the canonical copy; the history keeps its
// versions, dates, order and bytes; -past is written once; fsck is
// clean; a rerun writes nothing.
func TestHistoryMiddle(t *testing.T) {
	ft := newFaultTree(t)
	x := ft.tree
	X, W, V := body('x', 1000), body('w', 500), body('v', 700)
	aX := x.write("/a", X)
	x.write("/b", W)
	x.write("/b", X)
	x.write("/b", V)
	ah, ab := x.versionBytes("/a")
	bh, bb := x.versionBytes("/b")

	ft.fs.reset()
	r := historyRun(t, x)
	w := r.Report.Writes
	if w.Replaced.Versions != 1 || w.Replaced.Bytes != 1000 || len(w.Skipped) != 0 {
		t.Errorf("writes %+v, want /b's X replaced", w)
	}
	if n := pastWrites(ft.fs.noted()); n != 1 {
		t.Errorf("-past written %d times, want once", n)
	}
	h := x.checkSameHistory("/b", bh, bb, "/a", 1) // newest first: V, X, W, the create's empty one
	if h[1].Ref.VersionID != aX.VersionID {
		t.Errorf("/b's stub names version %s, want /a's %s", h[1].Ref.VersionID, aX.VersionID)
	}
	x.checkSameHistory("/a", ah, ab, "")
	if got := x.state("/a"); got.current.VersionID != aX.VersionID {
		t.Errorf("/a, the canonical copy's item, was rewritten")
	}
	x.checkClean()

	ft.fs.reset()
	r = historyRun(t, x)
	if got := ft.fs.noted(); len(got) != 0 {
		t.Errorf("a rerun wrote %v", got)
	}
	if w := r.Report.Writes; w.Replaced.Versions != 0 || w.Repointed != 0 {
		t.Errorf("rerun: %+v, want nothing", w)
	}
}

// TestHistoryConsecutive: identical consecutive versions: the newer
// becomes a stub at the older, in the file itself. A later run, after
// two more, rewrites the self-referencing file again; fsck is clean
// after each, and a rerun writes nothing.
func TestHistoryConsecutive(t *testing.T) {
	ft := newFaultTree(t)
	x := ft.tree
	X, Y := body('x', 1000), body('y', 800)
	x.write("/c", X)
	x.write("/c", X)
	bh, bb := x.versionBytes("/c")

	r := historyRun(t, x)
	if w := r.Report.Writes; w.Replaced.Versions != 1 || len(w.Skipped) != 0 {
		t.Errorf("writes %+v, want the newer X replaced", w)
	}
	h := x.checkSameHistory("/c", bh, bb, "/c", 0)
	if h[0].Ref.VersionID != h[1].VersionID {
		t.Errorf("/c's stub names %s, want its older version %s", h[0].Ref.VersionID, h[1].VersionID)
	}
	x.checkClean()

	x.write("/c", Y)
	x.write("/c", Y)
	bh, bb = x.versionBytes("/c")
	r = historyRun(t, x)
	if w := r.Report.Writes; w.Replaced.Versions != 1 || len(w.Skipped) != 0 {
		t.Errorf("second run: writes %+v, want the newer Y replaced", w)
	}
	h = x.checkSameHistory("/c", bh, bb, "/c", 0, 2)
	if h[0].Ref.VersionID != h[1].VersionID || h[2].Ref.VersionID != h[3].VersionID {
		t.Errorf("/c's stubs name %s and %s, want %s and %s", h[0].Ref.VersionID, h[2].Ref.VersionID, h[1].VersionID, h[3].VersionID)
	}
	x.checkClean()

	ft.fs.reset()
	historyRun(t, x)
	if got := ft.fs.noted(); len(got) != 0 {
		t.Errorf("a rerun wrote %v", got)
	}
}

// TestHistoryRepoint: an item with live referrers has them re-pointed to
// the canonical copy before it is rewritten: a referrer whose current
// version is the stub gets a new stub (the old one erased), one whose
// stub is in its past has its history rewritten; the old entries leave
// the item's .refs.
func TestHistoryRepoint(t *testing.T) {
	x := newTree(t)
	X, Z := body('x', 1000), body('z', 600)
	cX := x.write("/c", X) // the canonical copy: its item is the oldest
	tX := x.write("/t", X)
	x.write("/t", Z)
	x.write("/t", Z) // /t has a duplicate of its own, so it is rewritten after /c
	x.write("/r1", "placeholder")
	if _, err := x.f.PutRef(x.ctx, x.keys["/r1"], x.keys["/t"], tX.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	x.write("/r2", "placeholder")
	if _, err := x.f.PutRef(x.ctx, x.keys["/r2"], x.keys["/t"], tX.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	x.write("/r2", "r2 now") // its stub is in its past
	r1Stub := currentVersion(t, x, "/r1")
	r1h, r1b := x.versionBytes("/r1")
	r2h, r2b := x.versionBytes("/r2")
	th, tb := x.versionBytes("/t")

	r := historyRun(t, x)
	w := r.Report.Writes
	if w.Repointed != 2 || w.Replaced.Versions != 2 || len(w.Skipped) != 0 {
		t.Errorf("writes %+v, want 2 re-pointed, /t's X and its newer Z replaced", w)
	}
	h := x.checkSameHistory("/r1", r1h, r1b, "/c", 0)
	if h[0].Ref.VersionID != cX.VersionID || h[0].VersionID == r1Stub {
		t.Errorf("/r1's current %+v; want a new stub naming /c's %s", h[0], cX.VersionID)
	}
	if _, err := x.store.HeadVersion(x.ctx, x.keys["/r1"], r1Stub); err == nil {
		t.Errorf("/r1's old stub is still there")
	}
	h = x.checkSameHistory("/r2", r2h, r2b, "/c", 1)
	if h[1].Ref.VersionID != cX.VersionID {
		t.Errorf("/r2's past stub names %s, want /c's %s", h[1].Ref.VersionID, cX.VersionID)
	}
	h = x.checkSameHistory("/t", th, tb, "", 0, 2) // Z (a stub at its older Z), Z, X (a stub at /c), the create's
	if h[2].Ref.Key != x.keys["/c"] || h[0].Ref.Key != x.keys["/t"] {
		t.Errorf("/t's stubs %+v, %+v", h[2].Ref, h[0].Ref)
	}
	live, err := x.f.LiveReferrers(x.ctx, x.keys["/t"])
	if err != nil || len(live) != 0 {
		t.Errorf("/t's referrers %+v, %v; want none but its own", live, err)
	}
	counts, _, err := x.f.ReferrerCounts(x.ctx, x.keys["/c"])
	if err != nil || counts[cX.VersionID] != 3 {
		t.Errorf("/c's .refs %v, %v; want /r1, /r2 and /t", counts, err)
	}
	x.checkClean()
}

// TestHistoryRetiredHolder: referrers of a retired holder are re-pointed
// to a live copy of its content; the last one re-pointed erases the
// holder. One whose content has no live copy is left.
func TestHistoryRetiredHolder(t *testing.T) {
	x := newTree(t)
	X, Q := body('x', 1000), body('q', 900)
	hX := x.write("/h/one", X)
	hQ := x.write("/h/two", Q)
	for _, p := range []struct{ path, target string }{{"/r1", "/h/one"}, {"/r2", "/h/one"}, {"/r3", "/h/two"}} {
		x.write(p.path, "placeholder")
		v := hX
		if p.target == "/h/two" {
			v = hQ
		}
		if _, err := x.f.PutRef(x.ctx, x.keys[p.path], x.keys[p.target], v.VersionID, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	x.write("/r2", "r2 now") // /r2's stub is in its past
	for _, n := range []string{"one", "two"} {
		if err := x.f.Purge(x.ctx, x.dirs["/h"], n, false); err != nil {
			t.Fatal(err)
		}
	}
	cX := x.write("/c", X) // the live copy
	if rep := x.checkClean(); rep.RetiredHolders != 2 {
		t.Fatalf("%d retired holders, want 2", rep.RetiredHolders)
	}

	r := historyRun(t, x)
	if w := r.Report.Writes; w.Repointed != 2 {
		t.Errorf("writes %+v, want /r1 and /r2 re-pointed", w)
	}
	for _, p := range []string{"/r1", "/r2"} {
		h, _ := x.versionBytes(p)
		found := false
		for _, v := range h {
			found = found || v.Ref != nil && v.Ref.Key == x.keys["/c"] && v.Ref.VersionID == cX.VersionID
		}
		if !found {
			t.Errorf("%s: %+v; want a stub naming /c", p, h)
		}
	}
	if rep := x.checkClean(); rep.RetiredHolders != 1 {
		t.Errorf("%d retired holders, want two's only (its content has no live copy)", rep.RetiredHolders)
	}
	if obj, err := x.store.Head(x.ctx, x.keys["/r3"]); err != nil || obj.Metadata[icbfs.MetaRefKey] != x.keys["/h/two"] {
		t.Errorf("/r3: %+v, %v; want it left naming the holder", obj, err)
	}
}

// TestHistoryOpen: a file open here is skipped and reported; once it is
// closed, a run rewrites it.
func TestHistoryOpen(t *testing.T) {
	x := newTree(t)
	X, V := body('x', 1000), body('v', 700)
	x.write("/a", X)
	x.write("/b", X)
	x.write("/b", V)
	bh, bb := x.versionBytes("/b")
	o, err := x.f.Open(x.ctx, x.keys["/b"])
	if err != nil {
		t.Fatal(err)
	}
	r := historyRun(t, x)
	if s := skipsOf(r); s["/b"] != dedupe.SkipOpen || r.Report.Writes.Replaced.Versions != 0 {
		t.Errorf("writes %+v, want /b skipped as open", r.Report.Writes)
	}
	x.checkSameHistory("/b", bh, bb, "")
	o.Close()
	r = historyRun(t, x)
	if w := r.Report.Writes; w.Replaced.Versions != 1 || len(w.Skipped) != 0 {
		t.Errorf("after the close: %+v, want /b's X replaced", w)
	}
	x.checkSameHistory("/b", bh, bb, "/a", 1)
	x.checkClean()
}

// TestHistorySkipsKeptReferrer: an item a stub names at a version that
// stays (it has no other copy) can't be rewritten: it is skipped, and
// reported, and nothing of it changes.
func TestHistorySkipsKeptReferrer(t *testing.T) {
	x := newTree(t)
	X, Y := body('x', 1000), body('y', 800)
	x.write("/a", X)
	bY := x.write("/b", Y)
	x.write("/b", X)
	x.write("/b", X) // a duplicate of its own: /b ranks after /a, which keeps X
	x.write("/r", "placeholder")
	if _, err := x.f.PutRef(x.ctx, x.keys["/r"], x.keys["/b"], bY.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	bh, bb := x.versionBytes("/b")
	r := historyRun(t, x)
	if s := skipsOf(r); s["/b"] != dedupe.SkipReferrers || r.Report.Writes.Replaced.Versions != 0 {
		t.Errorf("writes %+v, want /b skipped for its referrer", r.Report.Writes)
	}
	h := x.checkSameHistory("/b", bh, bb, "")
	for i := range h {
		if h[i].VersionID != bh[i].VersionID {
			t.Errorf("/b version %d is %s, was %s: rewritten", i, h[i].VersionID, bh[i].VersionID)
		}
	}
	x.checkClean()
}

// TestHistorySourceTime: in a filesystem written in a source's time, the
// rewrite keeps every version's date (its .version line).
func TestHistorySourceTime(t *testing.T) {
	x := newTree(t)
	if err := x.f.MarkSourceTime(x.ctx); err != nil {
		t.Fatal(err)
	}
	X, W := body('x', 1000), body('w', 500)
	dated := func(y int, path, data string) {
		t.Helper()
		rec := &objstore.Recorder{}
		dctx := objstore.WithRecorder(objstore.WithDeclaredTime(x.ctx, time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)), rec)
		key, ok := x.keys[path]
		if !ok {
			var err error
			if key, _, _, err = x.f.Create(dctx, x.f.RootKey(), path[1:], 0644, 0, 0); err != nil {
				t.Fatal(err)
			}
			x.keys[path] = key
		}
		if _, err := x.f.WriteFile(dctx, key, []byte(data), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := x.f.RecordDates(dctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	dated(2019, "/a", X)
	dated(2020, "/b", W)
	dated(2021, "/b", X)
	dated(2022, "/b", W)
	bh, bb := x.versionBytes("/b")
	if bh[1].At.Year() != 2021 {
		t.Fatalf("/b's history %+v", bh)
	}
	r := historyRun(t, x)
	if w := r.Report.Writes; w.Replaced.Versions != 2 || len(w.Skipped) != 0 {
		t.Errorf("writes %+v, want /b's X and its newer W replaced", w)
	}
	h := x.checkSameHistory("/b", bh, bb, "", 0, 1)
	if h[1].Ref.Key != x.keys["/a"] || h[0].Ref.Key != x.keys["/b"] || h[0].Ref.VersionID != h[2].VersionID {
		t.Errorf("/b's stubs %+v, %+v", h[1].Ref, h[0].Ref)
	}
	x.checkClean()
}

// TestHistoryRepointDropsEntry: re-pointing a referrer's current stub
// drops its entry from the retired holder it named, which, its last,
// erases the holder.
func TestHistoryRepointDropsEntry(t *testing.T) {
	x := newTree(t)
	X := body('x', 1000)
	hX := x.write("/h", X)
	x.write("/r", "placeholder")
	if _, err := x.f.PutRef(x.ctx, x.keys["/r"], x.keys["/h"], hX.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := x.f.Purge(x.ctx, x.f.RootKey(), "h", false); err != nil {
		t.Fatal(err)
	}
	x.write("/c", X)
	r := historyRun(t, x)
	if w := r.Report.Writes; w.Repointed != 1 || len(w.Skipped) != 0 {
		t.Errorf("writes %+v, want /r re-pointed", w)
	}
	if rep := x.checkClean(); rep.RetiredHolders != 0 {
		t.Errorf("%d retired holders left, want the holder erased", rep.RetiredHolders)
	}
}
