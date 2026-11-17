package icbfsdrive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestRebuildRate (interloc T82-9d, step 0) measures a mirror's
// RebuildHashes over the large-Upstream gate's shape (T82-5, luScaled)
// with every store request held a set latency (production's store is
// tens of milliseconds away): files a second, store requests a file by
// kind, and the most requests in flight at once. It also measures a walk
// alone of the same tree, on a drive of its own, so the walk's share
// shows. A measurement, not a gate: it runs only when
// ICBFS_REBUILD_RATE names the scale (1 is the full 96,775 files).
//
//	ICBFS_REBUILD_RATE        the scale of luFull (e.g. 0.1)
//	ICBFS_REBUILD_LATENCY_MS  the latency of each store request, ms (110)
//	ICBFS_REBUILD_MIN_RATE    files a second the rebuild must reach (0: none)
//	ICBFS_REBUILD_TRUST_FOR   the drive's Config.TrustFor (0, as
//	                          icbfs-library-sync's; icbfs-clio's replay sets 10m)
//	ICBFS_REBUILD_IDENTITY    set: also rebuild the map as before interloc
//	                          T82-9d (rebuildBefore, at no latency) and
//	                          hold the rebuilt map to it, row for row
func TestRebuildRate(t *testing.T) {
	scaleEnv := os.Getenv("ICBFS_REBUILD_RATE")
	if scaleEnv == "" {
		t.Skip("a measurement: set ICBFS_REBUILD_RATE to the scale (e.g. 0.1)")
	}
	if testing.Short() {
		t.Skip("a measurement, not under -short")
	}
	scale, err := strconv.ParseFloat(scaleEnv, 64)
	if err != nil || scale <= 0 {
		t.Fatalf("ICBFS_REBUILD_RATE=%q: a scale over 0", scaleEnv)
	}
	latency := 110 * time.Millisecond
	if v := os.Getenv("ICBFS_REBUILD_LATENCY_MS"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 0 {
			t.Fatalf("ICBFS_REBUILD_LATENCY_MS=%q: whole milliseconds, 0 or more", v)
		}
		latency = time.Duration(ms) * time.Millisecond
	}
	var minRate float64
	if v := os.Getenv("ICBFS_REBUILD_MIN_RATE"); v != "" {
		if minRate, err = strconv.ParseFloat(v, 64); err != nil {
			t.Fatalf("ICBFS_REBUILD_MIN_RATE=%q: %v", v, err)
		}
	}
	var trustFor time.Duration
	if v := os.Getenv("ICBFS_REBUILD_TRUST_FOR"); v != "" {
		if trustFor, err = time.ParseDuration(v); err != nil {
			t.Fatalf("ICBFS_REBUILD_TRUST_FOR=%q: %v", v, err)
		}
	}

	ctx := system()
	store, srv := miniotest.Start(t)
	s := icbfs.Settings{Name: "rebuildrate", Size: 1 << 40, RootMode: 0o777}
	began := time.Now()
	rt := buildLUShape(t, store, s, luScaled(scale))
	t.Logf("built scale %g: %d folders, %d files (no /Large, no big file; one version each) in %v at no latency",
		scale, rt.Folders, rt.Files, time.Since(began).Round(time.Millisecond))
	cfg := Config{TrustFor: trustFor} // DirCacheBytes 0: DefaultDirCacheBytes, as every daemon has

	// The walk alone, on a fresh drive.
	wd, wp := proxiedDrive(t, srv, s, latency, cfg)
	wp.Reset()
	folders, walked := 0, 0
	began = time.Now()
	err = drive.Walk(ctx, wd, drive.ByPath("/"), drive.WalkOptions{}, func(_ drive.ItemRef, its []drive.Item) error {
		folders++
		walked += len(its)
		return nil
	})
	walkTime := time.Since(began)
	if err != nil {
		t.Fatal(err)
	}
	ws := wp.Stats()
	t.Logf("walk alone: %d folders, %d items in %v at %v a request", folders, walked, walkTime.Round(time.Millisecond), latency)
	logStats(t, "walk alone", ws, rt.Files)

	// The rebuild, on another fresh drive: its cache cold.
	d, p := proxiedDrive(t, srv, s, latency, cfg)
	var items, files int
	m := drive.NewMirror(d, srcDrive).OnRebuildProgress(func(i, f int) { items, files = i, f })
	p.Reset()
	began = time.Now()
	if err := m.RebuildHashes(ctx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(began)
	st := p.Stats()
	entries := m.Hashes().Len()
	rate := float64(files) / elapsed.Seconds()
	t.Logf("rebuild: %d files (map %d entries), %d items, %d folders in %v at %v a request: %.1f files/s",
		files, entries, items, rt.Folders, elapsed.Round(time.Millisecond), latency, rate)
	logStats(t, "rebuild", st, rt.Files)
	if files > 0 {
		t.Logf("rebuild less the walk: %.2f requests a file", float64(st.Total-ws.Total)/float64(files))
	}

	if entries != rt.Files {
		t.Errorf("the map has %d entries, want one a file: %d", entries, rt.Files)
	}
	if files != rt.Files {
		t.Errorf("the rebuild read %d files' versions, want %d", files, rt.Files)
	}
	if st.Errors > 0 || ws.Errors > 0 {
		t.Errorf("the proxy failed %d requests in the rebuild, %d in the walk", st.Errors, ws.Errors)
	}
	t.Logf("rebuild: peak store requests in flight %d (8 files' versions at once, beside the walk's 16 stats)", st.MaxInFlight)
	if minRate > 0 && rate < minRate {
		t.Errorf("the rebuild ran %.1f files/s, under ICBFS_REBUILD_MIN_RATE %g", rate, minRate)
	}

	// The map as the rebuild made it before interloc T82-9d, a file's
	// Versions at a time, on a drive of its own at no latency.
	if os.Getenv("ICBFS_REBUILD_IDENTITY") != "" {
		rd, _ := proxiedDrive(t, srv, s, 0, cfg)
		began = time.Now()
		want, err := rebuildBefore(ctx, rd, srcDrive)
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.Hashes().MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := want.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("the map as before: %d entries in %v at no latency", want.Len(), time.Since(began).Round(time.Millisecond))
		if !bytes.Equal(got, wantJSON) {
			t.Errorf("the rebuilt map (%d entries) isn't the one rebuilt as before (%d entries)", entries, want.Len())
		}
	}
}

// logStats logs a proxy's counts: the total, a file's share (of files),
// the most in flight, and by kind, the most first.
func logStats(t *testing.T, what string, st proxyStats, files int) {
	t.Helper()
	per := func(n int64) float64 { return float64(n) / float64(max(files, 1)) }
	t.Logf("%s: %d requests, %.2f a file, max in flight %d, errors %d", what, st.Total, per(st.Total), st.MaxInFlight, st.Errors)
	kinds := make([]string, 0, len(st.ByKind))
	for k := range st.ByKind {
		kinds = append(kinds, k)
	}
	slices.SortFunc(kinds, func(a, b string) int {
		if c := st.ByKind[b] - st.ByKind[a]; c != 0 {
			return int(c)
		}
		return strings.Compare(a, b)
	})
	var b strings.Builder
	for _, k := range kinds {
		fmt.Fprintf(&b, "; %s %d (%.2f)", k, st.ByKind[k], per(st.ByKind[k]))
	}
	t.Logf("%s by kind (a file's share):%s", what, strings.TrimPrefix(b.String(), ";"))
}

// The large Upstream's shape and layout, copied from interloc's
// tests/large-upstream/internal/seed/layout.go (T82-5: Shape, Full,
// Scaled, Generate, FilePath; another module), itself T80-9's
// (connectors/clio/internal/mirror/memory_test.go). Left out: the big
// file and its folder /Large, the documents' bodies and the manifest.

// luShape is a generated tree's shape.
type luShape struct {
	Folders int // folders, the top's not counted
	Files   int
	Biggest int // children (folders and files) of the largest folder
	Depth   int // levels of folders at most: the depth chain's length
	AvgPath int // a file path's average length, in bytes
}

// luFull is the shape of the production mirror that ran out of memory on
// 2026-11-13: 4,694 folders, 96,775 files, 2,142 children in the largest
// folder, 9 levels, file paths of 130 bytes on average.
var luFull = luShape{Folders: 4694, Files: 96775, Biggest: 2142, Depth: 9, AvgPath: 130}

// luScaled is luFull with its folders, files and largest folder
// multiplied by scale (rounded), its depth and path length kept.
func luScaled(scale float64) luShape {
	s := luFull
	s.Folders = int(math.Round(float64(s.Folders) * scale))
	s.Files = int(math.Round(float64(s.Files) * scale))
	s.Biggest = int(math.Round(float64(s.Biggest) * scale))
	return s
}

// luFolder is one folder of the tree.
type luFolder struct {
	Parent    int // index in luTree.Folders; -1 for a folder at the top
	Depth     int // 1 for a folder at the top
	Name      string
	Path      string
	Files     int // the files directly in it
	FirstFile int // the number of its first file; the rest follow
}

// luTree is a laid-out shape. Folders come parents first.
type luTree struct {
	luShape
	Folders  []*luFolder
	FileName int // a file name's length
	Big      int // the largest folder's index
}

// luGenerate lays out shape as seed.Generate does: a chain of
// shape.Depth folders, the rest a tree three wide under the top, and one
// folder at depth 4 (the first not in the chain) holding shape.Biggest
// children; the other folders share the remaining files evenly.
func luGenerate(shape luShape) (*luTree, error) {
	if shape.Depth < 4 {
		return nil, fmt.Errorf("shape %+v: the depth is under 4, so there is no folder at depth 4 for the biggest", shape)
	}
	const dirName = 12
	pad := func(s string, n int) string {
		if len(s) >= n {
			return s
		}
		return s + strings.Repeat("x", n-len(s))
	}
	var folders []*luFolder
	for i := 0; i < shape.Depth && i < shape.Folders; i++ {
		folders = append(folders, &luFolder{Parent: i - 1, Depth: i + 1})
	}
	heap := []int{} // the rest: parent of k is (k-3)/3 among them
	for len(folders) < shape.Folders {
		k := len(heap)
		f := &luFolder{Parent: -1, Depth: 1}
		if k >= 3 {
			p := heap[(k-3)/3]
			f.Parent, f.Depth = p, folders[p].Depth+1
		}
		heap = append(heap, len(folders))
		folders = append(folders, f)
	}
	subs := make([]int, len(folders))
	for i, f := range folders {
		f.Name = pad(fmt.Sprintf("f%05d-", i), dirName)
		if f.Parent >= 0 {
			subs[f.Parent]++
			f.Path = folders[f.Parent].Path + "/" + f.Name
		} else {
			f.Path = "/" + f.Name
		}
	}
	big := -1
	for i, f := range folders {
		if f.Depth == 4 && i >= shape.Depth {
			big = i
			break
		}
	}
	if big < 0 {
		return nil, fmt.Errorf("shape %+v: too few folders for one at depth 4 outside the chain (at least %d)", shape, shape.Depth+40)
	}
	own := shape.Biggest - subs[big]
	if own < 1 || own > shape.Files {
		return nil, fmt.Errorf("shape %+v: the biggest folder would hold %d files", shape, own)
	}
	folders[big].Files = own
	rest := shape.Files - own
	others := len(folders) - 1
	n := 0
	for i, f := range folders {
		if i == big {
			continue
		}
		f.Files = rest / others
		if n < rest%others {
			f.Files++
		}
		n++
	}
	var dirLen, count int
	for _, f := range folders {
		f.FirstFile = count
		dirLen += f.Files * len(f.Path)
		count += f.Files
	}
	fileName := max(shape.AvgPath-1-dirLen/count, 16)
	return &luTree{luShape: shape, Folders: folders, FileName: fileName, Big: big}, nil
}

// fileName is the name of the k-th file of folder i: "d" and its number
// (six digits), padded with y to the tree's file name length, ".md".
func (t *luTree) fileName(i, k int) string {
	name := fmt.Sprintf("d%06d-", t.Folders[i].FirstFile+k)
	return name + strings.Repeat("y", max(t.FileName-len(name)-3, 0)) + ".md"
}

// filePath is the path of the k-th file of folder i (seed's FilePath).
func (t *luTree) filePath(i, k int) string {
	return t.Folders[i].Path + "/" + t.fileName(i, k)
}

// luBuilt is what buildLUShape made.
type luBuilt struct {
	Tree    *luTree
	Folders int
	Files   int
}

// luContent is file number g's content: a few bytes, its own.
func luContent(g int) []byte { return fmt.Appendf(nil, "content %d\n", g) }

// buildLUShape makes shape's tree in filesystem s of store (created if
// it isn't there), at whatever latency store has: the folders parents
// first, one at a time; then each folder's files, one after another, 32
// folders at once, straight on the filesystem with their content, their
// checksums and their origin in srcDrive (treeOrigin) in the create, as
// a mirror's copy records them. Each file has one version, as a file a
// mirror copied once has.
func buildLUShape(t *testing.T, store objstore.Store, s icbfs.Settings, shape luShape) luBuilt {
	t.Helper()
	tr, err := luGenerate(shape)
	if err != nil {
		t.Fatal(err)
	}
	ctx := system()
	fs := newDrive(t, store, s, "").Filesystem()
	keys := make([]string, len(tr.Folders))
	for i, f := range tr.Folders {
		parent := fs.RootKey()
		if f.Parent >= 0 {
			parent = keys[f.Parent]
		}
		if keys[i], _, err = fs.Mkdir(ctx, parent, f.Name, 0o755, 0, 0); err != nil {
			t.Fatalf("mkdir %s: %v", f.Path, err)
		}
	}
	work := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	for range 32 {
		wg.Go(func() {
			for i := range work {
				f := tr.Folders[i]
				for k := range f.Files {
					g := f.FirstFile + k
					name := tr.fileName(i, k)
					data := luContent(g)
					sums, err := icbfs.ChecksumsOf(bytes.NewReader(data))
					var origin []byte
					if err == nil {
						origin, err = json.Marshal(treeOrigin(g, 1, f.Path+"/"+name))
					}
					if err == nil {
						info := icbfs.ItemInfo{App: map[string][]byte{icbfs.AppDriveOrigin: origin}, Checksums: &sums}
						_, _, err = fs.CreateWith(ctx, keys[i], name, 0o644, 0, 0, data, nil, info)
					}
					if err != nil {
						mu.Lock()
						failed = fmt.Errorf("%s/%s: %w", f.Path, name, err)
						mu.Unlock()
						return
					}
				}
			}
		})
	}
	for i := range tr.Folders {
		work <- i
	}
	close(work)
	wg.Wait()
	if failed != nil {
		t.Fatal(failed)
	}
	files := 0
	for _, f := range tr.Folders {
		files += f.Files
	}
	return luBuilt{Tree: tr, Folders: len(tr.Folders), Files: files}
}

// proxiedDrive is a fresh drive (its caches cold) on filesystem s of the
// MinIO srv, its store's every request through a latencyProxy of its own
// that holds it latency first. The proxy's counts include the drive's
// opening: Reset it before what is measured.
func proxiedDrive(t *testing.T, srv miniotest.Server, s icbfs.Settings, latency time.Duration, cfg Config) (*Drive, *latencyProxy) {
	t.Helper()
	p, err := newLatencyProxy(srv.Endpoint, latency)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(p)
	t.Cleanup(hs.Close)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(hs.URL),
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, ""),
	})
	return newDriveWith(t, s3store.New(client, srv.Bucket), s, cfg), p
}

