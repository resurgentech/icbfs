package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// The tiers the tests use: up to 1 KiB in memory, to 64 KiB in a file,
// and past that a multipart upload as the bytes arrive. Parts are 8 MiB
// (tierDrive's 8 GiB file limit), so bigBody's 20 MiB go in three.
const (
	tierMemory = 1 << 10
	tierFile   = 64 << 10
)

// smallTiers sets the drive's and the mirror's tiers to the tests' for
// t, and records the way each write's content went: "memory", "file" or
// "stream" for a body of unknown length, "known" for one of known
// length, "from" for content the caller holds (WriteFrom).
func smallTiers(t *testing.T) *[]string {
	t.Helper()
	mem, file, dmem, dfile := inMemoryWrite, inFileWrite, drive.SpoolInMemory, drive.SpoolInFile
	inMemoryWrite, inFileWrite = tierMemory, tierFile
	drive.SpoolInMemory, drive.SpoolInFile = tierMemory, tierFile
	seen := &[]string{}
	writeTier = func(tier string) { *seen = append(*seen, tier) }
	t.Cleanup(func() {
		inMemoryWrite, inFileWrite, drive.SpoolInMemory, drive.SpoolInFile = mem, file, dmem, dfile
		writeTier = nil
	})
	return seen
}

// tierDrive is a drive on its own MinIO with 8 MiB parts; the mirror's
// spool files go to a directory of the test's own (TMPDIR).
func tierDrive(t *testing.T, name string) (*Drive, objstore.Store, *s3.Client, string) {
	t.Helper()
	store, srv := miniotest.Start(t)
	d := newDrive(t, store, icbfs.Settings{Name: name, Size: 1 << 30, MaxFileSize: 8 << 30, RootMode: 0o777}, "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.Endpoint),
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(srv.AccessKey, srv.SecretKey, ""),
	})
	return d, store, client, srv.Bucket
}

// leftovers fails if a multipart upload is still open in the bucket, or a
// spool file is left in the drive's cache directory or the temp one.
func leftovers(t *testing.T, d *Drive, client *s3.Client, bucket string) {
	t.Helper()
	out, err := client.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Uploads) > 0 {
		t.Fatalf("%d multipart uploads left open", len(out.Uploads))
	}
	for _, dir := range []string{d.cfg.CacheDir, os.TempDir()} {
		if m, _ := filepath.Glob(filepath.Join(dir, "drive-write-*")); len(m) > 0 {
			t.Fatalf("spool files left: %v", m)
		}
	}
}

// onlyReader hides a body's other methods (bytes.Reader's WriterTo).
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func tierBodies(t *testing.T) []struct {
	name string
	body []byte
	tier string
} {
	big := bigBody(t)
	return []struct {
		name string
		body []byte
		tier string
	}{
		{"memory", big[:100], "memory"},
		{"memory, at its most", big[:tierMemory], "memory"},
		{"file", big[:tierMemory+1], "file"},
		{"file, at its most", big[:tierFile], "file"},
		{"stream, one part", big[:tierFile+1], "stream"},
		{"stream, in parts", big, "stream"},
	}
}

