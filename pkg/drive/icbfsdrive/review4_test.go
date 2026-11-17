package icbfsdrive

// Review 4 of T82-10, brought in by round 5. Its probes log what each
// create writes (TestReview4CreateObjects); round 5 makes the P1 probes
// require that a create that fails, is cut or loses a race leaves no
// version of anything under the key it minted (r4Leak).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

const r4Large = 9<<20 + 777 // past one 8 MiB part
const r4Spool = 3 << 20     // spooled, unknown length
const r4Small = 1000

// r4Drive is proxiedDrive with the store wrapped (wrap may be nil).
func r4Drive(t *testing.T, srv miniotest.Server, name string, wrap func(objstore.Store) objstore.Store) (*Drive, *latencyProxy) {
	t.Helper()
	p, err := newLatencyProxy(srv.Endpoint, 0)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(p)
	t.Cleanup(hs.Close)
	client := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(hs.URL), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, "")})
	var st objstore.Store = s3store.New(client, srv.Bucket)
	if wrap != nil {
		st = wrap(st)
	}
	return newDriveWith(t, st, icbfs.Settings{Name: name, Size: 1 << 36, RootMode: 0o755, MaxFileSize: 8 << 30}, Config{Root: "/"}), p
}

type r4Ver struct {
	Key, VersionID string
	Size           int64
	Deleted        bool
	LastModified   time.Time
}

func r4All(t *testing.T, st objstore.Store) map[string]r4Ver {
	t.Helper()
	vs, err := st.HistoryUnder(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]r4Ver{}
	for _, v := range vs {
		out[v.Key+"@"+v.VersionID] = r4Ver{v.Key, v.VersionID, v.Size, v.Deleted, v.LastModified}
	}
	return out
}

var r4UUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

type r4Norm struct {
	m map[string]string
}

func (n *r4Norm) s(x string) string {
	return r4UUID.ReplaceAllStringFunc(x, func(u string) string {
		if v, ok := n.m[u]; ok {
			return v
		}
		v := fmt.Sprintf("U%d", len(n.m))
		n.m[u] = v
		return v
	})
}

// r4Delta logs every version the write added (and any removed), each
// with its size, its time against t0, and its user metadata; for small
// non-content objects, their body as printable text.
func r4Delta(t *testing.T, st objstore.Store, before, after map[string]r4Ver, t0 time.Time, n *r4Norm) (added int, addedBytes int64) {
	var keys []string
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := after[keys[i]], after[keys[j]]
		if !a.LastModified.Equal(b.LastModified) {
			return a.LastModified.Before(b.LastModified)
		}
		return a.Key < b.Key
	})
	for _, k := range keys {
		v := after[k]
		added++
		addedBytes += v.Size
		md := ""
		if !v.Deleted {
			if o, err := st.HeadVersion(context.Background(), v.Key, v.VersionID); err == nil && len(o.Metadata) > 0 {
				ks := make([]string, 0, len(o.Metadata))
				for mk := range o.Metadata {
					ks = append(ks, mk)
				}
				sort.Strings(ks)
				for _, mk := range ks {
					md += fmt.Sprintf(" %s=%q", mk, n.s(o.Metadata[mk]))
				}
			}
		}
		t.Logf("   + %-48s size=%-9d del=%-5v t=%+.1fs%s", n.s(v.Key), v.Size, v.Deleted, v.LastModified.Sub(t0).Seconds(), md)
	}
	for k, v := range before {
		if _, ok := after[k]; !ok {
			t.Logf("   - %-48s size=%d (erased)", n.s(v.Key), v.Size)
		}
	}
	return
}

