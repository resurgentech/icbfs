package dedupe_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/dedupe"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
)

const minSize = 100

// tree is the filesystem the tests scan.
type tree struct {
	t     *testing.T
	ctx   context.Context
	store objstore.Store
	f     *icbfs.Filesystem
	keys  map[string]string // path -> item key
	dirs  map[string]string // dir path -> key
}

func newTree(t *testing.T) *tree {
	t.Helper()
	store := miniotest.Store(t)
	ctx := context.Background()
	if _, err := icbfs.CreateFilesystem(ctx, store, icbfs.DefaultSettings("t")); err != nil {
		t.Fatal(err)
	}
	f := icbfs.New(store, "t")
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	return &tree{t: t, ctx: ctx, store: store, f: f, keys: map[string]string{}, dirs: map[string]string{"/": f.RootKey()}}
}

// write puts data as a new content version of the file at path (creating
// it and its directory), as a mount does: no checksums recorded.
func (x *tree) write(path, data string) *objstore.Object {
	x.t.Helper()
	i := strings.LastIndex(path, "/")
	dir, name := path[:i], path[i+1:]
	if dir == "" {
		dir = "/"
	}
	if _, ok := x.dirs[dir]; !ok {
		key, _, err := x.f.Mkdir(x.ctx, x.f.RootKey(), dir[1:], 0755, 0, 0)
		if err != nil {
			x.t.Fatal(err)
		}
		x.dirs[dir] = key
	}
	key, ok := x.keys[path]
	if !ok {
		var err error
		if key, _, _, err = x.f.Create(x.ctx, x.dirs[dir], name, 0644, 0, 0); err != nil {
			x.t.Fatal(err)
		}
		x.keys[path] = key
	}
	obj, err := x.f.WriteFile(x.ctx, key, []byte(data), "")
	if err != nil {
		x.t.Fatal(err)
	}
	return obj
}

// record has the filesystem record the current version's checksums.
func (x *tree) record(path string) {
	x.t.Helper()
	if _, err := x.f.VersionChecksums(x.ctx, x.keys[path], ""); err != nil {
		x.t.Fatal(err)
	}
}

func body(c byte, n int) string { return strings.Repeat(string(c), n) }

