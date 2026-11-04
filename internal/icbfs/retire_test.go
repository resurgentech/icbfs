package icbfs

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// retireFixture is a filesystem with a target file of three content
// versions (its create's empty one, then one and two), a file a whose
// content is a stub naming one, and a file b created as a stub naming two.
type retireFixture struct {
	f          *Filesystem
	store      objstore.Store
	target     string
	v0, v1, v2 string
	one, two   []byte
	a, b       string
	aStub      *objstore.Object
	bStub      *objstore.Object
}

func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	ctx := context.Background()
	f, store := newTestFilesystem(t)
	x := &retireFixture{f: f, store: store,
		one: []byte(strings.Repeat("version one of the target. ", 50)),
		two: []byte(strings.Repeat("version two of the target! ", 50))}
	root := f.RootKey()
	x.target = writeNew(t, ctx, f, root, "target", string(x.one))
	if _, err := f.WriteFile(ctx, x.target, x.two, ""); err != nil {
		t.Fatal(err)
	}
	h, err := store.History(ctx, x.target)
	if err != nil || len(h) != 3 {
		t.Fatalf("target history %v, %v; want 3 versions", h, err)
	}
	x.v2, x.v1, x.v0 = h[0].VersionID, h[1].VersionID, h[2].VersionID
	x.a = writeNew(t, ctx, f, root, "a", "a's own")
	if x.aStub, err = f.PutRef(ctx, x.a, x.target, x.v1, nil, ""); err != nil {
		t.Fatalf("put ref a: %v", err)
	}
	if x.b, _, err = f.CreateWithRef(ctx, root, "b", 0644, 0, 0, nil, ItemInfo{}, x.target, x.v2, nil); err != nil {
		t.Fatalf("create with ref b: %v", err)
	}
	if x.bStub, err = store.Head(ctx, x.b); err != nil {
		t.Fatal(err)
	}
	return x
}

func referrerPaths(r RetiredItem) []string {
	var out []string
	for _, ref := range r.Referrers {
		out = append(out, ref.Path)
	}
	sort.Strings(out)
	return out
}