func r4JSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestReview4CreateObjects: what a create of each kind writes, object by
// object, and what a reader then sees; the store requests by method.
func TestReview4CreateObjects(t *testing.T) {
	_, srv := miniotest.Start(t)
	rawStore := s3store.New(s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, "")}), srv.Bucket)
	srcTime := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	large := bytes.Repeat([]byte("L"), r4Large)
	spool := bytes.Repeat([]byte("S"), r4Spool)
	small := bytes.Repeat([]byte("s"), r4Small)
	origin := &drive.Origin{Drive: "clio:firm", ID: "doc-42", Version: "v7", Path: "/Matters/doc.pdf", Copied: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	type tc struct {
		name string
		ctx  func() context.Context
		do   func(ctx context.Context, d *Drive) (drive.Item, error)
	}
	cases := []tc{
		{"small-memory", system, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.Write(ctx, "/a/f.bin", bytes.NewReader(small), drive.Condition{}, drive.WriteOptions{})
		}},
		{"small-known(API PUT)", system, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.Write(ctx, "/a/f.bin", bytes.NewReader(small), drive.Condition{}, drive.WriteOptions{Size: r4Small})
		}},
		{"spooled-3MiB", system, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.Write(ctx, "/a/f.bin", io.MultiReader(bytes.NewReader(spool)), drive.Condition{}, drive.WriteOptions{})
		}},
		{"large-from", system, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.WriteFrom(ctx, "/a/f.bin", bytes.NewReader(large), r4Large, drive.Condition{}, drive.WriteOptions{})
		}},
		{"large-known", system, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.Write(ctx, "/a/f.bin", io.MultiReader(bytes.NewReader(large)), drive.Condition{}, drive.WriteOptions{Size: r4Large})
		}},
		{"stub", system, nil}, // filled in below
		{"large-sourcetime", func() context.Context { return drive.WithSourceTime(system(), srcTime) }, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.WriteFrom(ctx, "/a/f.bin", bytes.NewReader(large), r4Large, drive.Condition{}, drive.WriteOptions{ModTime: srcTime.Add(-time.Hour), Created: srcTime.Add(-2 * time.Hour)})
		}},
		{"large-origin(mirror)", func() context.Context { return drive.WithSourceTime(system(), srcTime) }, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.WriteFrom(ctx, "/a/f.bin", bytes.NewReader(large), r4Large, drive.Condition{}, drive.WriteOptions{ModTime: srcTime.Add(-time.Hour), Origin: origin})
		}},
		{"small-origin(mirror)", func() context.Context { return drive.WithSourceTime(system(), srcTime) }, func(ctx context.Context, d *Drive) (drive.Item, error) {
			return d.Write(ctx, "/a/f.bin", bytes.NewReader(small), drive.Condition{}, drive.WriteOptions{ModTime: srcTime.Add(-time.Hour), Origin: origin})
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, p := r4Drive(t, srv, fmt.Sprintf("r4c%d", i), nil)
			sys := system()
			if _, err := d.Mkdir(sys, "/a", false); err != nil {
				t.Fatal(err)
			}
			do := c.do
			if c.name == "stub" {
				src, err := d.WriteFrom(sys, "/src.bin", bytes.NewReader(large), r4Large, drive.Condition{}, drive.WriteOptions{})
				if err != nil {
					t.Fatal(err)
				}
				do = func(ctx context.Context, d *Drive) (drive.Item, error) {
					return d.Write(ctx, "/a/f.bin", nil, drive.Condition{}, drive.WriteOptions{SameAs: &drive.Ref{ID: src.ID, Version: src.Version.ID}})
				}
			}
			n := &r4Norm{m: map[string]string{}}
			before := r4All(t, rawStore)
			ctx := c.ctx()
			p.Reset()
			t0 := time.Now()
			it, err := do(ctx, d)
			took := time.Since(t0)
			st := p.Stats()
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			n.m[it.ID] = "FILE"
			after := r4All(t, rawStore)
			t.Logf("REQ %s total=%d byMethod=%v byKind=%v took=%s", c.name, st.Total, st.ByMethod, st.ByKind, took.Round(time.Millisecond))
			added, bytesAdded := r4Delta(t, rawStore, before, after, t0, n)
			t.Logf("OBJ %s versions added=%d bytes=%d", c.name, added, bytesAdded)
			// The key's own UUIDv7 time against the write's start.
			if u := r4UUID.FindString(it.ID); u != "" {
				t.Logf("KEY minted %+.3fs after the write began", r4UUIDTime(u).Sub(t0).Seconds())
			}
			stat, err := d.Stat(sys, drive.ByPath("/a/f.bin"), drive.At{})
			if err != nil {
				t.Fatal(err)
			}
			rel := func(x time.Time) string {
				if x.IsZero() {
					return "zero"
				}
				if x.Before(t0.Add(-time.Hour)) {
					return x.UTC().Format(time.RFC3339Nano)
				}
				return fmt.Sprintf("%+.1fs", x.Sub(t0).Seconds())
			}
			t.Logf("STAT size=%d type=%s modified=%s changed=%s created=%s ver.time=%s ver.stored=%s sums=%s origin=%s ct=%q",
				stat.Size, stat.Type, rel(stat.Modified), rel(stat.Changed), rel(stat.Created), rel(stat.Version.Time), rel(stat.Version.Stored),
				r4JSON(stat.Checksums), n.s(r4JSON(stat.Origin)), stat.ContentType)
			vs, err := d.Versions(sys, drive.ByPath("/a/f.bin"))
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range vs {
				t.Logf("VERS size=%d time=%s stored=%s current=%v sums=%v", v.Size, rel(v.Time), rel(v.Stored), v.Current, v.Checksums != nil)
			}
			page, err := d.List(sys, drive.ByPath("/a"), drive.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range page.Items {
				t.Logf("LIST %s size=%d type=%s modified=%s changed=%s created=%s hasVer=%v sums=%v", e.Name, e.Size, e.Type, rel(e.Modified), rel(e.Changed), rel(e.Created), e.Version != nil, e.Checksums != nil)
			}
			attr, err := d.fs.Stat(sys, it.ID, icbfs.TypeFile)
			if err == nil {
				t.Logf("ATTR ctime=%s mtime=%s btime=%s sums=%d app=%d", rel(attr.Ctime), rel(attr.Mtime), r4BT(attr), len(attr.Checksums), len(attr.App))
			}
			if c.ctx != nil && strings.Contains(c.name, "source") || strings.Contains(c.name, "origin") {
				for _, at := range []time.Time{srcTime.Add(-time.Second), srcTime.Add(time.Second), time.Now()} {
					s, err := d.Stat(sys, drive.ByPath("/a/f.bin"), drive.At{Time: at})
					pg, lerr := d.List(sys, drive.ByPath("/a"), drive.ListOptions{At: drive.At{Time: at}})
					nl := -1
					if lerr == nil {
						nl = len(pg.Items)
					}
					t.Logf("ASOF %s stat: size=%d err=%v; list /a: %d entries (%v)", at.UTC().Format(time.RFC3339), s.Size, err, nl, lerr)
				}
			}
			rep, err := d.fs.Check(sys)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("FSCK problems=%d unreachable=%d %v", len(rep.Problems), rep.Unreachable, rep.UnreachableListed)
		})
	}
}