// latencyProxy is a reverse proxy to a store that holds every request a
// set latency before it forwards it, and counts what passes: the counting
// of interloc's tests/large-upstream/internal/storeproxy/proxy.go
// (lu-storeproxy, T82-5: Kind, count, ServeHTTP), copied so the counts
// are the same thing; without its byte counts and control listener.
type latencyProxy struct {
	rp      *httputil.ReverseProxy
	latency time.Duration

	mu          sync.Mutex
	total       int64
	byMethod    map[string]int64
	byKind      map[string]int64
	inFlight    int64
	maxInFlight int64
	errors      int64
}

// proxyStats are a latencyProxy's counts.
type proxyStats struct {
	Total       int64
	ByMethod    map[string]int64
	ByKind      map[string]int64 // by storeKind
	InFlight    int64
	MaxInFlight int64
	Errors      int64
}

// newLatencyProxy is a proxy to the store at endpoint (http://host:port).
// The request goes on with the Host the client sent: S3's SigV4 signs it.
func newLatencyProxy(endpoint string, latency time.Duration) (*latencyProxy, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	p := &latencyProxy{latency: latency, byMethod: map[string]int64{}, byKind: map[string]int64{}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns, tr.MaxIdleConnsPerHost = 1024, 1024
	tr.DisableCompression = true
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.mu.Lock()
			p.errors++
			p.mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p, nil
}

// ServeHTTP waits the latency, then forwards the request.
func (p *latencyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.count(r)
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()
	if p.latency > 0 {
		t := time.NewTimer(p.latency)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	p.rp.ServeHTTP(w, r)
}

// count counts r as begun and in flight.
func (p *latencyProxy) count(r *http.Request) {
	k := storeKind(r)
	p.mu.Lock()
	p.total++
	p.byMethod[r.Method]++
	p.byKind[k]++
	p.inFlight++
	p.maxInFlight = max(p.maxInFlight, p.inFlight)
	p.mu.Unlock()
}

// storeKind is a store request's method and what it asks for: "list",
// "versions" (a version listing), "listen" (change notifications) or
// "bucket" for a bucket-level call; for an object, the extension of the
// key's last part (".metadata") or "object" when it has none.
func storeKind(r *http.Request) string {
	q := r.URL.Query()
	key := strings.TrimPrefix(r.URL.Path, "/")
	_, obj, hasObj := strings.Cut(key, "/")
	hasObj = hasObj && obj != ""
	switch {
	case q.Has("versions"):
		return r.Method + " versions"
	case q.Has("notification"):
		return r.Method + " listen"
	case !hasObj && (q.Has("list-type") || q.Has("prefix") || q.Has("delimiter")):
		return r.Method + " list"
	case !hasObj:
		return r.Method + " bucket"
	}
	last := obj[strings.LastIndex(obj, "/")+1:]
	if i := strings.LastIndex(last, "."); i >= 0 {
		return r.Method + " " + last[i:]
	}
	return r.Method + " object"
}

// Stats are the counters.
func (p *latencyProxy) Stats() proxyStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := proxyStats{Total: p.total, ByMethod: map[string]int64{}, ByKind: map[string]int64{},
		InFlight: p.inFlight, MaxInFlight: p.maxInFlight, Errors: p.errors}
	for k, v := range p.byMethod {
		s.ByMethod[k] = v
	}
	for k, v := range p.byKind {
		s.ByKind[k] = v
	}
	return s
}

// Reset zeroes the counters (in flight now stays: those requests are
// still open).
func (p *latencyProxy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total, p.errors, p.maxInFlight = 0, 0, p.inFlight
	p.byMethod = map[string]int64{}
	p.byKind = map[string]int64{}
}
