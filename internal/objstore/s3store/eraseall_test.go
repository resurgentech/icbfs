package s3store_test

import (
	"context"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
)

// fakeVersions is S3 for EraseAll and nothing else: a bucket "b" holding
// n synthetic entries under "p/", three per key, newest first (as S3 lists
// them), every fifth key's newest a delete marker; ListObjectVersions
// served a page at a time, DeleteObject with a versionId accepted and
// counted per entry. Its own state is two bytes an entry, made before any
// measuring, so what the heap does while EraseAll runs is EraseAll's.
type fakeVersions struct {
	n       int
	erased  []bool
	deletes []uint8 // DeleteObject requests per entry
	ignore  bool    // accept deletes and erase nothing
	low     int     // no entry before this is left

	mu      sync.Mutex
	lists   atomic.Int64
	dels    atomic.Int64
	measure bool
	peak    uint64 // highest live heap seen at a listing
}

func (f *fakeVersions) key(i int) string { return fmt.Sprintf("p/%07d", i/3) }
func (f *fakeVersions) version(i int) string {
	return fmt.Sprintf("v%07d", i)
}
func (f *fakeVersions) marker(i int) bool { return i%3 == 0 && (i/3)%5 == 0 }

func (f *fakeVersions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Has("versions"):
		f.lists.Add(1)
		if f.measure {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			f.peak = max(f.peak, ms.HeapAlloc)
		}
		f.list(w, q)
	case r.Method == http.MethodDelete:
		f.dels.Add(1)
		i, err := strconv.Atoi(strings.TrimPrefix(q.Get("versionId"), "v"))
		if err != nil || i < 0 || i >= f.n || "/b/"+f.key(i) != r.URL.Path {
			http.Error(w, "bad delete "+r.URL.String(), http.StatusBadRequest)
			return
		}
		if f.deletes[i] < 255 {
			f.deletes[i]++
		}
		if f.erased[i] {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<Error><Code>NoSuchVersion</Code><Message>gone</Message></Error>`)
			return
		}
		if !f.ignore {
			f.erased[i] = true
		}
		w.Header().Set("x-amz-version-id", f.version(i))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
	}
}

func (f *fakeVersions) list(w http.ResponseWriter, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	maxKeys, _ := strconv.Atoi(get("max-keys"))
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	start := 0
	switch km, vm := get("key-marker"), get("version-id-marker"); {
	case vm != "":
		i, _ := strconv.Atoi(strings.TrimPrefix(vm, "v"))
		start = i + 1
	case km != "":
		for start < f.n && f.key(start) <= km {
			start++
		}
	}
	for f.low < f.n && f.erased[f.low] {
		f.low++
	}
	start = max(start, f.low)
	var versions, markers strings.Builder
	last, count, i := -1, 0, start
	for ; i < f.n && count < maxKeys; i++ {
		if f.erased[i] {
			continue
		}
		latest := i%3 == 0
		if f.marker(i) {
			fmt.Fprintf(&markers, `<DeleteMarker><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%t</IsLatest><LastModified>2026-01-01T00:00:00.000Z</LastModified></DeleteMarker>`,
				f.key(i), f.version(i), latest)
		} else {
			fmt.Fprintf(&versions, `<Version><Key>%s</Key><VersionId>%s</VersionId><IsLatest>%t</IsLatest><LastModified>2026-01-01T00:00:00.000Z</LastModified><ETag>"e%d"</ETag><Size>1</Size><StorageClass>STANDARD</StorageClass></Version>`,
				f.key(i), f.version(i), latest, i)
		}
		last, count = i, count+1
	}
	more := false
	for ; i < f.n; i++ {
		if !f.erased[i] {
			more = true
			break
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>b</Name><Prefix>p/</Prefix><MaxKeys>%d</MaxKeys><IsTruncated>%t</IsTruncated>`, maxKeys, more)
	if more && last >= 0 {
		fmt.Fprintf(w, `<NextKeyMarker>%s</NextKeyMarker><NextVersionIdMarker>%s</NextVersionIdMarker>`, f.key(last), f.version(last))
	}
	io.WriteString(w, versions.String())
	io.WriteString(w, markers.String())
	io.WriteString(w, `</ListVersionsResult>`)
}

// fakeStore is an S3Store on f, with obviously fake keys.
func fakeStore(t *testing.T, f *fakeVersions) *s3store.Store {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider("test", "test", ""),
	})
	return s3store.New(client, "b")
}

func newFake(n int) *fakeVersions {
	return &fakeVersions{n: n, erased: make([]bool, n), deletes: make([]uint8, n)}
}

type eraseRun struct {
	peak     uint64 // live heap above the start, at its highest
	perEntry float64
	lists    int64
	deletes  int64
}

// eraseFake erases a fake of n entries, checks every entry was erased by
// exactly one delete, and says what it held and asked for.
func eraseFake(t *testing.T, n int) eraseRun {
	t.Helper()
	f := newFake(n)
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
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.n {
		if !f.erased[i] || f.deletes[i] != 1 {
			t.Fatalf("entry %d (%s %s): erased %t by %d deletes; want once", i, f.key(i), f.version(i), f.erased[i], f.deletes[i])
		}
	}
	run := eraseRun{
		perEntry: float64(after.TotalAlloc-before.TotalAlloc) / float64(n),
		lists:    f.lists.Load(),
		deletes:  f.dels.Load(),
	}
	if f.peak > before.HeapAlloc {
		run.peak = f.peak - before.HeapAlloc
	}
	return run
}