func r4BT(a icbfs.Attr) string {
	b, _ := json.Marshal(a)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"Btime", "BirthTime", "Crtime", "btime"} {
		if v, ok := m[k]; ok {
			return fmt.Sprint(v)
		}
	}
	return "?"
}

// r4UUIDTime is a UUIDv7's time (its first 48 bits, ms).
func r4UUIDTime(u string) time.Time {
	h := strings.ReplaceAll(u, "-", "")[:12]
	var ms int64
	fmt.Sscanf(h, "%x", &ms)
	return time.UnixMilli(ms)
}

// r4Hook is a store whose Put, PutParts and Delete a test can intercept.
type r4Hook struct {
	objstore.Store
	put      func(ctx context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error)
	putParts func(ctx context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error)
	deletes  atomic.Int64
	delErrs  atomic.Int64
}

func (s *r4Hook) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	do := func(c context.Context) (*objstore.Object, error) { return s.Store.Put(c, key, body, md, ifMatch) }
	if s.put != nil {
		return s.put(ctx, key, do)
	}
	return do(ctx)
}

func (s *r4Hook) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	do := func(c context.Context) (*objstore.Object, error) { return s.Store.PutParts(c, key, parts, md, ifMatch) }
	if s.putParts != nil {
		return s.putParts(ctx, key, do)
	}
	return do(ctx)
}

func (s *r4Hook) Delete(ctx context.Context, key string) error {
	s.deletes.Add(1)
	err := s.Store.Delete(ctx, key)
	if err != nil {
		s.delErrs.Add(1)
	}
	return err
}

