package drive_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// sha is a SHA-256-shaped key for the tests: c repeated.
func sha(c string) string { return strings.Repeat(c, 64) }

// TestHashMapContentMap: the in-memory map as a ContentMap looks up by
// SHA-256 whatever its case and by source hash, keeps the lesser of two
// versions with the same content, drops a version only if it's still
// the one held, and counts its contents.
func TestHashMapContentMap(t *testing.T) {
	ctx := context.Background()
	h := drive.NewHashMap()
	var c drive.ContentMap = h.ContentMap()
	a, b := drive.HashEntry{ID: "a", Version: "1", Size: 3}, drive.HashEntry{ID: "b", Version: "1", Size: 3}
	if err := c.Add(ctx, strings.ToUpper(sha("a")), b); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(ctx, sha("a"), a); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(ctx, sha("a"), b); err != nil { // the greater: kept out
		t.Fatal(err)
	}
	if err := c.Add(ctx, "", a); err != nil { // nothing to record
		t.Fatal(err)
	}
	if e, ok, err := c.Lookup(ctx, strings.ToUpper(sha("a"))); err != nil || !ok || e != a {
		t.Fatalf("lookup: %+v %v %v, want a", e, ok, err)
	}
	if _, ok, err := c.Lookup(ctx, sha("b")); ok || err != nil {
		t.Fatalf("lookup of an unknown: %v %v", ok, err)
	}
	md5 := drive.Hash{Kind: "md5", Value: "feed"}
	if err := c.AddSource(ctx, md5, sha("a")); err != nil {
		t.Fatal(err)
	}
	if s, e, ok, err := c.LookupSource(ctx, md5); err != nil || !ok || s != sha("a") || e != a {
		t.Fatalf("lookup by source: %q %+v %v %v", s, e, ok, err)
	}
	if n, err := c.Count(ctx); err != nil || n != 1 {
		t.Fatalf("count %d %v, want 1", n, err)
	}
	if err := c.Drop(ctx, sha("a"), b); err != nil { // not the one held
		t.Fatal(err)
	}
	if _, ok, _ := c.Lookup(ctx, sha("a")); !ok {
		t.Fatal("dropped another version's content")
	}
	if err := c.Drop(ctx, strings.ToUpper(sha("a")), a); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := c.LookupSource(ctx, md5); ok || err != nil {
		t.Fatalf("a source hash of dropped content: %v %v", ok, err)
	}
	if n, _ := c.Count(ctx); n != 0 || h.Len() != 0 {
		t.Fatalf("count after the drop %d (Len %d), want 0", n, h.Len())
	}
	// The HashMap and its ContentMap are one map.
	h.Add(sha("c"), a)
	if e, ok, _ := c.Lookup(ctx, sha("c")); !ok || e != a {
		t.Fatalf("the HashMap's add isn't the ContentMap's: %+v %v", e, ok)
	}
}

// TestHashMapRebuild: a rebuild of the in-memory map leaves the map as
// it was until it's committed; committed, the map is what was added,
// the lesser version of each content, with the source hashes whose
// content is still there; abandoned, or failing, the map is as it was.
func TestHashMapRebuild(t *testing.T) {
	ctx := context.Background()
	h := drive.NewHashMap()
	c := h.ContentMap()
	old := drive.HashEntry{ID: "old", Version: "1", Size: 1}
	h.Add(sha("a"), old)
	h.Add(sha("b"), old)
	h.AddSource(drive.Hash{Kind: "md5", Value: "a"}, sha("a"))
	h.AddSource(drive.Hash{Kind: "md5", Value: "b"}, sha("b"))
	before, beforeSrc := h.Entries(), h.SourceEntries()

	// Abandoned: as it was.
	r, err := c.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(ctx, sha("z"), old); err != nil {
		t.Fatal(err)
	}
	r.Abandon()
	if !reflect.DeepEqual(h.Entries(), before) || !reflect.DeepEqual(h.SourceEntries(), beforeSrc) {
		t.Fatalf("after an abandoned rebuild %+v %+v", h.Entries(), h.SourceEntries())
	}

	// Committed.
	r, err = c.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Abandon() // after a commit: nothing
	x, y := drive.HashEntry{ID: "x", Version: "2", Size: 3}, drive.HashEntry{ID: "x", Version: "1", Size: 3}
	for _, e := range []struct {
		sha string
		e   drive.HashEntry
	}{{strings.ToUpper(sha("a")), x}, {sha("a"), y}, {sha("c"), x}, {"", x}} {
		if err := r.Add(ctx, e.sha, e.e); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := c.Count(ctx); n != 2 || !reflect.DeepEqual(h.Entries(), before) {
		t.Fatalf("the map during the rebuild: %d %+v, want it as it was", n, h.Entries())
	}
	if err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]drive.HashEntry{sha("a"): y, sha("c"): x}
	if got := h.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt %+v, want %+v", got, want)
	}
	wantSrc := map[drive.Hash]string{{Kind: "md5", Value: "a"}: sha("a")}
	if got := h.SourceEntries(); !reflect.DeepEqual(got, wantSrc) {
		t.Fatalf("source hashes %+v, want %+v", got, wantSrc)
	}
	if err := r.Commit(ctx); !errors.Is(err, drive.ErrInvalid) {
		t.Fatalf("a second commit: %v, want ErrInvalid", err)
	}
	if n, _ := c.Count(ctx); n != 2 {
		t.Fatalf("count %d after the commit, want 2", n)
	}
}
