// Package storetest is the conformance suite every objstore.Store runs:
// the behaviour icbfs relies on, whatever the store. Each store's own
// tests call Run with a function that makes an empty store (or one whose
// keys no other test sees).
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// Run runs the suite, each test with a store from newStore.
func Run(t *testing.T, newStore func(*testing.T) objstore.Store) {
	t.Run("PutGetRoundTrip", func(t *testing.T) { putGetRoundTrip(t, newStore) })
	t.Run("HeadReturnsMetadataAndSize", func(t *testing.T) { headReturnsMetadataAndSize(t, newStore) })
	t.Run("PutIfAbsent", func(t *testing.T) { putIfAbsent(t, newStore) })
	t.Run("ConditionalPutRejectsStaleETag", func(t *testing.T) { conditionalPutRejectsStaleETag(t, newStore) })
	t.Run("PutCutOffIsLostRaceOnlyIfChanged", func(t *testing.T) { putCutOffIsLostRaceOnlyIfChanged(t, newStore) })
	t.Run("PutPartsCopiesAndUploadsUnderIfMatch", func(t *testing.T) { putPartsCopiesAndUploadsUnderIfMatch(t, newStore) })
	t.Run("HistoryIsNewestFirstWithTimestamps", func(t *testing.T) { historyIsNewestFirstWithTimestamps(t, newStore) })
	t.Run("HistoryPaginatesAndIncludesDeletions", func(t *testing.T) { historyPaginatesAndIncludesDeletions(t, newStore) })
	t.Run("DeleteAndUndelete", func(t *testing.T) { deleteAndUndelete(t, newStore) })
	t.Run("Erase", func(t *testing.T) { erase(t, newStore) })
	t.Run("EraseAll", func(t *testing.T) { eraseAll(t, newStore) })
	t.Run("List", func(t *testing.T) { list(t, newStore) })
	t.Run("ServerTimeIsTheStoresClock", func(t *testing.T) { serverTimeIsTheStoresClock(t, newStore) })
	t.Run("AsOf", func(t *testing.T) { asOf(t, newStore) })
	t.Run("HistoryCacheLastsForItsTTL", func(t *testing.T) { historyCacheLastsForItsTTL(t, newStore) })
	t.Run("AsOfAfterErase", func(t *testing.T) { asOfAfterErase(t, newStore) })
	t.Run("HistoryCacheKeepsVersions", func(t *testing.T) { historyCacheKeepsVersions(t, newStore) })
}

// pause lets the store's clock pass a whole second, so a time taken
// after it falls strictly between the writes on either side.
func pause() { time.Sleep(1100 * time.Millisecond) }

func serverNow(t *testing.T, s objstore.Store) time.Time {
	t.Helper()
	now, err := s.ServerTime(context.Background())
	if err != nil {
		t.Fatalf("server time: %v", err)
	}
	return now
}