// r4Leak: bytes in versions of keys of the filesystem's prefix that no
// name reaches (current or behind a marker), from the raw store, against
// what DiskUsage and fs check say.
func r4Leak(t *testing.T, d *Drive, raw objstore.Store, before map[string]r4Ver, label string) {
	t.Helper()
	after := r4All(t, raw)
	var cur, hidden int64
	var curN, hidN int
	byKey := map[string][]r4Ver{}
	for k, v := range after {
		if _, ok := before[k]; ok {
			continue
		}
		byKey[v.Key] = append(byKey[v.Key], v)
	}
	// newest version per key says current or not
	full, _ := raw.HistoryUnder(context.Background(), "")
	newest := map[string]objstore.Object{}
	for _, v := range full {
		if o, ok := newest[v.Key]; !ok || v.LastModified.After(o.LastModified) {
			newest[v.Key] = v
		}
	}
	for k, vs := range byKey {
		for _, v := range vs {
			if v.Size < 1<<20 {
				continue // only content-sized versions
			}
			if newest[k].Deleted {
				hidden += v.Size
				hidN++
			} else {
				cur += v.Size
				curN++
			}
		}
	}
	du, _ := d.fs.DiskUsage(context.Background())
	rep, _ := d.fs.Check(system())
	t.Logf("%s: content-size versions added: %d current (%d bytes), %d behind a delete marker (%d bytes); DiskUsage=%d; fsck problems=%d unreachable=%d %v",
		label, curN, cur, hidN, hidden, du, len(rep.Problems), rep.Unreachable, rep.UnreachableListed)
	// Round 5: no version (nor marker) under a key minted since before but
	// named by nothing: every new key's item is /a/f.bin's, if it is there.
	named := ""
	if n, err := d.resolvePath(system(), d.fs, "/a/f.bin"); err == nil {
		named = r4UUID.FindString(n.key)
	}
	old := map[string]bool{}
	for _, v := range before {
		if u := r4UUID.FindString(v.Key); u != "" {
			old[u] = true
		}
	}
	left := map[string]int{}
	for _, v := range after {
		if _, ok := before[v.Key+"@"+v.VersionID]; ok {
			continue
		}
		u := r4UUID.FindString(v.Key)
		if u == "" || old[u] || u == named {
			continue
		}
		left[v.Key]++
	}
	if len(left) > 0 {
		t.Errorf("%s: left under keys no name reaches (versions and markers): %v", label, left)
	}
	if n := d.fs.UnnamedLeft(); n != 0 {
		t.Errorf("%s: %d failed creates not erased", label, n)
	}
}