// TestEraseAllHoldsOnePage: erasing ten times as many entries holds no
// more: the live heap at its highest stays within a page's worth of what
// a tenth as many held, and what is allocated per entry stays the same.
// It runs at 2,000 then 20,000 entries; at full size (10,000 then
// 100,000) with ICBFS_ERASE_FULL=1, which just ci sets in a step of its
// own after the other tests (test-erase-full), so its load does not slow
// the timing tests running beside it.
func TestEraseAllHoldsOnePage(t *testing.T) {
	small, large := 2_000, 20_000
	if os.Getenv("ICBFS_ERASE_FULL") == "1" && !testing.Short() {
		small, large = 10_000, 100_000
	}
	a := eraseFake(t, small)
	b := eraseFake(t, large)
	page := int64(objstore.HistoryPageSize)
	t.Logf("%d entries: peak live heap +%d KiB, %.0f bytes allocated an entry, %d lists, %d deletes", small, a.peak>>10, a.perEntry, a.lists, a.deletes)
	t.Logf("%d entries: peak live heap +%d KiB, %.0f bytes allocated an entry, %d lists, %d deletes", large, b.peak>>10, b.perEntry, b.lists, b.deletes)
	if b.peak > a.peak+1<<20 {
		t.Errorf("peak live heap grew with the entries: +%d KiB at %d, +%d KiB at %d; want within 1 MiB", a.peak>>10, small, b.peak>>10, large)
	}
	if b.perEntry > 1.25*a.perEntry+64 {
		t.Errorf("allocated %.0f bytes an entry at %d, %.0f at %d; want about the same", a.perEntry, small, b.perEntry, large)
	}
	for _, r := range []struct {
		n   int
		run eraseRun
	}{{small, a}, {large, b}} {
		if want := (int64(r.n) + page - 1) / page; r.run.lists > want+1 {
			t.Errorf("%d entries: %d listing requests, want at most %d", r.n, r.run.lists, want+1)
		}
		if r.run.deletes != int64(r.n) {
			t.Errorf("%d entries: %d deletes, want one each", r.n, r.run.deletes)
		}
	}
}

// TestEraseAllStopsWhenAnEraseDoesNothing: a store that accepts the
// deletes and keeps the versions gets an error back, not a loop.
func TestEraseAllStopsWhenAnEraseDoesNothing(t *testing.T) {
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 4
	f := newFake(30)
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

// cutTransport cancels after its after'th DELETE has been answered.
type cutTransport struct {
	next    http.RoundTripper
	after   int64
	deletes atomic.Int64
	cancel  context.CancelFunc
}

func (c *cutTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	if r.Method == http.MethodDelete && c.deletes.Add(1) == c.after && c.cancel != nil {
		c.cancel()
	}
	return resp, err
}

// TestEraseAllCutAndRerun, on MinIO: an EraseAll cut part way (its
// context cancelled after some erases, at two entries a page) leaves the
// entries it hadn't reached and nothing outside the prefix touched; a
// second EraseAll finishes it.
func TestEraseAllCutAndRerun(t *testing.T) {
	defer func(n int32) { objstore.HistoryPageSize = n }(objstore.HistoryPageSize)
	objstore.HistoryPageSize = 2
	_, srv := miniotest.Start(t)
	cut := &cutTransport{next: http.DefaultTransport, after: 3}
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, ""),
		HTTPClient:  &http.Client{Transport: cut},
	})
	store := s3store.New(client, srv.Bucket).WithKeyPrefix("cut/")
	ctx := context.Background()
	for _, kv := range [][2]string{{"p/a", "1"}, {"p/a", "2"}, {"p/a", "3"}, {"p/b", "1"}, {"p/b", "2"}, {"p/c", "1"}, {"q", "keep"}, {"pq", "keep"}} {
		if _, err := store.Put(ctx, kv[0], strings.NewReader(kv[1]), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Delete(ctx, "p/b"); err != nil {
		t.Fatal(err)
	}
	before, err := store.HistoryUnder(ctx, "p/")
	if err != nil || len(before) != 7 {
		t.Fatalf("history under p/: %d entries, %v; want 7", len(before), err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cut.deletes.Store(0)
	cut.cancel = cancel
	err = store.EraseAll(cctx, "p/")
	cut.cancel = nil
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cut erase all: %v, want it cancelled", err)
	}
	left, err := store.HistoryUnder(ctx, "p/")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != len(before)-3 {
		t.Fatalf("after a cut at 3 erases: %d of %d entries left, want %d", len(left), len(before), len(before)-3)
	}
	for _, k := range []string{"q", "pq"} {
		if body, _, err := store.Get(ctx, k); err != nil {
			t.Fatalf("%s after a cut erase of p/: %v", k, err)
		} else {
			body.Close()
		}
	}

	if err := store.EraseAll(ctx, "p/"); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if h, err := store.HistoryUnder(ctx, "p/"); err != nil || len(h) != 0 {
		t.Fatalf("history under p/ after the rerun: %+v, %v; want none", h, err)
	}
	for _, k := range []string{"q", "pq"} {
		if h, err := store.History(ctx, k); err != nil || len(h) != 1 {
			t.Fatalf("%s's history after erasing p/: %+v, %v; want its one version", k, h, err)
		}
	}
}
