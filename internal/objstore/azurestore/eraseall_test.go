package azurestore_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/azurestore"
)

// fakeBlobs is Azure Blob Storage for EraseAll and nothing else: container
// "c" holding n synthetic versions under "p/", three a blob, oldest first
// (as Azure lists them), the newest current except on every fifth blob
// (deleted: no current version). List Blobs with versions is served a page
// at a time (the marker is the next entry's index); Delete Blob ends a
// blob's current version, and Delete Blob with a versionid erases a version
// that isn't current; a current one is refused, as the store's Erase says
// Azure does. Its own state is a few bytes an entry, made before any
// measuring.
type fakeBlobs struct {
	n       int
	erased  []bool
	ended   []bool  // a current version ended by a Delete Blob
	deletes []uint8 // Delete Blob requests with this entry's versionid
	ends    []uint8 // Delete Blob requests that ended this entry
	ignore  bool    // accept version deletes and erase nothing
	low     int

	mu      sync.Mutex
	lists   atomic.Int64
	dels    atomic.Int64
	measure bool
	peak    uint64

	cutAfter int64 // cancel after this many deletes, if cancel is set
	cancel   context.CancelFunc
}

const fakeAccount = "devstoreaccount"

func (f *fakeBlobs) name(i int) string { return fmt.Sprintf("p/%07d", i/3) }
func (f *fakeBlobs) version(i int) string {
	return fmt.Sprintf("2026-01-01T00:00:00.%07dZ", i)
}
func (f *fakeBlobs) current(i int) bool { return i%3 == 2 && (i/3)%5 != 0 && !f.ended[i] }

func (f *fakeBlobs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/"+fakeAccount+"/c" && q.Get("comp") == "list":
		f.lists.Add(1)
		if f.measure {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			f.peak = max(f.peak, ms.HeapAlloc)
		}
		if !strings.Contains(q.Get("include"), "versions") {
			http.Error(w, "want versions", http.StatusBadRequest)
			return
		}
		f.list(w, q.Get("prefix"), q.Get("maxresults"), q.Get("marker"))
	case r.Method == http.MethodDelete:
		defer func() {
			if f.dels.Add(1) == f.cutAfter && f.cancel != nil {
				f.cancel()
			}
		}()
		name := strings.TrimPrefix(r.URL.Path, "/"+fakeAccount+"/c/")
		if v := q.Get("versionid"); v != "" {
			var i int
			if _, err := fmt.Sscanf(v, "2026-01-01T00:00:00.%07dZ", &i); err != nil || i < 0 || i >= f.n || f.name(i) != name {
				http.Error(w, "bad delete "+r.URL.String(), http.StatusBadRequest)
				return
			}
			f.deletes[i] = min(f.deletes[i]+1, 255)
			switch {
			case f.erased[i]:
				azureError(w, http.StatusNotFound, "BlobNotFound")
			case f.current(i):
				azureError(w, http.StatusForbidden, "OperationNotAllowedOnRootBlob")
			default:
				if !f.ignore {
					f.erased[i] = true
				}
				w.WriteHeader(http.StatusAccepted)
			}
			return
		}
		k, err := strconv.Atoi(strings.TrimPrefix(name, "p/"))
		if err != nil || !strings.HasPrefix(name, "p/") || 3*k+2 >= f.n {
			http.Error(w, "bad delete "+r.URL.String(), http.StatusBadRequest)
			return
		}
		i := 3*k + 2
		if f.erased[i] || !f.current(i) {
			azureError(w, http.StatusNotFound, "BlobNotFound")
			return
		}
		f.ended[i] = true
		f.ends[i] = min(f.ends[i]+1, 255)
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	}
}

func azureError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("x-ms-error-code", code)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>fake</Message></Error>`, code)
}

func (f *fakeBlobs) list(w http.ResponseWriter, prefix, maxResults, marker string) {
	limit, _ := strconv.Atoi(maxResults)
	if limit <= 0 {
		limit = 5000
	}
	start := 0
	if marker != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(marker, "i"))
	}
	for f.low < f.n && f.erased[f.low] {
		f.low++
	}
	start = max(start, f.low)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><EnumerationResults ServiceEndpoint="http://fake/%s/" ContainerName="c"><Prefix>%s</Prefix><MaxResults>%d</MaxResults><Blobs>`, fakeAccount, prefix, limit)
	i, count := start, 0
	if prefix == "p/" {
		for ; i < f.n && count < limit; i++ {
			if f.erased[i] {
				continue
			}
			cur := ""
			if f.current(i) {
				cur = `<IsCurrentVersion>true</IsCurrentVersion>`
			}
			fmt.Fprintf(w, `<Blob><Name>%s</Name><VersionId>%s</VersionId>%s<Properties><Content-Length>1</Content-Length><BlobType>BlockBlob</BlobType></Properties></Blob>`,
				f.name(i), f.version(i), cur)
			count++
		}
		for i < f.n && f.erased[i] {
			i++
		}
	} else {
		i = f.n
	}
	io.WriteString(w, `</Blobs>`)
	if i < f.n {
		fmt.Fprintf(w, `<NextMarker>i%d</NextMarker>`, i)
	} else {
		io.WriteString(w, `<NextMarker/>`)
	}
	io.WriteString(w, `</EnumerationResults>`)
}

func newFakeBlobs(n int) *fakeBlobs {
	return &fakeBlobs{n: n, erased: make([]bool, n), ended: make([]bool, n), deletes: make([]uint8, n), ends: make([]uint8, n)}
}