// TestReview4CutAfterContent: the content has landed; then the create is
// cut (its context cancelled) or the store refuses the next write, before
// the name. What is left, what a reader sees, what DiskUsage and fs check
// count. On bc622f1 the same hook cuts after the named file's content.
func TestReview4CutAfterContent(t *testing.T) {
	_, srv := miniotest.Start(t)
	raw := s3store.New(s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, "")}), srv.Bucket)
	large := bytes.Repeat([]byte("L"), r4Large)
	for i, mode := range []string{"cancel-after-content", "cancel-at-metadata", "store-error-at-metadata", "cancel-at-link"} {
		t.Run(mode, func(t *testing.T) {
			h := &r4Hook{}
			d, _ := r4Drive(t, srv, fmt.Sprintf("r4k%d", i), func(s objstore.Store) objstore.Store { h.Store = s; return h })
			sys := system()
			if _, err := d.Mkdir(sys, "/a", false); err != nil {
				t.Fatal(err)
			}
			before := r4All(t, raw)
			ctx, cancel := context.WithCancelCause(sys)
			var armed atomic.Bool
			var contentDone atomic.Bool
			var metaDone atomic.Bool
			h.putParts = func(c context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error) {
				o, err := do(c)
				if err == nil && armed.Load() {
					contentDone.Store(true)
					if mode == "cancel-after-content" {
						cancel(errors.New("stopped by admin"))
					}
				}
				return o, err
			}
			h.put = func(c context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error) {
				if armed.Load() && contentDone.Load() && strings.HasSuffix(key, ".metadata") && !metaDone.Load() {
					switch mode {
					case "cancel-at-metadata":
						cancel(errors.New("stopped by admin"))
						return nil, context.Cause(c)
					case "store-error-at-metadata":
						return nil, errors.New("injected: 503 SlowDown")
					}
					o, err := do(c)
					metaDone.Store(true)
					return o, err
				}
				if armed.Load() && metaDone.Load() && mode == "cancel-at-link" && !strings.HasSuffix(key, ".metadata") {
					cancel(errors.New("stopped by admin"))
					return nil, context.Cause(c)
				}
				return do(c)
			}
			armed.Store(true)
			_, err := d.WriteFrom(ctx, "/a/f.bin", bytes.NewReader(large), r4Large, drive.Condition{}, drive.WriteOptions{})
			armed.Store(false)
			_, serr := d.Stat(sys, drive.ByPath("/a/f.bin"), drive.At{})
			size := int64(-1)
			if serr == nil {
				it, _ := d.Stat(sys, drive.ByPath("/a/f.bin"), drive.At{})
				size = it.Size
			}
			t.Logf("RESULT %s: write err=%v; reader: present=%v size=%d; deletes tried=%d failed=%d", mode, err, serr == nil, size, h.deletes.Load(), h.delErrs.Load())
			r4Leak(t, d, raw, before, "LEAK "+mode)
		})
	}
}

// TestReview4TwoWritersOfANewFile: A and B create /a/f.bin at once, each
// with its own content; A links first. B, re-readable, writes over A's;
// B streamed (known length) answers busy. Each writer's content: kept as
// a version of the file, or left where.
func TestReview4TwoWritersOfANewFile(t *testing.T) {
	_, srv := miniotest.Start(t)
	raw := s3store.New(s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, "")}), srv.Bucket)
	for i, bWay := range []string{"from", "known", "from-ifnonematch"} {
		t.Run("B="+bWay, func(t *testing.T) {
			h := &r4Hook{}
			d, _ := r4Drive(t, srv, fmt.Sprintf("r4w%d", i), func(s objstore.Store) objstore.Store { h.Store = s; return h })
			sys := system()
			if _, err := d.Mkdir(sys, "/a", false); err != nil {
				t.Fatal(err)
			}
			before := r4All(t, raw)
			aBody := bytes.Repeat([]byte("A"), r4Large)
			bBody := bytes.Repeat([]byte("B"), r4Large+1)
			var mu sync.Mutex
			contentKeys := []string{}
			bContent := make(chan struct{})
			aDone := make(chan struct{})
			var bOnce sync.Once
			h.putParts = func(c context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error) {
				o, err := do(c)
				mu.Lock()
				contentKeys = append(contentKeys, key)
				nth := len(contentKeys)
				mu.Unlock()
				if nth == 2 {
					bOnce.Do(func() { close(bContent) })
				}
				return o, err
			}
			var metas atomic.Int64
			h.put = func(c context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error) {
				if strings.HasSuffix(key, ".metadata") {
					switch metas.Add(1) {
					case 1: // A's: wait until B's content is in
						select {
						case <-bContent:
						case <-time.After(60 * time.Second):
						}
					case 2: // B's: wait until A has linked and returned
						select {
						case <-aDone:
						case <-time.After(60 * time.Second):
						}
					}
				}
				return do(c)
			}
			var aErr, bErr error
			var aIt, bIt drive.Item
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				aIt, aErr = d.WriteFrom(sys, "/a/f.bin", bytes.NewReader(aBody), int64(len(aBody)), drive.Condition{}, drive.WriteOptions{})
				close(aDone)
			}()
			time.Sleep(300 * time.Millisecond)
			go func() {
				defer wg.Done()
				switch bWay {
				case "from":
					bIt, bErr = d.WriteFrom(sys, "/a/f.bin", bytes.NewReader(bBody), int64(len(bBody)), drive.Condition{}, drive.WriteOptions{})
				case "from-ifnonematch":
					bIt, bErr = d.WriteFrom(sys, "/a/f.bin", bytes.NewReader(bBody), int64(len(bBody)), drive.Condition{IfNoneMatch: true}, drive.WriteOptions{})
				case "known":
					bIt, bErr = d.Write(sys, "/a/f.bin", io.MultiReader(bytes.NewReader(bBody)), drive.Condition{}, drive.WriteOptions{Size: int64(len(bBody))})
				}
			}()
			wg.Wait()
			got := readAll(t, sys, d, "/a/f.bin")
			vs, _ := d.Versions(sys, drive.ByPath("/a/f.bin"))
			sizes := []int64{}
			for _, v := range vs {
				sizes = append(sizes, v.Size)
			}
			t.Logf("RESULT B=%s: A err=%v size=%d; B err=%v size=%d; file now %d bytes (first byte %q); versions %v; content keys written %d; deletes %d",
				bWay, aErr, aIt.Size, bErr, bIt.Size, len(got), got[:1], sizes, len(contentKeys), h.deletes.Load())
			r4Leak(t, d, raw, before, "LEAK B="+bWay)
		})
	}
}