// fixture builds, oldest first (the sleep keeps /b/twin a store second
// after /a/orig):
//
//	/a/orig    X                       X is 1000 bytes
//	/b/twin    X, recorded; the stub's target
//	/c/hist    Y, Y, Z                 Y is 300 bytes, Z 400
//	/d/mount   W, written bare         W is 500 bytes
//	/d/mount2  W, recorded
//	/e/stub    "placeholder", then a stub naming /b/twin's X
//	/f/tiny1, /f/tiny2   "tiny" (below the minimum, 100)
func fixture(t *testing.T) (*tree, *objstore.Object) {
	x := newTree(t)
	X := body('x', 1000)
	x.write("/a/orig", X)
	time.Sleep(1100 * time.Millisecond)
	twin := x.write("/b/twin", X)
	x.record("/b/twin")
	x.write("/c/hist", body('y', 300))
	x.write("/c/hist", body('y', 300))
	x.write("/c/hist", body('z', 400))
	x.write("/d/mount", body('w', 500))
	x.write("/d/mount2", body('w', 500))
	x.record("/d/mount2")
	x.write("/e/stub", "placeholder")
	if _, err := x.f.PutRef(x.ctx, x.keys["/e/stub"], x.keys["/b/twin"], twin.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	x.write("/f/tiny1", "tiny")
	x.write("/f/tiny2", "tiny")
	return x, twin
}

func scan(t *testing.T, x *tree, o dedupe.Options) *dedupe.Result {
	t.Helper()
	if o.MinSize == 0 {
		o.MinSize = minSize
	}
	r, err := dedupe.Scan(x.ctx, x.store, x.f, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func byPath(r *dedupe.Result, path string) []dedupe.Record {
	var out []dedupe.Record
	for _, rec := range r.Records {
		if rec.Path == path {
			out = append(out, rec)
		}
	}
	return out
}

// groupOf is the history group of size-byte content.
func groupOf(t *testing.T, r *dedupe.Result, size int64) dedupe.Group {
	t.Helper()
	for _, g := range r.History {
		if r.Records[g.Canonical].Size == size {
			return g
		}
	}
	t.Fatalf("no history group of %d-byte content", size)
	return dedupe.Group{}
}

// TestDump: one line per content version of every file in scope (those
// of the minimum size or over), or with Current only the current ones;
// the fields, the recorded-or-computed flags, a stub's ref; a second run
// computes nothing.
func TestDump(t *testing.T) {
	x, twin := fixture(t)

	r := scan(t, x, dedupe.Options{})
	var buf bytes.Buffer
	if err := dedupe.WriteDump(&buf, r.Records); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 8 { // orig, twin, hist x3, mount, mount2, stub
		t.Fatalf("dump has %d lines, want 8:\n%s", len(lines), buf.String())
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		for _, k := range []string{"path", "itemId", "versionId", "etag", "date", "size", "md5", "md5_computed", "sha256", "sha256_computed"} {
			if _, ok := m[k]; !ok {
				t.Errorf("a dump line lacks %q: %s", k, l)
			}
		}
	}

	// A bare version: computed now, and right.
	mount := byPath(r, "/d/mount")
	if len(mount) != 1 {
		t.Fatalf("/d/mount: %d records", len(mount))
	}
	want, _ := icbfs.ChecksumsOf(strings.NewReader(body('w', 500)))
	m := mount[0]
	if m.SHA256 != hex.EncodeToString(want.SHA256) || m.MD5 != hex.EncodeToString(want.MD5) || !m.SHA256Computed || !m.MD5Computed || m.Size != 500 || m.ItemID != x.keys["/d/mount"] || m.Ref != nil {
		t.Errorf("/d/mount: %+v, want sha256 %x md5 %x computed, 500 bytes", m, want.SHA256, want.MD5)
	}
	if m.ETag == "" || m.VersionID == "" || m.Date.IsZero() {
		t.Errorf("/d/mount: no etag, version id or date: %+v", m)
	}
	// A recorded one: not computed.
	if m2 := byPath(r, "/d/mount2")[0]; m2.SHA256Computed || m2.MD5Computed || m2.SHA256 != m.SHA256 {
		t.Errorf("/d/mount2: %+v, want recorded", m2)
	}
	// The stub: its target's bytes' checksum and size, and where it points.
	stub := byPath(r, "/e/stub")
	if len(stub) != 1 || stub[0].Ref == nil || stub[0].Ref.Key != x.keys["/b/twin"] || stub[0].Ref.Version != twin.VersionID || stub[0].Size != 1000 {
		t.Fatalf("/e/stub: %+v, want a ref to /b/twin version %s, 1000 bytes", stub, twin.VersionID)
	}
	if orig := byPath(r, "/a/orig")[0]; stub[0].SHA256 != orig.SHA256 {
		t.Errorf("stub sha256 %s, want its target's %s", stub[0].SHA256, orig.SHA256)
	}
	if !strings.Contains(buf.String(), `"ref":{"key":"`+x.keys["/b/twin"]+`","version":"`+twin.VersionID+`"}`) {
		t.Errorf("the stub's line has no ref {key, version}:\n%s", buf.String())
	}
	if strings.Count(buf.String(), `"ref"`) != 1 {
		t.Errorf("only the stub's line has a ref:\n%s", buf.String())
	}
	// Three versions of /c/hist, newest first by date.
	h := byPath(r, "/c/hist")
	if len(h) != 3 || h[0].Size != 400 || h[1].SHA256 != h[2].SHA256 {
		t.Errorf("/c/hist: %+v", h)
	}
	if len(h) == 3 && (!h[0].Current || h[1].Current || h[2].Current) {
		t.Errorf("/c/hist: only the newest is current: %+v", h)
	}
	// Computed: orig, hist x3, mount (twin, mount2 and the stub were recorded).
	if r.Report.Computed != 5 {
		t.Errorf("computed %d versions, want 5", r.Report.Computed)
	}

	// The second run computes none: they were recorded.
	r2 := scan(t, x, dedupe.Options{})
	if r2.Report.Computed != 0 {
		t.Errorf("second run computed %d, want 0", r2.Report.Computed)
	}
	for _, rec := range r2.Records {
		if rec.SHA256Computed || rec.MD5Computed {
			t.Errorf("second run: %s computed: %+v", rec.Path, rec)
		}
	}

	// Current only.
	rc := scan(t, x, dedupe.Options{Current: true})
	buf.Reset()
	if err := dedupe.WriteDump(&buf, rc.Records); err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimSpace(buf.String()), "\n")); n != 6 {
		t.Errorf("current-only dump has %d lines, want 6:\n%s", n, buf.String())
	}
	if rc.Report.History != nil {
		t.Errorf("a current-only scan reports history: %+v", rc.Report.History)
	}
}

// TestDryRunNumbers: groups, duplicate versions, reclaimable bytes in
// current-only and history mode, and what fell below the minimum.
func TestDryRunNumbers(t *testing.T) {
	x, _ := fixture(t)
	r := scan(t, x, dedupe.Options{})
	rep := r.Report
	if rep.Files != 8 {
		t.Errorf("files %d, want 8", rep.Files)
	}
	cur := rep.Current
	// Current: X (orig, twin, and the stub), W (mount, mount2): 2 groups,
	// 2 duplicates; below the minimum: the two tiny files.
	if cur.Groups != 2 || cur.Duplicates != 2 || cur.Reclaimable != 1000+500 || cur.BelowMin.Versions != 2 || cur.BelowMin.Bytes != 8 || cur.Stubs != 1 {
		t.Errorf("current-only: %+v, want 2 groups, 2 duplicates, 1500 bytes, 2 below the minimum (8 bytes), 1 stub", cur)
	}
	h := rep.History
	if h == nil {
		t.Fatal("no history numbers")
	}
	// History: X, Y (hist's first two), W: 3 groups, 3 duplicates; below
	// the minimum: the 8 empty versions Create makes, the tiny files and
	// the stub file's first version.
	if h.Groups != 3 || h.Duplicates != 3 || h.Reclaimable != 1000+300+500 || h.BelowMin.Versions != 8+2+1 || h.BelowMin.Bytes != 8+11 || h.Stubs != 1 {
		t.Errorf("history: %+v, want 3 groups, 3 duplicates, 1800 bytes, 11 below the minimum (19 bytes), 1 stub", h)
	}
	// A higher minimum drops the 300- to 500-byte versions.
	r = scan(t, x, dedupe.Options{MinSize: 600})
	h = r.Report.History
	if h.Groups != 1 || h.Duplicates != 1 || h.Reclaimable != 1000 || h.BelowMin.Versions != 16 || h.BelowMin.Bytes != 2019 {
		t.Errorf("minimum 600: %+v, want 1 group of 1000 bytes, 16 below (8 empty, tiny x2, the placeholder, hist x3, mount, mount2: 2019 bytes)", h)
	}
}

// TestCanonical: the oldest version by date is canonical, unless another
// copy already has referrers; that one stays.
func TestCanonical(t *testing.T) {
	x, twin := fixture(t)
	r := scan(t, x, dedupe.Options{})

	// /a/orig is older, but the stub names /b/twin's version.
	g := groupOf(t, r, 1000)
	can := r.Records[g.Canonical]
	if can.Path != "/b/twin" || can.VersionID != twin.VersionID || can.Referrers != 1 {
		t.Errorf("canonical %s %s (%d referrers), want /b/twin with 1", can.Path, can.VersionID, can.Referrers)
	}
	if len(g.Duplicates) != 1 || r.Records[g.Duplicates[0]].Path != "/a/orig" {
		t.Errorf("duplicates %v, want /a/orig", g.Duplicates)
	}

	// No referrers: the oldest. /d/mount was written before /d/mount2.
	g = groupOf(t, r, 500)
	if c := r.Records[g.Canonical]; c.Path != "/d/mount" || c.Referrers != 0 {
		t.Errorf("W canonical %s (%d referrers), want /d/mount with none", c.Path, c.Referrers)
	}

	// Y: hist's older identical version.
	h := byPath(r, "/c/hist") // newest first
	g = groupOf(t, r, 300)
	if c := r.Records[g.Canonical]; c.VersionID != h[2].VersionID || len(g.Duplicates) != 1 || r.Records[g.Duplicates[0]].VersionID != h[1].VersionID {
		t.Errorf("Y canonical %s, duplicates %v; want the oldest, %s, and %s", c.VersionID, g.Duplicates, h[2].VersionID, h[1].VersionID)
	}

	// Current-only: the same content, groups of current versions only.
	rc := scan(t, x, dedupe.Options{Current: true})
	if len(rc.Current) != 2 {
		t.Fatalf("current-only groups: %d, want 2", len(rc.Current))
	}
	for _, g := range rc.Current {
		if rc.Records[g.Canonical].Size == 1000 && rc.Records[g.Canonical].Path != "/b/twin" {
			t.Errorf("current-only canonical %s, want /b/twin", rc.Records[g.Canonical].Path)
		}
	}
}

// TestSubtree: a path limits the scan to that subtree.
func TestSubtree(t *testing.T) {
	x, _ := fixture(t)
	r := scan(t, x, dedupe.Options{Path: "/d"})
	if r.Report.Files != 2 || len(r.Records) != 2 || r.Report.History.Groups != 1 || r.Report.History.Reclaimable != 500 {
		t.Errorf("/d: %+v, %d records", r.Report, len(r.Records))
	}
	r = scan(t, x, dedupe.Options{Path: "/a/orig"})
	if r.Report.Files != 1 || len(r.Records) != 1 {
		t.Errorf("/a/orig: %+v", r.Report)
	}
	if _, err := dedupe.Scan(x.ctx, x.store, x.f, dedupe.Options{Path: "/nothing", MinSize: 1}); err == nil {
		t.Error("a path that isn't there scanned")
	}
}

// TestDryRunWritesNothing: but for the checksums a first scan records, a
// scan changes no object: the same listing before and after a rescan.
func TestDryRunWritesNothing(t *testing.T) {
	x, _ := fixture(t)
	scan(t, x, dedupe.Options{})
	before, err := x.store.HistoryUnder(x.ctx, x.f.Prefix())
	if err != nil {
		t.Fatal(err)
	}
	scan(t, x, dedupe.Options{})
	after, err := x.store.HistoryUnder(x.ctx, x.f.Prefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("a rescan changed the store: %d versions before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i].VersionID != after[i].VersionID {
			t.Fatalf("a rescan changed %s", before[i].Key)
		}
	}
}

// TestRecordKeepsDates: recording a computed checksum doesn't move the
// dates of a source-time filesystem's history.
func TestRecordKeepsDates(t *testing.T) {
	x := newTree(t)
	if err := x.f.MarkSourceTime(x.ctx); err != nil {
		t.Fatal(err)
	}
	dated := func(y int, change func(ctx context.Context)) {
		t.Helper()
		rec := &objstore.Recorder{}
		dctx := objstore.WithRecorder(objstore.WithDeclaredTime(x.ctx, time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)), rec)
		change(dctx)
		if _, err := x.f.RecordDates(dctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	var key string
	dated(2021, func(ctx context.Context) {
		var err error
		if key, _, _, err = x.f.Create(ctx, x.f.RootKey(), "old", 0644, 0, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := x.f.WriteFile(ctx, key, []byte(body('o', 200)), ""); err != nil {
			t.Fatal(err)
		}
	})
	dated(2023, func(ctx context.Context) {
		if _, err := x.f.WriteFile(ctx, key, []byte(body('p', 200)), ""); err != nil {
			t.Fatal(err)
		}
	})
	mk := key + ".metadata"
	datesOf := func(k string) []time.Time {
		t.Helper()
		hist, err := x.store.History(x.ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		d, err := objstore.ReadDates(x.ctx, x.store, objstore.BaseKey(k))
		if err != nil {
			t.Fatal(err)
		}
		var out []time.Time
		for i := range hist {
			out = append(out, d.Time(&hist[i]))
		}
		return out
	}
	fileBefore, metaBefore := datesOf(key), datesOf(mk)

	r := scan(t, x, dedupe.Options{MinSize: 1})
	if r.Report.Computed != 2 {
		t.Fatalf("computed %d, want 2", r.Report.Computed)
	}
	if got := datesOf(key); !equalTimes(got, fileBefore) {
		t.Errorf("the file's dates moved: %v -> %v", fileBefore, got)
	}
	got := datesOf(mk)
	if len(got) <= len(metaBefore) {
		t.Fatalf(".metadata has %d versions, recording wrote none", len(got))
	}
	// Each new .metadata version keeps the date of the one it replaced.
	for _, d := range got {
		if d.Year() > 2023 {
			t.Errorf(".metadata version dated %v: recording moved its date", d)
		}
	}
	h, _, err := x.f.FileHistory(x.ctx, key)
	if err != nil || len(h) != 3 || h[0].At.Year() != 2023 || h[1].At.Year() != 2021 || h[2].At.Year() != 2021 { // Create's empty first version too
		t.Errorf("history %+v, %v", h, err)
	}
	if r.Records[0].Date.Year() != 2023 && r.Records[1].Date.Year() != 2023 {
		t.Errorf("records dated %v, %v: want the declared 2023 and 2021", r.Records[0].Date, r.Records[1].Date)
	}
}

func equalTimes(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}
