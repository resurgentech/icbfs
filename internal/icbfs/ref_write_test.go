package icbfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// readRefs is target's .refs as stored now (nil if it has none).
func readRefs(t *testing.T, ctx context.Context, store objstore.Store, target string) *pb.Refs {
	t.Helper()
	body, _, err := store.Get(ctx, refsObjectKey(target))
	if objstore.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get %s: %v", refsObjectKey(target), err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var r pb.Refs
	if err := proto.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode .refs: %v", err)
	}
	return &r
}

// listing is every version under the filesystem's prefix, as key@version
// (delete markers marked), sorted as the store lists them.
func listing(t *testing.T, ctx context.Context, f *Filesystem) []string {
	t.Helper()
	versions, err := f.store.HistoryUnder(ctx, f.prefix())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		s := v.Key + "@" + v.VersionID
		if v.Deleted {
			s += "(deleted)"
		}
		out = append(out, s)
	}
	return out
}

func masterVersions(t *testing.T, ctx context.Context, store objstore.Store) int {
	t.Helper()
	h, err := store.History(ctx, masterBlockKey)
	if err != nil {
		t.Fatal(err)
	}
	return len(h)
}

// refTarget is a filesystem with a target file whose checksums are
// recorded.
type refTarget struct {
	f      *Filesystem
	store  objstore.Store
	key    string
	data   []byte
	ver    *objstore.Object
	sums   Checksums
	format uint32
}

func newRefTarget(t *testing.T) *refTarget {
	t.Helper()
	ctx := context.Background()
	f, store := newTestFilesystem(t)
	x := &refTarget{f: f, store: store, data: []byte(strings.Repeat("the target's bytes. ", 60))}
	x.key = writeNew(t, ctx, f, f.RootKey(), "target", string(x.data))
	var err error
	if x.ver, err = store.Head(ctx, x.key); err != nil {
		t.Fatal(err)
	}
	if x.sums, err = f.VersionChecksums(ctx, x.key, ""); err != nil {
		t.Fatal(err)
	}
	return x
}