// TestWriteTiers: a write of unknown length goes, by its size, from
// memory, through a temporary file in the cache directory, or, past
// that, up in parts as it arrives; each leaves the same item, new or
// over a version, and nothing behind.
func TestWriteTiers(t *testing.T) {
	d, store, client, bucket := tierDrive(t, "tiers")
	seen := smallTiers(t)
	ctx := system()
	for _, tc := range tierBodies(t) {
		t.Run(tc.name, func(t *testing.T) {
			p := "/" + strings.ReplaceAll(tc.name, " ", "-")
			second := bytes.Clone(tc.body)
			second[0] ^= 0xff
			for i, body := range [][]byte{tc.body, second} {
				*seen = nil
				it, err := d.Write(ctx, p, onlyReader{bytes.NewReader(body)}, drive.Condition{}, drive.WriteOptions{})
				if err != nil {
					t.Fatalf("write %d: %v", i, err)
				}
				if !slices.Equal(*seen, []string{tc.tier}) {
					t.Fatalf("write %d went by %v, want %s", i, *seen, tc.tier)
				}
				if it.Size != int64(len(body)) || it.Checksums == nil || *it.Checksums != sumsOf(body) {
					t.Fatalf("write %d: item %+v (checksums %+v)", i, it, it.Checksums)
				}
				if got := read(t, ctx, d, drive.ByID(it.ID), drive.At{}); got != string(body) {
					t.Fatalf("write %d reads back %d bytes, not what was written", i, len(got))
				}
				if n := versionCount(t, store, it.ID); n != i+1 {
					t.Fatalf("write %d: %d versions, want %d", i, n, i+1)
				}
				leftovers(t, d, client, bucket)
			}
		})
	}

	// Checksums the caller gives are checked before the upload completes:
	// a mismatch writes nothing.
	big := bigBody(t)
	wrong := &drive.Checksums{SHA256: strings.Repeat("0", 64)}
	if _, err := d.Write(ctx, "/mismatch.bin", onlyReader{bytes.NewReader(big)}, drive.Condition{}, drive.WriteOptions{Checksums: wrong}); !errors.Is(err, drive.ErrChecksum) {
		t.Fatalf("a mismatched create: %v, want ErrChecksum", err)
	}
	if _, err := d.Stat(ctx, drive.ByPath("/mismatch.bin"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("after a mismatched create: %v, want not found", err)
	}
	old := write(t, ctx, d, "/kept.bin", "the old content\n", drive.Condition{})
	if _, err := d.Write(ctx, "/kept.bin", onlyReader{bytes.NewReader(big)}, drive.Condition{}, drive.WriteOptions{Checksums: wrong}); !errors.Is(err, drive.ErrChecksum) {
		t.Fatalf("a mismatched overwrite: %v, want ErrChecksum", err)
	}
	if n := versionCount(t, store, old.ID); n != 1 {
		t.Fatalf("after a mismatched overwrite: %d versions, want 1", n)
	}
	leftovers(t, d, client, bucket)
}

// cutOff is a body that fails after n bytes of r.
type cutOff struct {
	r io.Reader
	n int64
}

var errCutOff = errors.New("the source went away")

func (c *cutOff) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errCutOff
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// TestWriteStreamInterrupted: a body of unknown length that fails past
// the file tier, in its first part or a later one, leaves no version: no
// file for a create (not even in the trash), the old version alone, and
// its .metadata as it was, for an overwrite; and no upload or spool file.
func TestWriteStreamInterrupted(t *testing.T) {
	d, store, client, bucket := tierDrive(t, "interrupted")
	seen := smallTiers(t)
	ctx := system()
	big := bigBody(t)
	old := write(t, ctx, d, "/old.bin", "the old content\n", drive.Condition{})
	meta := versionCount(t, store, old.ID+".metadata")
	for _, cut := range []int64{tierFile + 100, 12 << 20} {
		*seen = nil
		if _, err := d.Write(ctx, "/new.bin", &cutOff{r: bytes.NewReader(big), n: cut}, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, errCutOff) {
			t.Fatalf("a create cut at %d: %v, want the body's error", cut, err)
		}
		if !slices.Equal(*seen, []string{"stream"}) {
			t.Fatalf("cut at %d: went by %v, want stream", cut, *seen)
		}
		if _, err := d.Stat(ctx, drive.ByPath("/new.bin"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
			t.Fatalf("after a create cut at %d: %v, want not found", cut, err)
		}
		if _, err := d.Write(ctx, "/old.bin", &cutOff{r: bytes.NewReader(big), n: cut}, drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, errCutOff) {
			t.Fatalf("an overwrite cut at %d: %v, want the body's error", cut, err)
		}
		if got := read(t, ctx, d, drive.ByID(old.ID), drive.At{}); got != "the old content\n" {
			t.Fatalf("after an overwrite cut at %d: %q", cut, got)
		}
		if n := versionCount(t, store, old.ID); n != 1 {
			t.Fatalf("after an overwrite cut at %d: %d versions, want 1", cut, n)
		}
		if n := versionCount(t, store, old.ID+".metadata"); n != meta {
			t.Fatalf("after an overwrite cut at %d: %d .metadata versions, want %d", cut, n, meta)
		}
		leftovers(t, d, client, bucket)
	}
	page, err := d.Trash(ctx, drive.TrashOptions{})
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("trash %+v, %v; want empty", page.Entries, err)
	}
}

// bigSource is a source item of body, counting its opens in s.
func (s *source) bigItem(id, version, path string, body []byte) drive.SourceItem {
	src := s.item(id, version, path, "", time.Time{})
	src.Open = func(context.Context) (io.ReadCloser, error) {
		s.opens.Add(1)
		return io.NopCloser(onlyReader{bytes.NewReader(body)}), nil
	}
	return src
}

// TestMirrorTiers: the mirror's spool keeps a small body in memory and
// serves it; a larger one in a temporary file, written from it
// (WriteFrom); and past that, passes the bytes through as they arrive,
// for the drive to send up in parts. Each leaves the bytes, and nothing
// behind.
func TestMirrorTiers(t *testing.T) {
	d, store, client, bucket := tierDrive(t, "mirror-tiers")
	seen := smallTiers(t)
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	want := map[string]string{"memory": "memory", "file": "from", "stream": "stream"}
	for i, tc := range tierBodies(t) {
		*seen = nil
		a := apply(t, ctx, m, tc.name, src.bigItem(string(rune('a'+i)), "1", "/"+strings.ReplaceAll(tc.name, " ", "-"), tc.body), drive.Created)
		if !slices.Equal(*seen, []string{want[tc.tier]}) {
			t.Fatalf("%s went by %v, want %s", tc.name, *seen, want[tc.tier])
		}
		notStub(t, store, a.Item.ID, a.Item.Version.ID)
		if a.Item.Checksums == nil || *a.Item.Checksums != sumsOf(tc.body) {
			t.Fatalf("%s: checksums %+v", tc.name, a.Item.Checksums)
		}
		if got := read(t, ctx, d, drive.ByID(a.Item.ID), drive.At{}); got != string(tc.body) {
			t.Fatalf("%s reads back %d bytes, not what was written", tc.name, len(got))
		}
		leftovers(t, d, client, bucket)
	}
}

// TestMirrorStreamTwin: a twin past the file tier, whose hash is known
// only once its bytes have gone by, aborts its upload and is a stub, with
// no full version: new, over another item's version, and as an
// identical version of its own item. Nothing is left behind.
func TestMirrorStreamTwin(t *testing.T) {
	d, store, client, bucket := tierDrive(t, "mirror-stream-twin")
	seen := smallTiers(t)
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	big := bigBody(t)
	a := apply(t, ctx, m, "the original", src.bigItem("a", "1", "/a.bin", big), drive.Created)
	notStub(t, store, a.Item.ID, a.Item.Version.ID)

	*seen = nil
	b := apply(t, ctx, m, "its twin", src.bigItem("b", "1", "/b.bin", big), drive.Created)
	if !slices.Equal(*seen, []string{"stream"}) {
		t.Fatalf("the twin went by %v, want stream (then a stub)", *seen)
	}
	stubOf(t, store, b.Item.ID, b.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := versionCount(t, store, b.Item.ID); n != 1 {
		t.Fatalf("the twin has %d versions, want its stub alone", n)
	}
	if got := read(t, ctx, d, drive.ByID(b.Item.ID), drive.At{}); got != string(big) {
		t.Fatalf("the twin reads back %d bytes", len(got))
	}
	leftovers(t, d, client, bucket)
	page, err := d.Trash(ctx, drive.TrashOptions{})
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("trash %+v, %v; want empty", page.Entries, err)
	}

	// Over an item of other bytes: its next version is a stub, and the
	// item's .metadata gains what a stub's write records there, as a
	// small one's does (one decided before anything is sent): the
	// aborted upload wrote nothing there.
	small := big[:100]
	apply(t, ctx, m, "a small original", src.bigItem("s", "1", "/s.bin", small), drive.Created)
	o := apply(t, ctx, m, "a small other item", src.bigItem("o", "1", "/o.bin", []byte("other bytes")), drive.Created)
	meta := versionCount(t, store, o.Item.ID+".metadata")
	apply(t, ctx, m, "it takes the small one's bytes", src.bigItem("o", "2", "/o.bin", small), drive.Updated)
	stubWrites := versionCount(t, store, o.Item.ID+".metadata") - meta
	c := apply(t, ctx, m, "another item", src.bigItem("c", "1", "/c.bin", []byte("other bytes")), drive.Created)
	meta = versionCount(t, store, c.Item.ID+".metadata")
	*seen = nil
	c2 := apply(t, ctx, m, "it takes the twin's bytes", src.bigItem("c", "2", "/c.bin", big), drive.Updated)
	if !slices.Equal(*seen, []string{"stream"}) {
		t.Fatalf("the new version went by %v, want stream (then a stub)", *seen)
	}
	stubOf(t, store, c2.Item.ID, c2.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := versionCount(t, store, c.Item.ID); n != 2 {
		t.Fatalf("the item has %d versions, want its first and the stub", n)
	}
	if n := versionCount(t, store, c.Item.ID+".metadata"); n != meta+stubWrites {
		t.Fatalf("%d .metadata versions, want %d (%d before, and %d for a stub's write)", n, meta+stubWrites, meta, stubWrites)
	}

	// The original's next version with the same bytes: a stub of its own.
	*seen = nil
	a2 := apply(t, ctx, m, "the original, again", src.bigItem("a", "2", "/a.bin", big), drive.Updated)
	stubOf(t, store, a2.Item.ID, a2.Item.Version.ID, a.Item.ID, a.Item.Version.ID)
	if n := versionCount(t, store, a.Item.ID); n != 2 {
		t.Fatalf("the original has %d versions, want 2", n)
	}
	leftovers(t, d, client, bucket)
}
