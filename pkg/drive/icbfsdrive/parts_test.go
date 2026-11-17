package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
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

// Uploads in parts (T82-10) over MinIO.

const partSize = int(icbfs.UploadPartSize)

func uploadData(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

// stage stages data as upload id's parts, in order.
func stage(t *testing.T, ctx context.Context, d *Drive, id string, data []byte) int {
	t.Helper()
	n := 0
	for off := 0; off < len(data); off += partSize {
		n++
		p := data[off:min(off+partSize, len(data))]
		if err := d.StagePart(ctx, id, n, bytes.NewReader(p), int64(len(p))); err != nil {
			t.Fatalf("part %d: %v", n, err)
		}
	}
	return n
}

func readAll(t *testing.T, ctx context.Context, d *Drive, p string) []byte {
	t.Helper()
	rc, _, err := d.Read(ctx, drive.ByPath(p), drive.At{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// uploadsLeft is every version under the filesystem's upload prefix, and
// the bucket's unfinished multipart uploads.
func uploadsLeft(t *testing.T, store objstore.Store, d *Drive, srv miniotest.Server) (versions, multipart int) {
	t.Helper()
	ctx := context.Background()
	vs, err := store.HistoryUnder(ctx, d.Filesystem().UploadPrefix())
	if err != nil {
		t.Fatal(err)
	}
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.Endpoint), UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, ""),
	})
	out, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(srv.Bucket)})
	if err != nil {
		t.Fatal(err)
	}
	return len(vs), len(out.Uploads)
}