// TestPurgeRetires: a purge (live, or from the trash) of an item with live
// referrers retires it: no name, no .metadata, no trash entry; the
// versions nobody references are erased; the referrers still read their
// bytes; a view as of before finds nothing at the name; fsck counts the
// holder and reports nothing.
func TestPurgeRetires(t *testing.T) {
	for _, fromTrash := range []bool{false, true} {
		name := "purge"
		if fromTrash {
			name = "purge from the trash"
		}
		t.Run(name, func(t *testing.T) {
			x := newRetireFixture(t)
			ctx := context.Background()
			f, root := x.f, x.f.RootKey()
			time.Sleep(1100 * time.Millisecond)
			at, err := x.store.ServerTime(ctx)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond)

			var rep PurgeReport
			if fromTrash {
				if err := f.Unlink(ctx, root, "target"); err != nil {
					t.Fatal(err)
				}
				trash, err := f.ListTrash(ctx, TrashOptions{})
				if err != nil || len(trash) != 1 {
					t.Fatalf("trash %v, %v", trash, err)
				}
				rep, err = f.PurgeTrashWithReport(ctx, trash[0].EntryID)
				if err != nil {
					t.Fatalf("purge trash: %v", err)
				}
			} else if rep, err = f.PurgeWithReport(ctx, root, "target", false); err != nil {
				t.Fatalf("purge: %v", err)
			}
			if len(rep.Retired) != 1 || rep.Retired[0].Key != x.target || rep.Retired[0].Name != "target" {
				t.Fatalf("report %+v; want the target retired", rep)
			}
			if got := referrerPaths(rep.Retired[0]); strings.Join(got, ",") != "/a,/b" {
				t.Fatalf("referrers by path %v, want /a and /b", got)
			}

			if _, _, err := f.Lookup(ctx, root, "target"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("lookup after the purge: %v", err)
			}
			for _, k := range []string{metadataObjectKey(x.target), x.target + objstore.VersionSuffix, x.target + ".lock", f.trashByKey(x.target), f.trashPrefix()} {
				if n := countVersions(t, ctx, f, k); n != 0 {
					t.Errorf("%s: %d versions left", k, n)
				}
			}
			if trash, _ := f.ListTrash(ctx, TrashOptions{}); len(trash) != 0 {
				t.Errorf("trash entries left: %+v", trash)
			}
			if _, err := x.store.Head(ctx, x.target); !objstore.IsNotFound(err) {
				t.Errorf("a plain Head of the holder: %v, want not found", err)
			}
			h, err := x.store.History(ctx, x.target)
			if err != nil {
				t.Fatal(err)
			}
			kept := map[string]bool{}
			for i, v := range h {
				if v.Deleted {
					if i != 0 {
						t.Errorf("a delete marker that isn't the newest: %+v", h)
					}
					continue
				}
				kept[v.VersionID] = true
			}
			if len(kept) != 2 || !kept[x.v1] || !kept[x.v2] || !h[0].Deleted {
				t.Fatalf("holder history %+v; want a delete marker over v1 and v2 only (v0 %s erased)", h, x.v0)
			}
			refs := readRefs(t, ctx, x.store, x.target)
			if refs == nil || !refs.Retired || len(refs.Referrers) != 2 {
				t.Fatalf(".refs %+v; want retired with two entries", refs)
			}
			if got := readName(t, ctx, f, root, "a"); got != string(x.one) {
				t.Errorf("a reads %d bytes, want version one", len(got))
			}
			if got := readName(t, ctx, f, root, "b"); got != string(x.two) {
				t.Errorf("b reads %d bytes, want version two", len(got))
			}

			hist, err := objstore.OpenTempHistoryCache(ctx, x.store, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer hist.Close()
			view, err := objstore.NewAsOf(ctx, hist, at)
			if err != nil {
				t.Fatal(err)
			}
			past := New(view, "test")
			if err := past.BootstrapReadOnly(ctx); err != nil {
				t.Fatal(err)
			}
			entries, err := past.ReadDir(ctx, past.RootKey())
			if err != nil {
				t.Fatal(err)
			}
			listed := false
			for _, e := range entries {
				listed = listed || e.Name == "target" && e.UUID == x.target
			}
			if !listed {
				t.Fatalf("as of before: the name is gone from the root's history: %+v", entries)
			}
			if _, _, err := past.Lookup(ctx, past.RootKey(), "target"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("as of before: opening the name: %v; want ErrNotFound", err)
			}
			if data, _, _, err := past.ReadFile(ctx, x.target); !errors.Is(err, ErrNotFound) {
				t.Fatalf("as of before: reading the item read %d bytes, %v; want ErrNotFound", len(data), err)
			}

			r, err := f.Check(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !r.OK() || r.Unreachable != 0 || r.RetiredHolders != 1 {
				t.Fatalf("check: problems %v, unreachable %v, holders %d", r.Problems, r.UnreachableListed, r.RetiredHolders)
			}
		})
	}
}

// TestPurgeTakesEveryReferrer: a purge whose items include every referrer
// erases the target entirely, .refs and all.
func TestPurgeTakesEveryReferrer(t *testing.T) {
	f, _ := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	d, _, err := f.Mkdir(ctx, root, "d", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Named to come first in the purge, while its referrers' stubs are
	// still there: their entries must go before it counts them.
	target := writeNew(t, ctx, f, d, "0target", strings.Repeat("bytes ", 100))
	tv, err := f.store.Head(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	a := writeNew(t, ctx, f, d, "a", "a")
	if _, err := f.PutRef(ctx, a, target, tv.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	b, _, err := f.CreateWithRef(ctx, d, "b", 0644, 0, 0, nil, ItemInfo{}, target, tv.VersionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := f.PurgeWithReport(ctx, root, "d", false)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(rep.Retired) != 0 {
		t.Fatalf("retired %+v; want none", rep.Retired)
	}
	for _, k := range []string{target, a, b, d} {
		if n := countVersions(t, ctx, f, k); n != 0 {
			t.Errorf("%s: %d versions left", k, n)
		}
	}
}

// TestPurgeReferrersOneByOne: the referrers' own purges drop their
// entries; the last one erases the holder and its .refs.
func TestPurgeReferrersOneByOne(t *testing.T) {
	x := newRetireFixture(t)
	ctx := context.Background()
	f, root := x.f, x.f.RootKey()
	if _, err := f.PurgeWithReport(ctx, root, "target", false); err != nil {
		t.Fatal(err)
	}
	if err := f.Purge(ctx, root, "a", false); err != nil {
		t.Fatalf("purge a: %v", err)
	}
	refs := readRefs(t, ctx, x.store, x.target)
	if refs == nil || !refs.Retired || len(refs.Referrers) != 1 || refs.Referrers[0].ReferrerKey != x.b {
		t.Fatalf(".refs after a's purge %+v; want b's entry only", refs)
	}
	if got := readName(t, ctx, f, root, "b"); got != string(x.two) {
		t.Fatalf("b reads %d bytes after a's purge", len(got))
	}
	// The last, from the trash.
	if err := f.Unlink(ctx, root, "b"); err != nil {
		t.Fatal(err)
	}
	trash, err := f.ListTrash(ctx, TrashOptions{})
	if err != nil || len(trash) != 1 {
		t.Fatalf("trash %v, %v", trash, err)
	}
	if err := f.PurgeTrash(ctx, trash[0].EntryID); err != nil {
		t.Fatalf("purge b: %v", err)
	}
	if n := countVersions(t, ctx, f, x.target); n != 0 {
		t.Fatalf("the holder: %d versions left after its last referrer went", n)
	}
	checkClean(t, f)
}

// TestPutRefRetired: a stub naming a retired holder is refused, and so is
// one whose write races a purge of its target (the purge lands between
// the stub's check of its target and its .refs entry).
func TestPutRefRetired(t *testing.T) {
	x := newRetireFixture(t)
	ctx := context.Background()
	f, root := x.f, x.f.RootKey()
	c := writeNew(t, ctx, f, root, "c", "c's own")
	if _, err := f.PurgeWithReport(ctx, root, "target", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PutRef(ctx, c, x.target, x.v1, nil, ""); !errors.Is(err, ErrRefTarget) {
		t.Fatalf("put ref to a retired holder: %v; want ErrRefTarget", err)
	}
	if _, _, err := f.CreateWithRef(ctx, root, "new", 0644, 0, 0, nil, ItemInfo{}, x.target, x.v2, nil); !errors.Is(err, ErrRefTarget) {
		t.Fatalf("create with ref to a retired holder: %v; want ErrRefTarget", err)
	}
	if refs := readRefs(t, ctx, x.store, x.target); len(refs.GetReferrers()) != 2 || !refs.GetRetired() {
		t.Fatalf(".refs %+v; want a's and b's entries only", refs)
	}

	defer func() { testHookRefChecked = nil }()
	race := func(t *testing.T, withReferrer bool) {
		name := "lonely"
		if withReferrer {
			name = "referenced"
		}
		target := writeNew(t, ctx, f, root, name, strings.Repeat(name, 100))
		tv, err := x.store.Head(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if withReferrer {
			if _, _, err := f.CreateWithRef(ctx, root, name+"-ref", 0644, 0, 0, nil, ItemInfo{}, target, tv.VersionID, nil); err != nil {
				t.Fatal(err)
			}
		}
		before, err := x.store.Head(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		testHookRefChecked = func() {
			testHookRefChecked = nil
			if err := f.Purge(ctx, root, name, false); err != nil {
				t.Errorf("purge in the race: %v", err)
			}
		}
		if _, err := f.PutRef(ctx, c, target, tv.VersionID, nil, ""); !errors.Is(err, ErrRefTarget) {
			t.Fatalf("put ref raced against a purge (%s): %v; want ErrRefTarget", name, err)
		}
		if now, _ := x.store.Head(ctx, c); now.VersionID != before.VersionID {
			t.Fatalf("%s: c was written", name)
		}
		refs := readRefs(t, ctx, x.store, target)
		if withReferrer {
			if refs == nil || !refs.Retired || len(refs.Referrers) != 1 {
				t.Fatalf("%s: .refs %+v; want retired, the one referrer", name, refs)
			}
		} else if n := countVersions(t, ctx, f, target); n != 0 {
			t.Fatalf("%s: %d versions left (.refs %+v)", name, n, refs)
		}
	}
	race(t, true)
	race(t, false)
	checkClean(t, f)
}

// TestReplaceFileHistoryRefs: a history rewrite of a referenced item is
// refused; one with a stub state, one keeping a stub version and one
// dropping a stub each keep the targets' .refs right.
func TestReplaceFileHistoryRefs(t *testing.T) {
	x := newRetireFixture(t)
	ctx := context.Background()
	f, root := x.f, x.f.RootKey()
	at := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }

	before, _ := x.store.History(ctx, x.target)
	err := f.ReplaceFileHistory(ctx, x.target, []FileState{{At: at(2020), Content: strings.NewReader("x")}}, "")
	if !errors.Is(err, ErrReferenced) {
		t.Fatalf("rewrite of a referenced item: %v; want ErrReferenced", err)
	}
	if after, _ := x.store.History(ctx, x.target); len(after) != len(before) {
		t.Fatalf("the refused rewrite changed the target: %d versions, was %d", len(after), len(before))
	}

	entriesOf := func(key string) []*pb.Referrer {
		t.Helper()
		var out []*pb.Referrer
		for _, r := range readRefs(t, ctx, x.store, x.target).GetReferrers() {
			if r.ReferrerKey == key {
				out = append(out, r)
			}
		}
		return out
	}
	checkStubNow := func(key string, want []byte, notVersion string) *objstore.Object {
		t.Helper()
		cur, err := x.store.Head(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if !IsRef(cur) || cur.VersionID == notVersion {
			t.Fatalf("%s's current %s isn't a new stub: %+v", key, cur.VersionID, cur.Metadata)
		}
		data, attr, _, err := f.ReadFile(ctx, key)
		if err != nil || !bytes.Equal(data, want) || attr.Size != int64(len(want)) {
			t.Fatalf("%s reads %d bytes (size %d), %v; want %d", key, len(data), attr.Size, err, len(want))
		}
		if r, ok := attr.Refs[cur.ETag]; !ok || r.Key != x.target {
			t.Fatalf("%s's .metadata refs %+v lack its stub's ETag %s", key, attr.Refs, cur.ETag)
		}
		es := entriesOf(key)
		if len(es) != 1 || es[0].ReferrerVersion != cur.VersionID || es[0].Pending {
			t.Fatalf("%s's entries %+v; want one, finalized, for %s", key, es, cur.VersionID)
		}
		return cur
	}

	// A stub state.
	c := writeNew(t, ctx, f, root, "c", "c's own")
	if err := f.ReplaceFileHistory(ctx, c, []FileState{{At: at(2020), Content: strings.NewReader("c in 2020")}, {At: at(2021), RefKey: c, RefVersion: x.v1}}, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a stub state naming the file itself: %v; want ErrInvalidArgument", err)
	}
	if err := f.ReplaceFileHistory(ctx, c, []FileState{
		{At: at(2020), Content: strings.NewReader("c in 2020")},
		{At: at(2021), RefKey: x.target, RefVersion: x.v1},
	}, ""); err != nil {
		t.Fatalf("rewrite with a stub state: %v", err)
	}
	checkStubNow(c, x.one, "")
	history, _, err := f.FileHistory(ctx, c)
	if err != nil || len(history) != 2 || history[0].Ref == nil || history[0].Size != int64(len(x.one)) || !history[0].At.Equal(at(2021)) {
		t.Fatalf("c's history %+v, %v", history, err)
	}

	// A kept stub.
	_, current, err := f.FileHistory(ctx, x.a)
	if err != nil || current != x.aStub.VersionID {
		t.Fatalf("a's current %s, %v; want its stub", current, err)
	}
	if err := f.ReplaceFileHistory(ctx, x.a, []FileState{
		{At: at(2020), Content: strings.NewReader("a in 2020")},
		{At: at(2021), Version: x.aStub.VersionID},
	}, current); err != nil {
		t.Fatalf("rewrite keeping a stub: %v", err)
	}
	checkStubNow(x.a, x.one, x.aStub.VersionID)
	if _, err := x.store.HeadVersion(ctx, x.a, x.aStub.VersionID); !objstore.IsNotFound(err) {
		t.Fatalf("the old stub version: %v; want erased", err)
	}

	// A stub dropped.
	if err := f.ReplaceFileHistory(ctx, x.b, []FileState{{At: at(2020), Content: strings.NewReader("plain")}}, ""); err != nil {
		t.Fatalf("rewrite dropping a stub: %v", err)
	}
	if es := entriesOf(x.b); len(es) != 0 {
		t.Fatalf("b's entries after its stub went: %+v", es)
	}
	checkClean(t, f)
}

// TestRestoreCurrentKeepsStub: a late write over a file whose current
// version is a stub puts a copy of the stub back on top: still a stub,
// its new version registered in the target's .refs, the old one's entry
// gone.
func TestRestoreCurrentKeepsStub(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	if err := f.MarkSourceTime(ctx); err != nil {
		t.Fatal(err)
	}
	dated := func(y, m int, change func(ctx context.Context)) {
		t.Helper()
		rec := &objstore.Recorder{}
		dctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)), rec)
		change(dctx)
		if _, err := f.RecordDates(dctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	data := strings.Repeat("the target. ", 100)
	var target, a string
	dated(2021, 1, func(ctx context.Context) { target = writeNew(t, ctx, f, f.RootKey(), "target", data) })
	tv, err := store.Head(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	dated(2022, 1, func(ctx context.Context) { a = writeNew(t, ctx, f, f.RootKey(), "a", "a") })
	var stub *objstore.Object
	dated(2023, 1, func(ctx context.Context) {
		if stub, err = f.PutRef(ctx, a, target, tv.VersionID, nil, ""); err != nil {
			t.Fatal(err)
		}
	})
	dated(2022, 6, func(ctx context.Context) {
		if _, err := f.WriteFile(ctx, a, []byte("late"), ""); err != nil {
			t.Fatal(err)
		}
	})
	cur, err := store.Head(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if !IsRef(cur) || cur.VersionID == stub.VersionID {
		t.Fatalf("a's current %s (stub %v); want a new copy of the stub %s", cur.VersionID, IsRef(cur), stub.VersionID)
	}
	if got := readName(t, ctx, f, f.RootKey(), "a"); got != data {
		t.Fatalf("a reads %q", got)
	}
	refs := readRefs(t, ctx, store, target)
	var mine []*pb.Referrer
	for _, r := range refs.GetReferrers() {
		if r.ReferrerKey == a {
			mine = append(mine, r)
		}
	}
	if len(mine) != 1 || mine[0].ReferrerVersion != cur.VersionID || mine[0].Pending {
		t.Fatalf("a's entries %+v; want one, finalized, for the copy %s", mine, cur.VersionID)
	}
	attr, err := f.Stat(ctx, a, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := attr.Refs[cur.ETag]; !ok || attr.Size != int64(len(data)) {
		t.Fatalf("a's .metadata refs %+v lack the copy's ETag %s (size %d)", attr.Refs, cur.ETag, attr.Size)
	}
	checkClean(t, f)
}

// TestCheckRefs: fsck finds each fault injected into stubs and .refs, a
// dead holder included, and repair fixes what it can.
func TestCheckRefs(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	target := writeNew(t, ctx, f, root, "target", strings.Repeat("one ", 100))
	if _, err := f.WriteFile(ctx, target, []byte(strings.Repeat("two ", 100)), ""); err != nil {
		t.Fatal(err)
	}
	h, _ := store.History(ctx, target)
	v2, v1 := h[0].VersionID, h[1].VersionID

	// ref-target-missing: m's stub names v1, then erased.
	m := writeNew(t, ctx, f, root, "m", "m")
	if _, err := f.PutRef(ctx, m, target, v1, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Erase(ctx, target, v1); err != nil {
		t.Fatal(err)
	}
	// ref-to-ref: r's stub names s's stub.
	s := writeNew(t, ctx, f, root, "s", "s")
	sStub, err := f.PutRef(ctx, s, target, v2, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	r := writeNew(t, ctx, f, root, "r", "r")
	rStub := putStub(t, ctx, store, r, s, sStub.VersionID, 400, refLayout)
	if err := f.writeMetadata(ctx, r, false, func(a *Attr) {
		a.Refs = map[string]Ref{rStub.ETag: {Key: s, VersionID: sStub.VersionID, Size: 400}}
	}); err != nil {
		t.Fatal(err)
	}
	// ref-entry-dead: an entry naming a version of g that is gone.
	g := writeNew(t, ctx, f, root, "g", "g one")
	gOld, _ := store.Head(ctx, g)
	if _, err := f.WriteFile(ctx, g, []byte("g two"), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Erase(ctx, g, gOld.VersionID); err != nil {
		t.Fatal(err)
	}
	// ref-entry-pending, twice: p's stub is there (finalized by repair);
	// g has none (dropped).
	p := writeNew(t, ctx, f, root, "p", "p")
	pStub, err := f.PutRef(ctx, p, target, v2, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.updateRefs(ctx, target, func(refs *pb.Refs) (bool, error) {
		for _, e := range refs.Referrers {
			if e.ReferrerKey == p {
				e.ReferrerVersion, e.Pending = "", true
			}
		}
		refs.Referrers = append(refs.Referrers,
			&pb.Referrer{ReferrerKey: g, ReferrerVersion: gOld.VersionID, TargetVersion: v2},
			&pb.Referrer{ReferrerKey: g, Pending: true, TargetVersion: v2})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// ref-holder-dead (and its entry dead): z's stub, holding a retired
	// holder, erased underneath.
	holder := writeNew(t, ctx, f, root, "holder", strings.Repeat("held ", 100))
	hv, _ := store.Head(ctx, holder)
	z := writeNew(t, ctx, f, root, "z", "z")
	zStub, err := f.PutRef(ctx, z, holder, hv.VersionID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := f.PurgeWithReport(ctx, root, "holder", false); err != nil || len(rep.Retired) != 1 {
		t.Fatalf("purge holder: %+v, %v", rep, err)
	}
	if _, err := f.WriteFile(ctx, z, []byte("z again"), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Erase(ctx, z, zStub.VersionID); err != nil {
		t.Fatal(err)
	}

	want := map[string]int{"ref-target-missing": 1, "ref-to-ref": 1, "ref-entry-dead": 2, "ref-entry-pending": 2, "ref-holder-dead": 1}
	kinds := func(r CheckReport) map[string]int {
		k := map[string]int{}
		for _, p := range r.Problems {
			k[p.Kind]++
		}
		return k
	}
	rep, err := f.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(rep); len(got) != len(want) || got["ref-target-missing"] != 1 || got["ref-to-ref"] != 1 || got["ref-entry-dead"] != 2 || got["ref-entry-pending"] != 2 || got["ref-holder-dead"] != 1 {
		t.Fatalf("check found %v; want %v\n%+v", got, want, rep.Problems)
	}
	if rep.RetiredHolders != 1 {
		t.Fatalf("retired holders %d, want 1", rep.RetiredHolders)
	}
	for _, p := range rep.Problems {
		if p.Repaired {
			t.Fatalf("a check without repair repaired %+v", p)
		}
	}

	rep, err = f.CheckRepair(ctx)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	for _, p := range rep.Problems {
		repairable := p.Kind == "ref-entry-dead" || p.Kind == "ref-entry-pending" || p.Kind == "ref-holder-dead"
		if p.Repaired != repairable {
			t.Errorf("%s %s: repaired %v, want %v (%s)", p.Kind, p.Key, p.Repaired, repairable, p.Detail)
		}
	}
	if got := kinds(rep); got["ref-entry-dead"] != 2 || got["ref-entry-pending"] != 2 || got["ref-holder-dead"] != 1 {
		t.Fatalf("repair found %v", got)
	}

	rep, err = f.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(rep); len(got) != 2 || got["ref-target-missing"] != 1 || got["ref-to-ref"] != 1 {
		t.Fatalf("after repair: %v; want only the two it can't repair\n%+v", got, rep.Problems)
	}
	var ps, gs []*pb.Referrer
	for _, e := range readRefs(t, ctx, store, target).GetReferrers() {
		switch e.ReferrerKey {
		case p:
			ps = append(ps, e)
		case g:
			gs = append(gs, e)
		}
	}
	if len(ps) != 1 || ps[0].Pending || ps[0].ReferrerVersion != pStub.VersionID || len(gs) != 0 {
		t.Fatalf("after repair: p's entries %+v (want finalized at %s), g's %+v (want none)", ps, pStub.VersionID, gs)
	}
	if n := countVersions(t, ctx, f, holder); n != 0 {
		t.Fatalf("the dead holder: %d versions left", n)
	}
	if rep.RetiredHolders != 0 {
		t.Fatalf("retired holders after repair: %d", rep.RetiredHolders)
	}
}