func mustGetVersion(t *testing.T, store objstore.Store, key, versionID string) []byte {
	t.Helper()
	body, _, err := store.GetVersion(context.Background(), key, versionID)
	if err != nil {
		t.Fatalf("get version %s: %v", versionID, err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read version %s: %v", versionID, err)
	}
	return data
}

func read(t *testing.T, store objstore.Store, key string) (string, error) {
	t.Helper()
	body, _, err := store.Get(context.Background(), key)
	if err != nil {
		return "", err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	return string(data), err
}

// failingReader fails after its first n bytes, as a connection the server
// reset would.
type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	n := min(len(p), r.n)
	clear(p[:n])
	r.n -= n
	return n, nil
}

// putIfAbsent: PutIfAbsent writes a key that has no current version,
// deleted ones included, and refuses one that has.
func putIfAbsent(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	if _, err := store.PutIfAbsent(ctx, "k", strings.NewReader("first"), nil); err != nil {
		t.Fatalf("put if absent: %v", err)
	}
	if _, err := store.PutIfAbsent(ctx, "k", strings.NewReader("second"), nil); !objstore.IsPreconditionFailed(err) {
		t.Fatalf("put if absent over an object: %v, want a precondition failure", err)
	}
	if got, _ := read(t, store, "k"); got != "first" {
		t.Fatalf("after a refused put if absent: %q, want first", got)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutIfAbsent(ctx, "k", strings.NewReader("third"), nil); err != nil {
		t.Fatalf("put if absent over a deleted key: %v", err)
	}
	if got, _ := read(t, store, "k"); got != "third" {
		t.Fatalf("after put if absent on a deleted key: %q, want third", got)
	}
}

// deleteAndUndelete: Delete ends the current version and records the
// deletion; Undelete brings the newest version back, and does nothing to
// a key that isn't deleted.
func deleteAndUndelete(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	for _, v := range []string{"one", "two"} {
		if _, err := store.Put(ctx, "k", strings.NewReader(v), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Undelete(ctx, "k"); err != nil {
		t.Fatalf("undelete a key that isn't deleted: %v", err)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := read(t, store, "k"); !objstore.IsNotFound(err) {
		t.Fatalf("get after delete: %v, want not found", err)
	}
	if _, err := store.Head(ctx, "k"); !objstore.IsNotFound(err) {
		t.Fatalf("head after delete: %v, want not found", err)
	}
	history, err := store.History(ctx, "k")
	if err != nil || len(history) < 3 || !history[0].Deleted {
		t.Fatalf("history after delete: %+v, %v; want the deletion first", history, err)
	}
	if err := store.Undelete(ctx, "k"); err != nil {
		t.Fatalf("undelete: %v", err)
	}
	if got, err := read(t, store, "k"); got != "two" || err != nil {
		t.Fatalf("after undelete: %q, %v; want two", got, err)
	}
}

// erase: Erase removes one entry from the history for good, leaving the
// current version as it was.
func erase(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	v1, err := store.Put(ctx, "k", strings.NewReader("one"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "k", strings.NewReader("two"), nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Erase(ctx, "k", v1.VersionID); err != nil {
		t.Fatalf("erase version one: %v", err)
	}
	history, err := store.History(ctx, "k")
	if err != nil || len(history) == 0 {
		t.Fatalf("history after erasing: %+v, %v", history, err)
	}
	for _, v := range history {
		if v.VersionID == v1.VersionID {
			t.Fatalf("version one is still in the history: %+v", history)
		}
	}
	if got, err := read(t, store, "k"); got != "two" || err != nil {
		t.Fatalf("after erasing version one: %q, %v; want two", got, err)
	}
	if _, _, err := store.GetVersion(ctx, "k", v1.VersionID); !objstore.IsNotFound(err) {
		t.Fatalf("get an erased version: %v, want not found", err)
	}
}

// eraseAll: EraseAll removes every key under a prefix, history and all,
// and nothing outside it.
func eraseAll(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	for _, kv := range [][2]string{{"p/a", "1"}, {"p/b", "1"}, {"p/b", "2"}, {"q", "keep"}} {
		if _, err := store.Put(ctx, kv[0], strings.NewReader(kv[1]), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(ctx, "p/a"); err != nil {
		t.Fatal(err)
	}
	if err := store.EraseAll(ctx, "p/"); err != nil {
		t.Fatalf("erase all: %v", err)
	}
	if h, err := store.HistoryUnder(ctx, "p/"); err != nil || len(h) != 0 {
		t.Fatalf("history under p/ after erase all: %+v, %v; want none", h, err)
	}
	if got, err := read(t, store, "q"); got != "keep" || err != nil {
		t.Fatalf("q after erasing p/: %q, %v", got, err)
	}
}

// list: List names the keys under a prefix that have a current version,
// with their sizes.
func list(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	for _, kv := range [][2]string{{"p/a", "12345"}, {"p/b", "1"}, {"pq", "x"}} {
		if _, err := store.Put(ctx, kv[0], strings.NewReader(kv[1]), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(ctx, "p/b"); err != nil {
		t.Fatal(err)
	}
	got, err := store.List(ctx, "p/")
	if err != nil || len(got) != 1 || got[0].Key != "p/a" || got[0].Size != 5 {
		t.Fatalf("list p/: %+v, %v; want p/a, 5 bytes", got, err)
	}
}

func putGetRoundTrip(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()

	obj, err := store.Put(ctx, "file-1", strings.NewReader("hello icbfs"), map[string]string{
		"mode": "0644",
	}, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if obj.VersionID == "" {
		t.Fatal("expected a version id on a versioned bucket")
	}

	body, got, err := store.Get(ctx, "file-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != "hello icbfs" {
		t.Fatalf("got body %q, want %q", data, "hello icbfs")
	}
	if got.Metadata["mode"] != "0644" {
		t.Fatalf("got mode metadata %q, want %q", got.Metadata["mode"], "0644")
	}
}

func headReturnsMetadataAndSize(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.Put(ctx, "file-1", strings.NewReader("twelve bytes"), map[string]string{
		"uid": "1000",
		"gid": "1000",
	}, ""); err != nil {
		t.Fatalf("put: %v", err)
	}

	head, err := store.Head(ctx, "file-1")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Size != int64(len("twelve bytes")) {
		t.Fatalf("got size %d, want %d", head.Size, len("twelve bytes"))
	}
	if head.Metadata["uid"] != "1000" || head.Metadata["gid"] != "1000" {
		t.Fatalf("got metadata %+v, want uid/gid=1000", head.Metadata)
	}
}

// historyIsNewestFirstWithTimestamps: a key's versions list newest
// first with timestamps, which as-of views pick from.
func historyIsNewestFirstWithTimestamps(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.Put(ctx, "file-1", strings.NewReader("v1"), nil, ""); err != nil {
		t.Fatalf("put v1: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // S3 LastModified has ~1s resolution
	tBetween := time.Now()
	time.Sleep(1100 * time.Millisecond)
	if _, err := store.Put(ctx, "file-1", strings.NewReader("v2"), nil, ""); err != nil {
		t.Fatalf("put v2: %v", err)
	}

	versions, err := store.History(ctx, "file-1")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(versions))
	}
	if !versions[0].LastModified.After(versions[1].LastModified) {
		t.Fatalf("expected newest-first ordering, got %+v", versions)
	}

	// Walk logic from docs/architecture.md: the version current as of tBetween
	// is the one with the latest LastModified <= tBetween.
	var asOfBetween *objstore.Object
	for i := range versions {
		if !versions[i].LastModified.After(tBetween) {
			asOfBetween = &versions[i]
			break
		}
	}
	if asOfBetween == nil {
		t.Fatal("expected to find a version current as of tBetween")
	}

	asOfData := mustGetVersion(t, store, "file-1", asOfBetween.VersionID)
	if string(asOfData) != "v1" {
		t.Fatalf("version resolved as-of tBetween = %q, want %q", asOfData, "v1")
	}
}

// TestConditionalPutRejectsStaleETag validates the concurrency mechanism
// docs/architecture.md commits to: a write conditioned on a stale ETag must be
// rejected, not silently overwrite a concurrent change.
func conditionalPutRejectsStaleETag(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()

	v1, err := store.Put(ctx, "dir-block", strings.NewReader("entry-a"), nil, "")
	if err != nil {
		t.Fatalf("initial put: %v", err)
	}

	// A second writer updates the object without knowing about a third
	// writer, moving the ETag forward.
	if _, err := store.Put(ctx, "dir-block", strings.NewReader("entry-a,entry-b"), nil, v1.ETag); err != nil {
		t.Fatalf("second writer's conditional put should have succeeded: %v", err)
	}

	// A first writer, still holding the stale v1 ETag, tries to write —
	// this must be rejected rather than silently clobbering entry-b.
	_, err = store.Put(ctx, "dir-block", strings.NewReader("entry-a,entry-c"), nil, v1.ETag)
	if err == nil {
		t.Fatal("expected conditional put with a stale ETag to fail")
	}
	if !objstore.IsPreconditionFailed(err) {
		t.Fatalf("expected a precondition-failed error, got: %v", err)
	}

	body, _, err := store.Get(ctx, "dir-block")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "entry-a,entry-b" {
		t.Fatalf("stale write must not have applied: got %q", data)
	}
}

// serverTimeIsTheStoresClock: MinIO returns a Date
// header, and it parses to about now, within a budget covering its
// whole-second resolution.
func serverTimeIsTheStoresClock(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()

	before := time.Now()
	got, err := store.ServerTime(ctx)
	after := time.Now()
	if err != nil {
		t.Fatalf("server time: %v", err)
	}

	// The header reads in whole seconds, so it can trail the request by
	// almost a second with no skew at all.
	const skewBudget = 2 * time.Second
	if got.Before(before.Add(-skewBudget)) || got.After(after.Add(skewBudget)) {
		t.Fatalf("server time %v is outside [%v, %v] (request window ± %v) — looks like real clock skew, not just Date's whole-second rounding", got, before.Add(-skewBudget), after.Add(skewBudget), skewBudget)
	}
}

// putPartsCopiesAndUploadsUnderIfMatch: a multipart write mixing
// server-side copies of an older version's ranges with uploaded parts
// assembles exactly those bytes, honors If-Match, and leaves the object
// alone when it fails.
func putPartsCopiesAndUploadsUnderIfMatch(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	const mib = 1 << 20

	base := bytes.Repeat([]byte("a"), 12*mib)
	v1, err := store.Put(ctx, "big", bytes.NewReader(base), nil, "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	middle := bytes.Repeat([]byte("M"), 5*mib)
	parts := func() []objstore.Part {
		return []objstore.Part{
			{Size: 5 * mib, CopyVersion: v1.VersionID, CopyOffset: 0},
			{Size: 5 * mib, Body: bytes.NewReader(middle)},
			{Size: 2 * mib, CopyVersion: v1.VersionID, CopyOffset: 10 * mib},
		}
	}

	if _, err := store.PutParts(ctx, "big", parts(), nil, `"stale-etag"`); !objstore.IsPreconditionFailed(err) {
		t.Fatalf("PutParts with a stale If-Match: %v, want precondition failed", err)
	}
	v2, err := store.PutParts(ctx, "big", parts(), map[string]string{"k": "v"}, v1.ETag)
	if err != nil {
		t.Fatalf("PutParts: %v", err)
	}
	want := append(append(bytes.Repeat([]byte("a"), 5*mib), middle...), bytes.Repeat([]byte("a"), 2*mib)...)
	body, obj, err := store.Get(ctx, "big")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(got, want) || obj.Size != 12*mib || obj.ETag != v2.ETag || obj.Metadata["k"] != "v" {
		t.Fatalf("assembled object: size %d etag %s (want %s) metadata %v, content equal %v",
			obj.Size, obj.ETag, v2.ETag, obj.Metadata, bytes.Equal(got, want))
	}

	// Where the store has a minimum part size, a part below it fails the
	// whole write and nothing changes. (Azure's blocks have no minimum.)
	if store.PartLimits().Min <= 1 {
		return
	}
	bad := []objstore.Part{{Size: 1, Body: bytes.NewReader([]byte("x"))}, {Size: 1, Body: bytes.NewReader([]byte("y"))}}
	if _, err := store.PutParts(ctx, "big", bad, nil, v2.ETag); err == nil {
		t.Fatal("PutParts with an undersized first part succeeded")
	}
	if head, err := store.Head(ctx, "big"); err != nil || head.ETag != v2.ETag {
		t.Fatalf("object after a failed PutParts: %v, %v; want unchanged %s", head, err, v2.ETag)
	}
}

// putCutOffIsLostRaceOnlyIfChanged: a conditional Put that fails without
// a server answer is a lost race if the object no longer matches, and an
// ordinary failure if it still does.
func putCutOffIsLostRaceOnlyIfChanged(t *testing.T, newStore func(*testing.T) objstore.Store) {
	s := newStore(t)
	ctx := context.Background()
	obj, err := s.Put(ctx, "k", strings.NewReader("v1"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, "k", &failingReader{n: 1 << 10}, nil, `"stale"`)
	if !objstore.IsPreconditionFailed(err) {
		t.Fatalf("cut off, object changed: %v, want a precondition failure", err)
	}
	_, err = s.Put(ctx, "k", &failingReader{n: 1 << 10}, nil, obj.ETag)
	if err == nil || objstore.IsPreconditionFailed(err) {
		t.Fatalf("cut off, object unchanged: %v, want a plain failure", err)
	}
}

func historyPaginatesAndIncludesDeletions(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 2

	var ids []string
	for i := 0; i < 4; i++ {
		o, err := store.Put(ctx, "k", bytes.NewReader([]byte{byte('0' + i)}), nil, "")
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		ids = append(ids, o.VersionID)
	}
	if err := store.Delete(ctx, "k"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Put(ctx, "k.metadata", bytes.NewReader(nil), nil, ""); err != nil {
		t.Fatalf("put sibling: %v", err)
	}

	versions, err := store.History(ctx, "k")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 5 || !versions[0].Deleted {
		t.Fatalf("got %d versions (first a delete marker: %v), want 5 with the delete first", len(versions), len(versions) > 0 && versions[0].Deleted)
	}
	for i, v := range versions[1:] {
		if v.Deleted || v.VersionID != ids[3-i] {
			t.Fatalf("version %d = %+v, want %s", i+1, v, ids[3-i])
		}
	}
	all, err := store.HistoryUnder(ctx, "k")
	if err != nil || len(all) != 6 {
		t.Fatalf("all versions under k: %d, %v; want 6", len(all), err)
	}
	body, obj, err := store.GetVersion(ctx, "k", ids[1])
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "1" || obj.VersionID != ids[1] {
		t.Fatalf("version %s = %q", ids[1], data)
	}
}

// TestAsOf checks views at three moments, all over one history cache:
// before a key existed, between two writes, and after it was deleted,
// plus List, read-only writes, the future check and the temporary
// cache's cleanup.
func asOf(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()

	pause()
	before := serverNow(t, store)
	pause()
	if _, err := store.Put(ctx, "p/a", bytes.NewReader([]byte("one")), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "p/gone", bytes.NewReader([]byte("x")), nil, ""); err != nil {
		t.Fatal(err)
	}
	pause()
	middle := serverNow(t, store)
	pause()
	if _, err := store.Put(ctx, "p/a", bytes.NewReader([]byte("two!")), nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "p/gone"); err != nil {
		t.Fatal(err)
	}
	pause()
	after := serverNow(t, store)

	parent := t.TempDir()
	hist, err := objstore.OpenTempHistoryCache(ctx, store, parent)
	if err != nil {
		t.Fatal(err)
	}
	read := func(v *objstore.AsOf, key string) (string, error) {
		body, _, err := v.Get(ctx, key)
		if err != nil {
			return "", err
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		return string(data), err
	}
	for _, tc := range []struct {
		at         time.Time
		a, gone    string // "" = didn't exist
		listed     int
		listedSize int64
	}{
		{before, "", "", 0, 0},
		{middle, "one", "x", 2, 4},
		{after, "two!", "", 1, 4},
	} {
		v, err := objstore.NewAsOf(ctx, hist, tc.at)
		if err != nil {
			t.Fatalf("as of %v: %v", tc.at, err)
		}
		for key, want := range map[string]string{"p/a": tc.a, "p/gone": tc.gone} {
			for round := 0; round < 2; round++ { // the second answer comes from the cache
				got, err := read(v, key)
				if want == "" {
					if !objstore.IsNotFound(err) {
						t.Fatalf("as of %v, %s (round %d): %q, %v; want not found", tc.at, key, round, got, err)
					}
					if _, err := v.Head(ctx, key); !objstore.IsNotFound(err) {
						t.Fatalf("as of %v, head %s: %v; want not found", tc.at, key, err)
					}
				} else if got != want || err != nil {
					t.Fatalf("as of %v, %s (round %d): %q, %v; want %q", tc.at, key, round, got, err, want)
				}
			}
		}
		listed, err := v.List(ctx, "p/")
		var size int64
		for _, o := range listed {
			size += o.Size
		}
		if err != nil || len(listed) != tc.listed || size != tc.listedSize {
			t.Fatalf("as of %v, listing: %d objects, %d bytes, %v; want %d, %d", tc.at, len(listed), size, err, tc.listed, tc.listedSize)
		}
		if _, err := v.Put(ctx, "p/a", bytes.NewReader(nil), nil, ""); !errors.Is(err, objstore.ErrReadOnly) {
			t.Fatalf("put through an as-of view: %v", err)
		}
		if err := v.Delete(ctx, "p/a"); !errors.Is(err, objstore.ErrReadOnly) {
			t.Fatalf("delete through an as-of view: %v", err)
		}
	}

	if _, err := objstore.NewAsOf(ctx, hist, after.Add(time.Hour)); err == nil {
		t.Fatal("an as-of time in the future was accepted")
	}
	if err := hist.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if left, _ := os.ReadDir(parent); len(left) != 0 {
		t.Fatalf("the temporary cache is still there after Close: %v", left)
	}
}

// countingStore counts the history listings, and the reads of one
// version, that reach the store.
type countingStore struct {
	objstore.Store
	listings, versionReads int
}

func (s *countingStore) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *objstore.Object, error) {
	s.versionReads++
	return s.Store.GetVersion(ctx, key, versionID)
}

func (s *countingStore) HeadVersion(ctx context.Context, key, versionID string) (*objstore.Object, error) {
	s.versionReads++
	return s.Store.HeadVersion(ctx, key, versionID)
}

func (s *countingStore) History(ctx context.Context, key string) ([]objstore.Object, error) {
	s.listings++
	return s.Store.History(ctx, key)
}

func (s *countingStore) HistoryUnder(ctx context.Context, prefix string) ([]objstore.Object, error) {
	s.listings++
	return s.Store.HistoryUnder(ctx, prefix)
}

// historyCacheLastsForItsTTL checks that a history cache answers a view
// of another time, and a later process, without the store, until what it
// holds is older than its TTL.
func historyCacheLastsForItsTTL(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := &countingStore{Store: newStore(t)}
	ctx := context.Background()
	if _, err := store.Put(ctx, "p/a", bytes.NewReader([]byte("one")), nil, ""); err != nil {
		t.Fatal(err)
	}
	pause()
	middle := serverNow(t, store)
	pause()
	if _, err := store.Put(ctx, "p/a", bytes.NewReader([]byte("two")), nil, ""); err != nil {
		t.Fatal(err)
	}
	pause()
	now := serverNow(t, store)
	path := filepath.Join(t.TempDir(), "history.db")

	look := func(ttl time.Duration, at time.Time) int {
		t.Helper()
		hist, err := objstore.OpenHistoryCache(ctx, store, path, ttl)
		if err != nil {
			t.Fatal(err)
		}
		defer hist.Close()
		v, err := objstore.NewAsOf(ctx, hist, at)
		if err != nil {
			t.Fatal(err)
		}
		before := store.listings
		if _, err := v.Head(ctx, "p/a"); err != nil {
			t.Fatalf("as of %v: %v", at, err)
		}
		if listed, err := v.List(ctx, "p/"); err != nil || len(listed) != 1 {
			t.Fatalf("as of %v, listing: %v, %v", at, listed, err)
		}
		return store.listings - before
	}
	if n := look(time.Hour, now); n != 2 {
		t.Fatalf("an empty cache asked the store %d times; want 2", n)
	}
	if n := look(time.Hour, middle); n != 0 {
		t.Fatalf("a reopened cache, at an earlier time, asked the store %d times; want 0", n)
	}
	time.Sleep(10 * time.Millisecond)
	if n := look(time.Millisecond, middle); n != 2 {
		t.Fatalf("a cache past its TTL asked the store %d times; want 2", n)
	}
}

// asOfAfterErase checks that a view whose cached version has since been
// erased (a purge) asks the store again, rather than failing.
func asOfAfterErase(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := newStore(t)
	ctx := context.Background()
	first, err := store.Put(ctx, "k", bytes.NewReader([]byte("one")), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	pause()
	middle := serverNow(t, store)
	pause()
	if _, err := store.Put(ctx, "k", bytes.NewReader([]byte("two")), nil, ""); err != nil {
		t.Fatal(err)
	}
	pause()
	hist, err := objstore.OpenTempHistoryCache(ctx, store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	v, err := objstore.NewAsOf(ctx, hist, middle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Head(ctx, "k"); err != nil {
		t.Fatalf("before the erase: %v", err)
	}
	if err := store.Erase(ctx, "k", first.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.Get(ctx, "k"); !objstore.IsNotFound(err) {
		t.Fatalf("after its only version then was erased: %v; want not found", err)
	}
	later, err := objstore.NewAsOf(ctx, hist, serverNow(t, store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := later.Head(ctx, "k"); err != nil {
		t.Fatalf("the current version, after the erase: %v", err)
	}
}

// historyCacheKeepsVersions checks that views of different times read a
// version's metadata from the store once, and its content once if it's
// marked as metadata (CacheContent), but file data every time.
func historyCacheKeepsVersions(t *testing.T, newStore func(*testing.T) objstore.Store) {
	store := &countingStore{Store: newStore(t)}
	ctx := context.Background()
	if _, err := store.Put(ctx, "k", bytes.NewReader([]byte("block")), map[string]string{"m": "1"}, ""); err != nil {
		t.Fatal(err)
	}
	pause()
	first := serverNow(t, store)
	pause()
	second := serverNow(t, store)
	hist, err := objstore.OpenTempHistoryCache(ctx, store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	reads := func(get func(*objstore.AsOf) error) int {
		t.Helper()
		before := store.versionReads
		for _, at := range []time.Time{first, second, first} {
			v, err := objstore.NewAsOf(ctx, hist, at)
			if err != nil {
				t.Fatal(err)
			}
			if err := get(v); err != nil {
				t.Fatalf("as of %v: %v", at, err)
			}
		}
		return store.versionReads - before
	}
	if n := reads(func(v *objstore.AsOf) error {
		o, err := v.Head(ctx, "k")
		if err == nil && o.Metadata["m"] != "1" {
			err = fmt.Errorf("metadata %v", o.Metadata)
		}
		return err
	}); n != 1 {
		t.Fatalf("three heads read the store %d times; want 1", n)
	}
	read := func(ctx context.Context) func(*objstore.AsOf) error {
		return func(v *objstore.AsOf) error {
			body, _, err := v.Get(ctx, "k")
			if err != nil {
				return err
			}
			defer body.Close()
			if data, _ := io.ReadAll(body); string(data) != "block" {
				return fmt.Errorf("read %q", data)
			}
			return nil
		}
	}
	if n := reads(read(objstore.CacheContent(ctx))); n != 1 {
		t.Fatalf("three reads marked as metadata reached the store %d times; want 1", n)
	}
	if n := reads(read(ctx)); n != 3 {
		t.Fatalf("three reads of file data reached the store %d times; want 3", n)
	}
}