// TestReview4WrongChecksumLeavesNothing (round 5): a known-length body whose
// checksum the caller gave wrong is refused after its content landed at
// the new key; nothing is named and nothing is left under the key.
func TestReview4WrongChecksumLeavesNothing(t *testing.T) {
	_, srv := miniotest.Start(t)
	raw := s3store.New(s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, "")}), srv.Bucket)
	d, _ := r4Drive(t, srv, "r4sum", nil)
	sys := system()
	if _, err := d.Mkdir(sys, "/a", false); err != nil {
		t.Fatal(err)
	}
	before := r4All(t, raw)
	large := bytes.Repeat([]byte("S"), r4Large)
	_, err := d.Write(sys, "/a/f.bin", io.MultiReader(bytes.NewReader(large)), drive.Condition{},
		drive.WriteOptions{Size: r4Large, Checksums: &drive.Checksums{SHA256: strings.Repeat("0", 64)}})
	if !errors.Is(err, drive.ErrChecksum) {
		t.Fatalf("a wrong checksum: %v", err)
	}
	if _, err := d.Stat(sys, drive.ByPath("/a/f.bin"), drive.At{}); err == nil {
		t.Fatal("a file named after a wrong checksum")
	}
	r4Leak(t, d, raw, before, "LEAK wrong-checksum")
}

// TestReview4CutInSourceTimeLeavesNothing (round 5): the same failure in a
// source's time (a mirror's write, dated): the operation's dating does not
// write a .version table for the key the failed create erased.
func TestReview4CutInSourceTimeLeavesNothing(t *testing.T) {
	_, srv := miniotest.Start(t)
	raw := s3store.New(s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, "")}), srv.Bucket)
	h := &r4Hook{}
	d, _ := r4Drive(t, srv, "r4st", func(s objstore.Store) objstore.Store { h.Store = s; return h })
	sys := system()
	if _, err := d.Mkdir(sys, "/a", false); err != nil {
		t.Fatal(err)
	}
	before := r4All(t, raw)
	var armed, contentDone atomic.Bool
	h.putParts = func(c context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error) {
		o, err := do(c)
		if err == nil && armed.Load() {
			contentDone.Store(true)
		}
		return o, err
	}
	h.put = func(c context.Context, key string, do func(context.Context) (*objstore.Object, error)) (*objstore.Object, error) {
		if armed.Load() && contentDone.Load() && strings.HasSuffix(key, ".metadata") {
			return nil, errors.New("injected: 503 SlowDown")
		}
		return do(c)
	}
	armed.Store(true)
	ctx := drive.WithSourceTime(sys, time.Now().Add(-time.Hour).Truncate(time.Second))
	_, err := d.WriteFrom(ctx, "/a/f.bin", bytes.NewReader(bytes.Repeat([]byte("T"), r4Large)), r4Large, drive.Condition{}, drive.WriteOptions{})
	armed.Store(false)
	if err == nil || !contentDone.Load() {
		t.Fatalf("the create: %v (content landed: %v)", err, contentDone.Load())
	}
	r4Leak(t, d, raw, before, "LEAK source-time")
}
