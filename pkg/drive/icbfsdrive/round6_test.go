package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// T82-10 round 6: the rows of interloc's docs/STATES.md section 2 that
// icbfs carries (AS-14, AS-15, AS-17, AS-18, AS-31, UP-16, bound 2.5),
// over MinIO.

func s3Client(srv miniotest.Server) *s3.Client {
	return s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, ""),
	})
}

// openCopies is the bucket's unfinished multipart uploads (the test may
// list; Drives' IAM user may not).
func openCopies(t *testing.T, srv miniotest.Server) []string {
	t.Helper()
	out, err := s3Client(srv).ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{Bucket: aws.String(srv.Bucket)})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range out.Uploads {
		ids = append(ids, aws.ToString(u.UploadId))
	}
	return ids
}

// stagedUpload is upload id started and data staged; its part count.
func stagedUpload(t *testing.T, ctx context.Context, d *Drive, id string, data []byte) int {
	t.Helper()
	if err := d.StartUpload(ctx, id, []byte("r")); err != nil {
		t.Fatal(err)
	}
	return stage(t, ctx, d, id, data)
}

// TestAS14AS15_PlanBeforeTheFirstByte: a write of an upload tells its
// Plan the file's key before anything is written under it (a new file:
// its freshly made key, New), then the store's copy id while that copy
// holds no piece yet; an existing file's key is its own (not New).
func TestAS14AS15_PlanBeforeTheFirstByte(t *testing.T) {
	store, srv := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "plan", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	data := uploadData(2*partSize+10, 3)
	const id = "upload-plan-000001"
	n := stagedUpload(t, ctx, d, id, data)
	var plans []drive.UploadPlan
	plan := func(ctx context.Context, p drive.UploadPlan) error {
		plans = append(plans, p)
		vs, err := store.HistoryUnder(ctx, p.Key)
		if err != nil {
			return err
		}
		if len(vs) != 0 {
			return fmt.Errorf("plan %+v: %d versions under the key already", p, len(vs))
		}
		if p.Copy != "" {
			if open := openCopies(t, srv); len(open) != 1 || open[0] != p.Copy {
				return fmt.Errorf("plan %+v: open copies %v", p, open)
			}
			parts, err := s3Client(srv).ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(srv.Bucket), Key: aws.String(p.Key), UploadId: aws.String(p.Copy)})
			if err != nil {
				return err
			}
			if len(parts.Parts) != 0 {
				return fmt.Errorf("plan %+v: %d pieces copied already", p, len(parts.Parts))
			}
		}
		return nil
	}
	it, err := d.CompleteUploadWith(ctx, id, "/a/new.bin", n, drive.Condition{}, drive.WriteOptions{Parents: true}, drive.UploadWrite{Completed: time.Now(), Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || !plans[0].New || plans[0].Key == "" || plans[0].Copy != "" || plans[1].Key != plans[0].Key || plans[1].Copy == "" {
		t.Fatalf("a new file's plans: %+v", plans)
	}
	if it.ID != plans[0].Key {
		t.Fatalf("the file's key %s, the plan's %s", it.ID, plans[0].Key)
	}
	if open := openCopies(t, srv); len(open) != 0 {
		t.Fatalf("copies left open: %v", open)
	}
	// An existing file: its own key, not New.
	const id2 = "upload-plan-000002"
	n2 := stagedUpload(t, ctx, d, id2, data)
	plans = nil
	if _, err := d.CompleteUploadWith(ctx, id2, "/a/new.bin", n2, drive.Condition{}, drive.WriteOptions{}, drive.UploadWrite{Plan: func(ctx context.Context, p drive.UploadPlan) error {
		plans = append(plans, p)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].New || plans[0].Key != it.ID || plans[1].Copy == "" {
		t.Fatalf("an existing file's plans: %+v", plans)
	}
	// A Plan that fails: nothing written, no copy left.
	const id3 = "upload-plan-000003"
	n3 := stagedUpload(t, ctx, d, id3, data)
	refuse := errors.New("the note could not be written")
	if _, err := d.CompleteUploadWith(ctx, id3, "/a/third.bin", n3, drive.Condition{}, drive.WriteOptions{}, drive.UploadWrite{Plan: func(ctx context.Context, p drive.UploadPlan) error {
		if p.Copy != "" {
			return refuse
		}
		return nil
	}}); !errors.Is(err, refuse) {
		t.Fatalf("a Plan's refusal: %v", err)
	}
	if _, err := d.Stat(ctx, drive.ByPath("/a/third.bin"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("written after its Plan refused: %v", err)
	}
	if open := openCopies(t, srv); len(open) != 0 {
		t.Fatalf("copies left open: %v", open)
	}
}

// TestAS14_AbortCopyEndsAKilledCopy: a copy a killed process left (made
// and given a piece, never completed) is ended by its id: no listing.
// Asked again (it is gone) it is not an error.
//
// Azure has no such id: its uncommitted blocks go at the blob's next
// commit, or after 7 days, so AbortCopy there does nothing (CopyAborter
// is S3Store's only).
func TestAS14_AbortCopyEndsAKilledCopy(t *testing.T) {
	store, srv := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "abort", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	const id = "upload-abort-00001"
	stagedUpload(t, ctx, d, id, uploadData(partSize, 4))
	key := d.Filesystem().Prefix() + "0190a7b0-0000-7000-8000-000000000001"
	c := s3Client(srv)
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(srv.Bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	part := d.Filesystem().UploadPrefix() + id + "/p00001"
	if _, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String(srv.Bucket), Key: aws.String(key), UploadId: up.UploadId,
		PartNumber: aws.Int32(1), CopySource: aws.String(srv.Bucket + "/" + part)}); err != nil {
		t.Fatal(err)
	}
	if open := openCopies(t, srv); len(open) != 1 {
		t.Fatalf("the killed copy: %v", open)
	}
	if err := d.AbortCopy(ctx, key, aws.ToString(up.UploadId)); err != nil {
		t.Fatal(err)
	}
	if open := openCopies(t, srv); len(open) != 0 {
		t.Fatalf("still open after AbortCopy: %v", open)
	}
	if err := d.AbortCopy(ctx, key, aws.ToString(up.UploadId)); err != nil {
		t.Fatalf("an id already ended: %v", err)
	}
}

// writeUnlinked writes upload id's content under a new key and stops
// there, as a process killed between a new file's content and its link
// leaves it; the key.
func writeUnlinked(t *testing.T, ctx context.Context, d *Drive, id string, n int) string {
	t.Helper()
	src, err := d.Filesystem().UploadSources(ctx, id, n)
	if err != nil {
		t.Fatal(err)
	}
	src.Completed = time.Now()
	key := d.Filesystem().Prefix() + "0190a7b0-0000-7000-8000-0000000000aa"
	if _, err := d.Filesystem().WriteUpload(ctx, key, src, ""); err != nil {
		t.Fatal(err)
	}
	return key
}

// TestAS15_ContentNothingNamesIsErased: a new file's content written and
// never linked is found under its key and not named anywhere; it is
// erased whole (EraseUnnamed), and nothing is left under the key. A key a
// name reaches is refused, and kept.
func TestAS15_ContentNothingNamesIsErased(t *testing.T) {
	store, _ := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "unnamed", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	const id = "upload-unnamed-001"
	n := stagedUpload(t, ctx, d, id, uploadData(partSize+3, 5))
	key := writeUnlinked(t, ctx, d, id, n)
	f, err := d.FindUpload(ctx, id, key, "/f.bin", time.Now().Add(-time.Minute), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Version == nil || f.AtPath || f.Named || f.Trashed {
		t.Fatalf("an unlinked new file's content: %+v", f)
	}
	if err := d.EraseUnnamed(ctx, key); err != nil {
		t.Fatal(err)
	}
	if vs, err := store.HistoryUnder(ctx, key); err != nil || len(vs) != 0 {
		t.Fatalf("left under the key: %d %v", len(vs), err)
	}
	named := write(t, ctx, d, "/named.txt", "x", drive.Condition{})
	if err := d.EraseUnnamed(ctx, named.ID); !errors.Is(err, drive.ErrExists) {
		t.Fatalf("a named key: %v", err)
	}
	if got := read(t, ctx, d, drive.ByPath("/named.txt"), drive.At{}); got != "x" {
		t.Fatalf("the named file: %q", got)
	}
}

// TestAS17AS18_FoundByKeyWhereverNamed: an upload's version is found
// under the key its Plan named: at the path (AS-17); renamed and moved
// (named elsewhere); deleted (in the trash). Nothing is written.
func TestAS17AS18_FoundByKeyWhereverNamed(t *testing.T) {
	store, _ := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "found", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	const id = "upload-found-00001"
	n := stagedUpload(t, ctx, d, id, uploadData(partSize+7, 6))
	since := time.Now().Add(-time.Minute)
	var key string
	it, err := d.CompleteUploadWith(ctx, id, "/a/f.bin", n, drive.Condition{}, drive.WriteOptions{Parents: true}, drive.UploadWrite{Plan: func(_ context.Context, p drive.UploadPlan) error {
		key = p.Key
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	check := func(what, p string, at, named, trashed bool) {
		t.Helper()
		f, err := d.FindUpload(ctx, id, key, p, since, time.Time{})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if f.Version == nil || f.Version.ID != it.Version.ID || f.AtPath != at || f.Named != named || f.Trashed != trashed {
			t.Fatalf("%s: %+v (version %+v), want at %v named %v trashed %v", what, f, f.Version, at, named, trashed)
		}
	}
	check("at its path", "/a/f.bin", true, true, false)
	if _, err := d.Mkdir(ctx, "/b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(ctx, drive.ByPath("/a/f.bin"), "/b/g.bin", false); err != nil {
		t.Fatal(err)
	}
	check("renamed and moved", "/a/f.bin", false, true, false)
	if err := d.Delete(ctx, drive.ByPath("/b/g.bin"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	check("deleted", "/a/f.bin", false, false, true)
	if err := d.EraseUnnamed(ctx, key); !errors.Is(err, drive.ErrExists) {
		t.Fatalf("a deleted file's key is not erased as unnamed: %v", err)
	}
}

// TestAS31_ANewerUploadIsSeen: an upload whose complete was accepted
// before another's that has written the path sees it (Newer); the newer
// one sees nothing newer; a plain write is no upload's.
func TestAS31_ANewerUploadIsSeen(t *testing.T) {
	store, _ := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "newer", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	data := uploadData(partSize+1, 8)
	const older, newer = "upload-older-00001", "upload-newer-00001"
	stagedUpload(t, ctx, d, older, data)
	n := stagedUpload(t, ctx, d, newer, data)
	t1 := time.Now()
	t2 := t1.Add(time.Second)
	write(t, ctx, d, "/f.bin", "a plain write", drive.Condition{})
	if f, err := d.FindUpload(ctx, older, "", "/f.bin", t1, t1); err != nil || f.Newer || f.Version != nil {
		t.Fatalf("after a plain write: %+v %v", f, err)
	}
	if _, err := d.CompleteUploadWith(ctx, newer, "/f.bin", n, drive.Condition{}, drive.WriteOptions{}, drive.UploadWrite{Completed: t2}); err != nil {
		t.Fatal(err)
	}
	if f, err := d.FindUpload(ctx, older, "", "/f.bin", t1, t1); err != nil || !f.Newer || f.Version != nil {
		t.Fatalf("the older upload: %+v %v", f, err)
	}
	if f, err := d.FindUpload(ctx, newer, "", "/f.bin", t2, t2); err != nil || f.Newer || f.Version == nil {
		t.Fatalf("the newer upload: %+v %v", f, err)
	}
}

// reverseErase is a store whose EraseAll erases one key under the prefix,
// the last in key order, and then fails: an erase cut partway, in an
// order EraseAll is free to take.
type reverseErase struct {
	objstore.Store
	mu   sync.Mutex
	fail bool
}

func (s *reverseErase) EraseAll(ctx context.Context, prefix string) error {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if !fail {
		return s.Store.EraseAll(ctx, prefix)
	}
	vs, err := s.Store.HistoryUnder(ctx, prefix)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, v := range vs {
		keys[v.Key] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(sorted)))
	if len(sorted) > 0 {
		if err := s.Store.EraseAll(ctx, sorted[0]); err != nil {
			return err
		}
	}
	return errors.New("the store failed partway")
}

// TestUP16_AnAbortCutPartwayKeepsTheRecord: an abort whose erase fails
// partway leaves the record, so the upload is still there to be aborted
// again; a part sent meanwhile is refused (out of order); the abort sent
// again finishes.
func TestUP16_AnAbortCutPartwayKeepsTheRecord(t *testing.T) {
	base, _ := miniotest.Start(t)
	store := &reverseErase{Store: base}
	d := newDrive(t, store, icbfs.Settings{Name: "abortcut", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	const id = "upload-abortcut-01"
	data := uploadData(3*partSize, 9)
	stagedUpload(t, ctx, d, id, data[:2*partSize])
	if err := d.SetUploadNote(ctx, id, "completing", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	if err := d.DropUpload(ctx, id); err == nil {
		t.Fatal("the cut erase answered nil")
	}
	if _, _, err := d.UploadRecord(ctx, id); err != nil {
		t.Fatalf("the record after an abort cut partway: %v", err)
	}
	if err := d.StagePart(ctx, id, 3, bytes.NewReader(data[2*partSize:]), int64(partSize)); !errors.Is(err, drive.ErrUploadOrder) {
		t.Fatalf("a part after the cut abort: %v", err)
	}
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	if err := d.DropUpload(ctx, id); err != nil {
		t.Fatal(err)
	}
	if vs, err := base.HistoryUnder(ctx, d.Filesystem().UploadPrefix()+id); err != nil || len(vs) != 0 {
		t.Fatalf("left after the abort sent again: %d %v", len(vs), err)
	}
}

// TestUploadMarksBound: the store's filesystems with uploads are found in
// one listing (bound 2.5: the sweep lists only those): a start marks its
// filesystem, UnmarkUploads and DropUploads clear it, Prune clears it.
func TestUploadMarksBound(t *testing.T) {
	store, _ := miniotest.Start(t)
	a := newDrive(t, store, icbfs.Settings{Name: "marks-a", Size: 1 << 30, RootMode: 0o755}, "/")
	b := newDrive(t, store, icbfs.Settings{Name: "marks-b", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	want := func(what string, names ...string) {
		t.Helper()
		got, err := a.UploadingFilesystems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var have []string
		for n := range got {
			have = append(have, n)
		}
		sort.Strings(have)
		if strings.Join(have, ",") != strings.Join(names, ",") {
			t.Fatalf("%s: %v, want %v", what, have, names)
		}
	}
	want("none")
	if err := b.StartUpload(ctx, "upload-marks-00001", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.StartUpload(ctx, "upload-marks-00002", nil); err != nil {
		t.Fatal(err)
	}
	if vs, _ := store.HistoryUnder(ctx, "_uploads/"); len(vs) != 1 {
		t.Fatalf("the mark has %d versions after two starts, want 1", len(vs))
	}
	want("b started", "marks-b")
	if err := b.UnmarkUploads(ctx); err != nil {
		t.Fatal(err)
	}
	want("b unmarked")
	if err := a.StartUpload(ctx, "upload-marks-00003", nil); err != nil {
		t.Fatal(err)
	}
	want("a started", "marks-a")
	if err := a.DropUploads(ctx); err != nil {
		t.Fatal(err)
	}
	want("a's uploads dropped")
	if err := b.StartUpload(ctx, "upload-marks-00004", nil); err != nil {
		t.Fatal(err)
	}
	if err := icbfs.Archive(ctx, store, "marks-b"); err != nil {
		t.Fatal(err)
	}
	if err := icbfs.Prune(ctx, store, "marks-b"); err != nil {
		t.Fatal(err)
	}
	if vs, _ := store.HistoryUnder(ctx, "_uploads/"); len(vs) != 0 {
		t.Fatalf("marks left after the prune: %d", len(vs))
	}
}

// TestUploadPartMemory (bound 2.5, "a part request: 4 at once, 20 MiB,
// measured with the store client's own buffers"): four parts of 5 MiB
// staged at once through the S3 store; the bodies (20 MiB, allocated
// before) and what the store client holds besides them, measured as the
// Go heap's and the process's resident set's peak growth over the four,
// three rounds. On Azure: TestAzureUploadPartMemory (a real account).
func TestUploadPartMemory(t *testing.T) {
	store, _ := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "partmem", Size: 1 << 30, RootMode: 0o755}, "/")
	extra := partMemory(t, d, "s3")
	// What the client holds beyond the bodies: a few buffers of 32 KiB to
	// 1 MiB a request. A copy of a body (5 MiB) would show.
	if extra.heap > 4<<20 {
		t.Fatalf("the S3 client held %d MiB of heap beyond the four bodies", extra.heap>>20)
	}
}

type memExtra struct{ heap, rss int64 }

// partMemory stages four 5 MiB parts at once, three times, and returns
// the largest growth of the heap and the resident set over the bodies.
func partMemory(t *testing.T, d *Drive, what string) memExtra {
	t.Helper()
	ctx := system()
	bodies := make([][]byte, 4)
	for i := range bodies {
		bodies[i] = uploadData(partSize, byte(i+1))
	}
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = fmt.Sprintf("upload-partmem-%04d", i)
		if err := d.StartUpload(ctx, ids[i], nil); err != nil {
			t.Fatal(err)
		}
	}
	var worst memExtra
	for round := 1; round <= 3; round++ {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		heap0, rss0 := int64(ms.HeapInuse), vmRSS()
		peakHeap, peakRSS := heap0, rss0
		stop := make(chan struct{})
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			for {
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				peakHeap = max(peakHeap, int64(ms.HeapInuse))
				peakRSS = max(peakRSS, vmRSS())
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond):
				}
			}
		}()
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range bodies {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = d.StagePart(ctx, ids[i], round, bytes.NewReader(bodies[i]), int64(partSize))
			}(i)
		}
		wg.Wait()
		close(stop)
		<-sampled
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		e := memExtra{heap: peakHeap - heap0, rss: peakRSS - rss0}
		t.Logf("%s round %d: four 5 MiB parts at once: heap +%.1f MiB, resident set +%.1f MiB beyond the 20 MiB of bodies", what, round, float64(e.heap)/(1<<20), float64(e.rss)/(1<<20))
		worst.heap, worst.rss = max(worst.heap, e.heap), max(worst.rss, e.rss)
	}
	return worst
}

// TestUP22_ThePruneOfAFilesystemErasesItsUploads: an Upstream removed: its
// filesystems pruned, and every object of their uploads with them.
func TestUP22_ThePruneOfAFilesystemErasesItsUploads(t *testing.T) {
	store, _ := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "pruned", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	stagedUpload(t, ctx, d, "upload-pruned-0001", uploadData(partSize+1, 12))
	prefix := d.Filesystem().UploadPrefix()
	if vs, _ := store.HistoryUnder(ctx, prefix); len(vs) == 0 {
		t.Fatal("nothing staged")
	}
	if err := icbfs.Archive(ctx, store, "pruned"); err != nil {
		t.Fatal(err)
	}
	if err := icbfs.Prune(ctx, store, "pruned"); err != nil {
		t.Fatal(err)
	}
	if vs, err := store.HistoryUnder(ctx, prefix); err != nil || len(vs) != 0 {
		t.Fatalf("left after the prune: %d %v", len(vs), err)
	}
}