// checkReadsAsTarget: key reads, stats and lists its history as x's
// target, through T49's readers, with .metadata's checksums and refs set
// for the stub's ETag.
func (x *refTarget) checkReadsAsTarget(t *testing.T, ctx context.Context, key string, stub *objstore.Object) {
	t.Helper()
	n := int64(len(x.data))
	if !IsRef(stub) {
		t.Fatalf("%s's current version isn't a stub: %+v", key, stub.Metadata)
	}
	if r, _, err := RefOf(stub); err != nil || r.Key != x.key || r.VersionID != x.ver.VersionID || r.Size != n {
		t.Fatalf("stub names %+v, %v; want %s %s %d", r, err, x.key, x.ver.VersionID, n)
	}
	data, attr, etag, err := x.f.ReadFile(ctx, key)
	if err != nil || !bytes.Equal(data, x.data) || etag != stub.ETag || attr.Size != n {
		t.Fatalf("read file: %d bytes, etag %s size %d, %v; want the target's %d, etag %s", len(data), etag, attr.Size, err, n, stub.ETag)
	}
	attr, err = x.f.Stat(ctx, key, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if attr.Size != n || attr.ContentETag() != stub.ETag {
		t.Fatalf("stat: size %d etag %s", attr.Size, attr.ContentETag())
	}
	want := Ref{Key: x.key, VersionID: x.ver.VersionID, Size: n}
	if attr.Refs[stub.ETag] != want {
		t.Fatalf(".metadata refs %+v, want %s: %+v", attr.Refs, stub.ETag, want)
	}
	if c, ok := attr.ContentChecksums(); !ok || !bytes.Equal(c.SHA256, x.sums.SHA256) || !bytes.Equal(c.MD5, x.sums.MD5) {
		t.Fatalf(".metadata checksums for the stub %x, %v; want the target's %x", c.SHA256, ok, x.sums.SHA256)
	}
	history, current, err := x.f.FileHistory(ctx, key)
	if err != nil || current != stub.VersionID {
		t.Fatalf("history current %s, %v", current, err)
	}
	for _, v := range history {
		if v.VersionID == stub.VersionID && (v.Size != n || v.Ref == nil || *v.Ref != want) {
			t.Fatalf("history's stub version %+v", v)
		}
	}
	// The record itself.
	body, _, err := x.store.GetVersion(ctx, key, stub.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(body)
	body.Close()
	rec, err := decodeRefRecord(raw)
	if err != nil || rec.Key != x.key || rec.VersionId != x.ver.VersionID || rec.Size != n || !bytes.Equal(rec.Sha256, x.sums.SHA256) {
		t.Fatalf("record %+v, %v", rec, err)
	}
}

// checkReferrer: target's .refs lists key's stub version, finalized.
func checkReferrer(t *testing.T, refs *pb.Refs, key string, stub *objstore.Object, targetVersion string) {
	t.Helper()
	if refs == nil {
		t.Fatalf("no .refs")
	}
	for _, r := range refs.Referrers {
		if r.ReferrerKey == key && r.ReferrerVersion == stub.VersionID && !r.Pending && r.TargetVersion == targetVersion {
			return
		}
	}
	t.Fatalf(".refs %v doesn't list %s version %s, finalized", refs.Referrers, key, stub.VersionID)
}

// TestPutRef: a stub written over a file reads back as its target, and
// the target's .refs lists it.
func TestPutRef(t *testing.T) {
	x := newRefTarget(t)
	ctx := context.Background()
	f := x.f
	a := writeNew(t, ctx, f, f.RootKey(), "a", "before")
	cur, err := x.store.Head(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	stub, err := f.PutRef(ctx, a, x.key, x.ver.VersionID, x.sums.SHA256, cur.ETag)
	if err != nil {
		t.Fatalf("put ref: %v", err)
	}
	if got, err := x.store.Head(ctx, a); err != nil || got.VersionID != stub.VersionID {
		t.Fatalf("current %+v, %v; want the stub %s", got, err, stub.VersionID)
	}
	x.checkReadsAsTarget(t, ctx, a, stub)
	refs := readRefs(t, ctx, x.store, x.key)
	checkReferrer(t, refs, a, stub, x.ver.VersionID)
	if len(refs.Referrers) != 1 || refs.Layout != refsLayout {
		t.Fatalf(".refs %+v", refs)
	}

	// No hash from the caller skips the compare.
	b := writeNew(t, ctx, f, f.RootKey(), "b", "b")
	stub, err = f.PutRef(ctx, b, x.key, x.ver.VersionID, nil, "")
	if err != nil {
		t.Fatalf("put ref without a hash: %v", err)
	}
	x.checkReadsAsTarget(t, ctx, b, stub)
	checkReferrer(t, readRefs(t, ctx, x.store, x.key), b, stub, x.ver.VersionID)

	// fsck: .refs is reachable.
	r, err := f.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Unreachable != 0 || len(r.Problems) != 0 {
		t.Fatalf("check: %d unreachable %v, problems %v", r.Unreachable, r.UnreachableListed, r.Problems)
	}
	// Nor once the target is in the trash.
	if err := f.Unlink(ctx, f.RootKey(), "target"); err != nil {
		t.Fatal(err)
	}
	if r, err = f.Check(ctx); err != nil || r.Unreachable != 0 {
		t.Fatalf("check, the target trashed: %d unreachable %v, %v", r.Unreachable, r.UnreachableListed, err)
	}
}

// TestCreateWithRef: a new file whose first content is a stub.
func TestCreateWithRef(t *testing.T) {
	x := newRefTarget(t)
	ctx := context.Background()
	f := x.f
	mtime := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	key, attr, err := f.CreateWithRef(ctx, f.RootKey(), "copy", 0640, 7, 8, nil, ItemInfo{Mtime: &mtime}, x.key, x.ver.VersionID, x.sums.SHA256)
	if err != nil {
		t.Fatalf("create with ref: %v", err)
	}
	if attr.Size != int64(len(x.data)) || attr.Mode&0777 != 0640 || attr.Uid != 7 || !attr.Mtime.Equal(mtime) {
		t.Fatalf("returned attributes: size %d mode %o uid %d mtime %v", attr.Size, attr.Mode, attr.Uid, attr.Mtime)
	}
	e, _, err := f.Lookup(ctx, f.RootKey(), "copy")
	if err != nil || e.UUID != key {
		t.Fatalf("lookup: %+v, %v", e, err)
	}
	stub, err := x.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	x.checkReadsAsTarget(t, ctx, key, stub)
	if st, _ := f.Stat(ctx, key, TypeFile); st.Size != attr.Size || st.ContentETag() != attr.ContentETag() || !st.Mtime.Equal(attr.Mtime) {
		t.Fatalf("stat %+v differs from what create returned %+v", st, attr)
	}
	checkReferrer(t, readRefs(t, ctx, x.store, x.key), key, stub, x.ver.VersionID)
}

// TestPutRefRefusals: each refusal writes nothing.
func TestPutRefRefusals(t *testing.T) {
	x := newRefTarget(t)
	ctx := context.Background()
	f := x.f
	a := writeNew(t, ctx, f, f.RootKey(), "a", "before")
	stubbed := writeNew(t, ctx, f, f.RootKey(), "stubbed", "x")
	stubVer := putStub(t, ctx, x.store, stubbed, x.key, x.ver.VersionID, int64(len(x.data)), refLayout)

	other := New(x.store, "other")
	if err := other.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	elsewhere := writeNew(t, ctx, other, other.RootKey(), "elsewhere", "theirs")
	elsewhereVer, err := x.store.Head(ctx, elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("other bytes"))
	// A version that was there and was erased.
	gone, err := x.store.Put(ctx, x.key, strings.NewReader("gone"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.store.Erase(ctx, x.key, gone.VersionID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name           string
		target, ver    string
		hash           []byte
		want, wantWrap error
	}{
		{"a missing target", x.key, gone.VersionID, nil, ErrRefTarget, ErrNotFound},
		{"a missing key", f.id + "-0190a000-0000-7000-8000-000000000000", uuid.NewString(), nil, ErrRefTarget, ErrNotFound},
		{"a stub as target", stubbed, stubVer.VersionID, nil, ErrRefTarget, ErrRefToRef},
		{"another filesystem's", elsewhere, elsewhereVer.VersionID, nil, ErrRefTarget, nil},
		{"a checksum mismatch", x.key, x.ver.VersionID, wrong[:], ErrRefChecksum, nil},
	}
	for _, c := range cases {
		before, masters := listing(t, ctx, f), masterVersions(t, ctx, x.store)
		_, err := f.PutRef(ctx, a, c.target, c.ver, c.hash, "")
		if !errors.Is(err, c.want) || (c.wantWrap != nil && !errors.Is(err, c.wantWrap)) {
			t.Errorf("put ref, %s: %v; want %v (%v)", c.name, err, c.want, c.wantWrap)
		}
		_, _, err = f.CreateWithRef(ctx, f.RootKey(), "new-"+strings.ReplaceAll(c.name, " ", "-"), 0644, 0, 0, nil, ItemInfo{}, c.target, c.ver, c.hash)
		if !errors.Is(err, c.want) {
			t.Errorf("create with ref, %s: %v; want %v", c.name, err, c.want)
		}
		if after := listing(t, ctx, f); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Errorf("%s: the listing changed:\nbefore %v\nafter  %v", c.name, before, after)
		}
		if m := masterVersions(t, ctx, x.store); m != masters {
			t.Errorf("%s: the master block was written (%d versions, was %d)", c.name, m, masters)
		}
	}

	// An undated target on a filesystem written in a source's time: a
	// version written outside any change (never dated).
	if err := f.MarkSourceTime(ctx); err != nil {
		t.Fatal(err)
	}
	// A write that dates itself gives the target its table, listing the
	// versions it had at the store's times.
	if _, err := f.WriteFile(ctx, x.key, x.data, ""); err != nil {
		t.Fatal(err)
	}
	undated, err := x.store.Put(ctx, x.key, strings.NewReader("undated"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	before, masters := listing(t, ctx, f), masterVersions(t, ctx, x.store)
	if _, err := f.PutRef(ctx, a, x.key, undated.VersionID, nil, ""); !errors.Is(err, ErrRefTarget) {
		t.Errorf("put ref to an undated target: %v; want ErrRefTarget", err)
	}
	if _, _, err := f.CreateWithRef(ctx, f.RootKey(), "new-undated", 0644, 0, 0, nil, ItemInfo{}, x.key, undated.VersionID, nil); !errors.Is(err, ErrRefTarget) {
		t.Errorf("create with ref to an undated target: %v; want ErrRefTarget", err)
	}
	if after := listing(t, ctx, f); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("undated: the listing changed:\nbefore %v\nafter  %v", before, after)
	}
	if m := masterVersions(t, ctx, x.store); m != masters {
		t.Errorf("undated: the master block was written")
	}
	// The dated one is still a target.
	if _, err := f.PutRef(ctx, a, x.key, x.ver.VersionID, nil, ""); err != nil {
		t.Errorf("put ref to a dated target: %v", err)
	}
}

// TestPutRefLostIfMatch: a stub whose If-Match on key loses leaves only a
// pending entry in the target's .refs: key and its .metadata unchanged.
func TestPutRefLostIfMatch(t *testing.T) {
	x := newRefTarget(t)
	ctx := context.Background()
	f := x.f
	a := writeNew(t, ctx, f, f.RootKey(), "a", "before")
	stale, err := x.store.Head(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteFile(ctx, a, []byte("newer"), stale.ETag); err != nil {
		t.Fatal(err)
	}
	contentBefore, _ := x.store.History(ctx, a)
	metaBefore, _ := x.store.History(ctx, metadataObjectKey(a))
	if _, err := f.PutRef(ctx, a, x.key, x.ver.VersionID, nil, stale.ETag); !objstore.IsPreconditionFailed(err) {
		t.Fatalf("put ref on a stale ETag: %v; want a precondition failure", err)
	}
	contentAfter, _ := x.store.History(ctx, a)
	metaAfter, _ := x.store.History(ctx, metadataObjectKey(a))
	if len(contentAfter) != len(contentBefore) || len(metaAfter) != len(metaBefore) {
		t.Fatalf("a changed: content %d -> %d versions, .metadata %d -> %d", len(contentBefore), len(contentAfter), len(metaBefore), len(metaAfter))
	}
	refs := readRefs(t, ctx, x.store, x.key)
	if refs == nil || len(refs.Referrers) != 1 {
		t.Fatalf(".refs %v, want one entry", refs)
	}
	if r := refs.Referrers[0]; r.ReferrerKey != a || !r.Pending || r.ReferrerVersion != "" {
		t.Fatalf(".refs entry %+v, want a pending one for %s", r, a)
	}
}

// TestPutRefConcurrent: two stubs onto one target at once are both in its
// .refs.
func TestPutRefConcurrent(t *testing.T) {
	x := newRefTarget(t)
	ctx := context.Background()
	f := x.f
	// A second mount of the same filesystem, so the two don't share
	// anything in memory.
	g := New(x.store, "test")
	if err := g.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	keys := []string{writeNew(t, ctx, f, f.RootKey(), "a", "a"), writeNew(t, ctx, f, f.RootKey(), "b", "b")}
	for round := 0; round < 3; round++ {
		stubs := make([]*objstore.Object, len(keys))
		errs := make([]error, len(keys))
		var wg sync.WaitGroup
		for i, fs := range []*Filesystem{f, g} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stubs[i], errs[i] = fs.PutRef(ctx, keys[i], x.key, x.ver.VersionID, nil, "")
			}()
		}
		wg.Wait()
		refs := readRefs(t, ctx, x.store, x.key)
		for i := range keys {
			if errs[i] != nil {
				t.Fatalf("round %d, put ref %d: %v", round, i, errs[i])
			}
			checkReferrer(t, refs, keys[i], stubs[i], x.ver.VersionID)
		}
		if len(refs.Referrers) != 2*(round+1) {
			t.Fatalf("round %d: %d entries, want %d", round, len(refs.Referrers), 2*(round+1))
		}
	}
}

// TestPutRefRaisesFormat: a filesystem's first stub raises its format to
// 2, once; a build pinned to format 1 then refuses it.
func TestPutRefRaisesFormat(t *testing.T) {
	x := newRefTarget(t)
	ctx := context.Background()
	f := x.f
	// Opened before the raise: it still thinks the filesystem is at 1.
	g := New(x.store, "test")
	if err := g.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := DescribeFilesystem(ctx, x.store, "test")
	if err != nil || info.FormatVersion != FormatVersion {
		t.Fatalf("format %d, %v; want %d", info.FormatVersion, err, FormatVersion)
	}
	masters := masterVersions(t, ctx, x.store)
	a := writeNew(t, ctx, f, f.RootKey(), "a", "a")
	if _, err := f.PutRef(ctx, a, x.key, x.ver.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if info, _ := DescribeFilesystem(ctx, x.store, "test"); info.FormatVersion != formatVersionRefs {
		t.Fatalf("after the first stub: format %d, want %d", info.FormatVersion, formatVersionRefs)
	}
	if m := masterVersions(t, ctx, x.store); m != masters+1 {
		t.Fatalf("master block: %d new versions, want 1", m-masters)
	}
	if _, err := f.PutRef(ctx, a, x.key, x.ver.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.CreateWithRef(ctx, f.RootKey(), "c", 0644, 0, 0, nil, ItemInfo{}, x.key, x.ver.VersionID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.PutRef(ctx, a, x.key, x.ver.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if m := masterVersions(t, ctx, x.store); m != masters+1 {
		t.Fatalf("master block written again: %d new versions, want 1", m-masters)
	}

	// A build that reads only format 1.
	defer func(was uint32) { openFormatVersion = was }(openFormatVersion)
	openFormatVersion = FormatVersion
	if err := New(x.store, "test").Attach(ctx); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("a format-1 build attaching: %v, want ErrNewerFormat", err)
	}
	if err := New(x.store, "test").BootstrapReadOnly(ctx); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("a format-1 build opening read-only: %v, want ErrNewerFormat", err)
	}
}

// TestPutRefDeclared: under a declared time, a stub is recorded and dated
// like any other write; the bookkeeping (.refs, the master block) isn't
// part of any item's history.
func TestPutRefDeclared(t *testing.T) {
	x := newRefTarget(t)
	ctx := context.Background()
	f := x.f
	if err := f.MarkSourceTime(ctx); err != nil {
		t.Fatal(err)
	}
	at := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	dated := func(y int, change func(ctx context.Context)) {
		t.Helper()
		rec := &objstore.Recorder{}
		dctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, at(y)), rec)
		change(dctx)
		if _, err := f.RecordDates(dctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	// The target, written in a source's time: dated.
	var target string
	dated(2021, func(ctx context.Context) { target = writeNew(t, ctx, f, f.RootKey(), "target-2021", string(x.data)) })
	tver, err := x.store.Head(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	var a, created string
	dated(2022, func(ctx context.Context) { a = writeNew(t, ctx, f, f.RootKey(), "a", "a") })
	var stub *objstore.Object
	dated(2023, func(ctx context.Context) {
		var err error
		if stub, err = f.PutRef(ctx, a, target, tver.VersionID, nil, ""); err != nil {
			t.Fatalf("put ref: %v", err)
		}
	})
	dated(2024, func(ctx context.Context) {
		var err error
		if created, _, err = f.CreateWithRef(ctx, f.RootKey(), "copy", 0644, 0, 0, nil, ItemInfo{}, target, tver.VersionID, nil); err != nil {
			t.Fatalf("create with ref: %v", err)
		}
	})
	dates, err := objstore.ReadDates(ctx, x.store, a)
	if err != nil {
		t.Fatal(err)
	}
	if got := dates[stub.VersionID]; !got.Equal(at(2023)) {
		t.Fatalf("the stub's date %v, want %v (table %v)", got, at(2023), dates)
	}
	cstub, err := x.store.Head(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	if cdates, err := objstore.ReadDates(ctx, x.store, created); err != nil || !cdates[cstub.VersionID].Equal(at(2024)) {
		t.Fatalf("the created stub's date: %v, %v", cdates, err)
	}
	if !IsRef(cstub) {
		t.Fatalf("created's current isn't a stub")
	}
	if Btime(created).Year() != 2024 {
		t.Fatalf("created's key was minted at %v, want 2024", Btime(created))
	}
	// Its .metadata is dated too: every version is in the table.
	mh, err := x.store.History(ctx, metadataObjectKey(a))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range mh {
		if _, ok := dates[v.VersionID]; !ok && !v.Deleted {
			t.Fatalf("a .metadata version %s isn't dated (table %v)", v.VersionID, dates)
		}
	}
	// .refs has no .version record of its own, and isn't recorded.
	if _, err := x.store.Head(ctx, refsObjectKey(target)+objstore.VersionSuffix); !objstore.IsNotFound(err) {
		t.Fatalf(".refs has a .version: %v", err)
	}
	if refs := readRefs(t, ctx, x.store, target); refs == nil || len(refs.Referrers) != 2 {
		t.Fatalf(".refs %v", refs)
	}
	tdates, err := objstore.ReadDates(ctx, x.store, target)
	if err != nil {
		t.Fatal(err)
	}
	rh, err := x.store.History(ctx, refsObjectKey(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range rh {
		if _, ok := tdates[v.VersionID]; ok {
			t.Fatalf(".refs version %s is dated in the target's table", v.VersionID)
		}
	}
	// It reads back through a view as of 2023.
	hist, err := objstore.OpenTempHistoryCache(ctx, x.store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	view, err := objstore.NewAsOf(ctx, hist, at(2023).Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	past := New(view, "test")
	if err := past.BootstrapReadOnly(ctx); err != nil {
		t.Fatal(err)
	}
	if data, _, _, err := past.ReadFile(ctx, a); err != nil || !bytes.Equal(data, x.data) {
		t.Fatalf("as of 2023: %d bytes, %v", len(data), err)
	}
	r, err := f.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Unreachable != 0 {
		t.Fatalf("check: unreachable %v", r.UnreachableListed)
	}
}

// TestBaseKeyRefs: a .refs object belongs to the item beside it.
func TestBaseKeyRefs(t *testing.T) {
	key := "0000-0190a000-0000-7000-8000-000000000000"
	if got := objstore.BaseKey(refsObjectKey(key)); got != key {
		t.Fatalf("BaseKey(%s) = %s", refsObjectKey(key), got)
	}
	f := &Filesystem{id: "0000", fsName: "test"}
	f.sourceTime.Store(true)
	if f.dated(refsObjectKey(key)) {
		t.Fatalf("%s is dated", refsObjectKey(key))
	}
}