// fakeStore is an AzureStore on f, under the key with an obviously fake
// account key.
func fakeStore(t *testing.T, f *fakeBlobs) *azurestore.Store {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	key := base64.StdEncoding.EncodeToString([]byte("not-a-real-key-not-a-real-key!!!"))
	s, err := azurestore.New(srv.URL+"/"+fakeAccount+"/", fakeAccount, key, "c")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// allErasedOnce checks every version was erased by exactly one delete
// with its versionid, and every current version ended by exactly one.
func (f *fakeBlobs) allErasedOnce(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.n {
		wantEnds := uint8(0)
		if i%3 == 2 && (i/3)%5 != 0 {
			wantEnds = 1
		}
		if !f.erased[i] || f.deletes[i] != 1 || f.ends[i] != wantEnds {
			t.Fatalf("entry %d (%s %s): erased %t by %d deletes, ended %d times; want erased by one, ended %d", i, f.name(i), f.version(i), f.erased[i], f.deletes[i], f.ends[i], wantEnds)
		}
	}
}

type eraseRun struct {
	peak     uint64
	perEntry float64
	lists    int64
	deletes  int64
}

// TestEraseAllHoldsOnePage is azurestore's: erasing ten times as many
// versions holds no more, and allocates the same an entry. It runs at
// about 2,000 then 20,000 versions; at full size (10,000 then 100,000)
// with ICBFS_ERASE_FULL=1, which just ci sets in a step of its own after
// the other tests (test-erase-full).
func TestEraseAllHoldsOnePage(t *testing.T) {
	small, large := 2_001, 20_001 // whole blobs of three versions
	if os.Getenv("ICBFS_ERASE_FULL") == "1" && !testing.Short() {
		small, large = 10_002, 100_002
	}
	a := eraseFakeUnder(t, small)
	b := eraseFakeUnder(t, large)
	t.Logf("%d versions: peak live heap +%d KiB, %.0f bytes allocated an entry, %d lists, %d deletes", small, a.peak>>10, a.perEntry, a.lists, a.deletes)
	t.Logf("%d versions: peak live heap +%d KiB, %.0f bytes allocated an entry, %d lists, %d deletes", large, b.peak>>10, b.perEntry, b.lists, b.deletes)
	if b.peak > a.peak+1<<20 {
		t.Errorf("peak live heap grew with the versions: +%d KiB at %d, +%d KiB at %d; want within 1 MiB", a.peak>>10, small, b.peak>>10, large)
	}
	if b.perEntry > 1.25*a.perEntry+64 {
		t.Errorf("allocated %.0f bytes an entry at %d, %.0f at %d; want about the same", a.perEntry, small, b.perEntry, large)
	}
	page := int64(objstore.HistoryPageSize)
	for _, r := range []struct {
		n   int
		run eraseRun
	}{{small, a}, {large, b}} {
		if want := (int64(r.n) + page - 1) / page; r.run.lists > want+1 {
			t.Errorf("%d versions: %d listing requests, want at most %d", r.n, r.run.lists, want+1)
		}
	}
}

// eraseFakeUnder is eraseFake erasing under "p/", the fake's prefix.
func eraseFakeUnder(t *testing.T, n int) eraseRun {
	t.Helper()
	f := newFakeBlobs(n)
	s := fakeStore(t, f)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	f.mu.Lock()
	f.measure = true
	f.mu.Unlock()
	if err := s.EraseAll(context.Background(), "p/"); err != nil {
		t.Fatalf("erase all of %d: %v", n, err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	f.allErasedOnce(t)
	run := eraseRun{perEntry: float64(after.TotalAlloc-before.TotalAlloc) / float64(n), lists: f.lists.Load(), deletes: f.dels.Load()}
	if f.peak > before.HeapAlloc {
		run.peak = f.peak - before.HeapAlloc
	}
	return run
}

// TestEraseAllStopsWhenAnEraseDoesNothing: a store that accepts the
// deletes and keeps the versions gets an error back, not a loop.
func TestEraseAllStopsWhenAnEraseDoesNothing(t *testing.T) {
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 4
	f := newFakeBlobs(30)
	f.ignore = true
	s := fakeStore(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.EraseAll(ctx, "p/")
	if err == nil || ctx.Err() != nil {
		t.Fatalf("erase all on a store that keeps what it's told to erase: %v (context %v); want an error of its own", err, ctx.Err())
	}
	if f.lists.Load() > 3 {
		t.Fatalf("%d listings before giving up; want it to notice on the next", f.lists.Load())
	}
	t.Logf("%v", err)
}

// TestEraseAllCutAndRerun: an EraseAll cut part way, at every point
// across the first pages (its context cancelled after k deletes, four
// versions a page, so a blob's current version is often on a later page
// than its older ones), leaves what it hadn't reached; a second EraseAll
// erases the rest, each version by one delete.
func TestEraseAllCutAndRerun(t *testing.T) {
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 4
	for k := int64(1); k <= 14; k++ {
		f := newFakeBlobs(30)
		s := fakeStore(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		f.cutAfter, f.cancel = k, cancel
		err := s.EraseAll(ctx, "p/")
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cut after %d deletes: %v, want it cancelled", k, err)
		}
		f.mu.Lock()
		f.cancel = nil
		left := 0
		for i := range f.n {
			if !f.erased[i] {
				left++
			}
		}
		f.mu.Unlock()
		if left == 0 {
			t.Fatalf("cut after %d deletes: no versions left; want the cut to have stopped it", k)
		}
		if err := s.EraseAll(context.Background(), "p/"); err != nil {
			t.Fatalf("rerun after a cut at %d: %v", k, err)
		}
		f.allErasedOnce(t)
	}
}