// TestUploadInParts: a file through parts is the same file as through
// Write (bytes, size, checksums, a version, a change event), the upload
// leaves nothing behind (no version of any staged object, no unfinished
// multipart upload), and two uploads of one path completing in turn
// leave the second, as two Writes do.
func TestUploadInParts(t *testing.T) {
	store, srv := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "parts", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	data := uploadData(2*partSize+12345, 1)

	plain, err := d.Write(ctx, "/a/plain.bin", bytes.NewReader(data), drive.Condition{}, drive.WriteOptions{Parents: true})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := d.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const id = "upload-0000000001"
	if err := d.StartUpload(ctx, id, []byte(`{"who":"test"}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.StartUpload(ctx, id, nil); !errors.Is(err, drive.ErrExists) {
		t.Fatalf("an id in use: %v", err)
	}
	if rec, started, err := d.UploadRecord(ctx, id); err != nil || string(rec) != `{"who":"test"}` || started.IsZero() {
		t.Fatalf("record %q %v %v", rec, started, err)
	}
	n := stage(t, ctx, d, id, data)
	st, err := d.UploadState(ctx, id)
	if err != nil || st.Parts != 3 || st.Size != int64(len(data)) || st.LastPart.IsZero() {
		t.Fatalf("state %+v %v", st, err)
	}
	it, err := d.CompleteUpload(ctx, id, "/a/parts.bin", n, drive.Condition{}, drive.WriteOptions{Parents: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, ctx, d, "/a/parts.bin"); !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, not the %d uploaded", len(got), len(data))
	}
	if it.Size != plain.Size || it.Checksums == nil || plain.Checksums == nil || *it.Checksums != *plain.Checksums || *it.Checksums != sumsOf(data) {
		t.Fatalf("item %+v %+v, plain %+v %+v", it.Size, it.Checksums, plain.Size, plain.Checksums)
	}
	if vs, err := d.Versions(ctx, drive.ByPath("/a/parts.bin")); err != nil || len(vs) != 1 || vs[0].Checksums == nil || *vs[0].Checksums != sumsOf(data) {
		t.Fatalf("versions %+v %v", vs, err)
	}
	select {
	case c := <-changes:
		if c.Path != "/a" {
			t.Errorf("change %+v, want /a", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no change event for the completed upload")
	}
	// The upload stays until its caller drops it: its result recorded
	// (what a complete asked again is told), its parts erased out of the
	// request, then all of it.
	if v, m := uploadsLeft(t, store, d, srv); v != 4 || m != 0 {
		t.Fatalf("after complete: %d versions staged (want the record and 3 parts), %d unfinished multipart uploads", v, m)
	}
	if _, _, err := d.UploadResult(ctx, id); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a result before FinishUpload: %v", err)
	}
	if err := d.FinishUpload(ctx, id, []byte(`{"version":"v1"}`)); err != nil {
		t.Fatal(err)
	}
	if r, at, err := d.UploadResult(ctx, id); err != nil || string(r) != `{"version":"v1"}` || at.IsZero() {
		t.Fatalf("result %q %v %v", r, at, err)
	}
	if err := d.DropUploadParts(ctx, id); err != nil {
		t.Fatal(err)
	}
	if v, m := uploadsLeft(t, store, d, srv); v != 2 || m != 0 {
		t.Fatalf("after the parts' erase: %d versions (want the record and the result), %d multipart uploads", v, m)
	}
	var listed []drive.UploadEntry
	if err := d.Uploads(ctx, func(e drive.UploadEntry) error { listed = append(listed, e); return nil }); err != nil || len(listed) != 1 || !listed[0].Done || !listed[0].Record || listed[0].Parts != 0 {
		t.Fatalf("listed %+v %v", listed, err)
	}
	if err := d.DropUpload(ctx, id); err != nil {
		t.Fatal(err)
	}
	if v, m := uploadsLeft(t, store, d, srv); v != 0 || m != 0 {
		t.Fatalf("after the drop: %d versions, %d multipart uploads", v, m)
	}
	if _, _, err := d.UploadRecord(ctx, id); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("the upload after its drop: %v", err)
	}

	// A second upload of the path, one part, completes after it: the last
	// wins, a second version.
	second := uploadData(1000, 9)
	const id2 = "upload-0000000002"
	if err := d.StartUpload(ctx, id2, nil); err != nil {
		t.Fatal(err)
	}
	stage(t, ctx, d, id2, second)
	if _, err := d.CompleteUpload(ctx, id2, "/a/parts.bin", 1, drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, ctx, d, "/a/parts.bin"); !bytes.Equal(got, second) {
		t.Fatal("the second upload is not the file")
	}
	if vs, _ := d.Versions(ctx, drive.ByPath("/a/parts.bin")); len(vs) != 2 {
		t.Fatalf("%d versions, want 2", len(vs))
	}
}

// TestUploadPartRules: parts in order, the last again replaces it, sizes
// kept, and complete names exactly the parts there; a refusal writes
// nothing.
func TestUploadPartRules(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "rules", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	full := uploadData(partSize, 2)
	const id = "upload-rules-0001"
	part := func(n int, b []byte) error { return d.StagePart(ctx, id, n, bytes.NewReader(b), int64(len(b))) }

	if err := part(1, full); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a part of an upload never started: %v", err)
	}
	if err := d.StartUpload(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		n    int
		b    []byte
		want error
	}{
		{"part 2 first", 2, full, drive.ErrUploadOrder},
		{"part 0", 0, full, drive.ErrInvalid},
		{"empty", 1, nil, drive.ErrInvalid},
		{"too large", 1, uploadData(partSize+1, 2), drive.ErrInvalid},
		{"part 1", 1, full, nil},
		{"part 1 again", 1, full, nil},
		{"part 3 before 2", 3, full, drive.ErrUploadOrder},
		{"part 2", 2, full[:100], nil},
		{"part 1 after 2", 1, full, drive.ErrUploadOrder},
		{"part 3 after a short last", 3, full, drive.ErrUploadOrder},
		{"part 2 again, whole now", 2, full, nil},
	} {
		if err := part(tc.n, tc.b); !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, count := range []int{1, 3, 0} {
		if _, err := d.CompleteUpload(ctx, id, "/f.bin", count, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrUploadParts) {
			t.Errorf("complete with %d of 2 parts: %v", count, err)
		}
	}
	if _, err := d.Stat(ctx, drive.ByPath("/f.bin"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a refused complete wrote: %v", err)
	}
	// A short part in the middle (staged as the last, then one after it
	// refused) can't be completed past: part 2 is whole again here, so
	// add a short part 3 and complete.
	if err := part(3, full[:7]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteUpload(ctx, id, "/f.bin", 3, drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte{}, full...), full...), full[:7]...)
	if got := readAll(t, ctx, d, "/f.bin"); !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes, want %d", len(got), len(want))
	}
}

// TestUploadDropAndInvisible: while an upload holds parts nothing that
// reads the filesystem sees them (a listing to depth, an as-of view, the
// filesystem's own key listing and usage, fsck), and a drop erases every
// version of every staged object.
func TestUploadDropAndInvisible(t *testing.T) {
	store, srv := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: "hidden", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	write(t, ctx, d, "/doc.txt", "hello", drive.Condition{})
	fs := d.Filesystem()
	usage, err := fs.DiskUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const id = "upload-hidden-0001"
	if err := d.StartUpload(ctx, id, []byte("r")); err != nil {
		t.Fatal(err)
	}
	data := uploadData(partSize+10, 3)
	stage(t, ctx, d, id, data[:partSize])
	stage(t, ctx, d, id, data[:partSize]) // part 1 again: a second version of it
	if err := d.StagePart(ctx, id, 2, bytes.NewReader(data[partSize:]), 10); err != nil {
		t.Fatal(err)
	}

	page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{Depth: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Path != "/doc.txt" {
		t.Errorf("listing %+v", page.Items)
	}
	past, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{Depth: 100, At: drive.At{Time: time.Now().Add(time.Second)}})
	if err != nil || len(past.Items) != 1 {
		t.Errorf("as-of listing %+v %v", past.Items, err)
	}
	if now, err := fs.DiskUsage(ctx); err != nil || now != usage {
		t.Errorf("usage %d with the parts, %d without (%v)", now, usage, err)
	}
	objs, err := store.List(ctx, fs.Prefix())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if strings.Contains(o.Key, "upload") {
			t.Errorf("the filesystem's listing has %s", o.Key)
		}
	}
	rep, err := fs.Check(ctx)
	if err != nil || len(rep.Problems) != 0 || rep.Unreachable != 0 {
		t.Errorf("fsck: %+v %v", rep, err)
	}
	if v, _ := uploadsLeft(t, store, d, srv); v != 4 {
		t.Errorf("%d versions staged, want 4 (record, part 1 twice, part 2)", v)
	}
	if err := d.DropUpload(ctx, id); err != nil {
		t.Fatal(err)
	}
	if v, m := uploadsLeft(t, store, d, srv); v != 0 || m != 0 {
		t.Fatalf("after drop: %d versions, %d multipart uploads", v, m)
	}
	if _, err := d.CompleteUpload(ctx, id, "/x", 2, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("complete after drop: %v", err)
	}
	// DropUploads (a fork's end): every upload of the filesystem goes.
	for _, other := range []string{"upload-hidden-0002", "upload-hidden-0003"} {
		if err := d.StartUpload(ctx, other, nil); err != nil {
			t.Fatal(err)
		}
		stage(t, ctx, d, other, data[:10])
	}
	if err := d.DropUploads(ctx); err != nil {
		t.Fatal(err)
	}
	if v, m := uploadsLeft(t, store, d, srv); v != 0 || m != 0 {
		t.Fatalf("after DropUploads: %d versions, %d multipart uploads", v, m)
	}
}

// TestUploadsListed: Uploads lists a page at a time, an upload across a
// page's end once, and finds parts with no record.
func TestUploadsListed(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "listed", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	old := objstore.HistoryPageSize
	objstore.HistoryPageSize = 2
	t.Cleanup(func() { objstore.HistoryPageSize = old })
	small := []byte("x")
	ids := []string{"upload-list-00001", "upload-list-00002", "upload-list-00003"}
	for i, id := range ids {
		if err := d.StartUpload(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= i; n++ {
			b := uploadData(partSize, 4)
			if n == i {
				b = small
			}
			if err := d.StagePart(ctx, id, n, bytes.NewReader(b), int64(len(b))); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Parts with no record: a crash between a drop's erases, say.
	orphan := d.Filesystem().UploadPrefix() + "upload-list-00004/p00001"
	if _, err := store.Put(ctx, orphan, bytes.NewReader(small), nil, ""); err != nil {
		t.Fatal(err)
	}
	var got []drive.UploadEntry
	if err := d.Uploads(ctx, func(e drive.UploadEntry) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []drive.UploadEntry{{ID: ids[0], Record: true}, {ID: ids[1], Record: true, Parts: 1}, {ID: ids[2], Record: true, Parts: 2}, {ID: "upload-list-00004", Parts: 1}}
	if len(got) != len(want) {
		t.Fatalf("listed %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
