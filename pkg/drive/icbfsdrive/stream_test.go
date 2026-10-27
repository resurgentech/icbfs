package icbfsdrive

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// bigBody is content bigger than one part (8 MiB on MinIO), so a write
// of it goes up in parts.
func bigBody(t *testing.T) []byte {
	t.Helper()
	data := make([]byte, 20<<20+12345)
	rand.New(rand.NewSource(7)).Read(data)
	return data
}

// noSpool fails the test if the drive's cache directory gains a temporary
// write file while the body is being read.
type noSpool struct {
	t   *testing.T
	r   io.Reader
	dir string
	n   int
}

func (s *noSpool) Read(p []byte) (int, error) {
	s.n++
	if s.n%64 == 0 {
		s.check()
	}
	return s.r.Read(p)
}

func (s *noSpool) check() {
	s.t.Helper()
	if m, _ := filepath.Glob(filepath.Join(s.dir, "drive-write-*")); len(m) > 0 {
		s.t.Fatalf("spooled to %v while the size was known", m)
	}
}

func streamDrive(t *testing.T) (*Drive, string) {
	t.Helper()
	dir := t.TempDir()
	d := newDriveWith(t, miniotest.Store(t), icbfs.Settings{Name: "stream", Size: 1 << 30, RootMode: 0o777}, Config{CacheDir: dir})
	return d, dir
}

// TestStreamedWrite: a body with its size known goes to the store as it
// arrives, nothing spooled, and leaves the same item as a spooled write
// of the same bytes: one version, its checksums, the size.
func TestStreamedWrite(t *testing.T) {
	d, dir := streamDrive(t)
	ctx := system()
	data := bigBody(t)
	want := sumsOf(data)

	body := &noSpool{t: t, r: bytes.NewReader(data), dir: dir}
	it, err := d.Write(ctx, "/streamed.bin", body, drive.Condition{}, drive.WriteOptions{Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	body.check()
	if it.Size != int64(len(data)) || it.Checksums == nil || *it.Checksums != want {
		t.Fatalf("item %+v, want size %d and %+v", it, len(data), want)
	}
	if got := read(t, ctx, d, drive.ByID(it.ID), drive.At{}); got != string(data) {
		t.Fatal("read back differs")
	}
	versions, err := d.Versions(ctx, drive.ByID(it.ID))
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions %+v, %v; want one", versions, err)
	}

	// The spooled path, for comparison: the same bytes with no size.
	sp, err := d.Write(ctx, "/spooled.bin", bytes.NewReader(data), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sp.Size != it.Size || *sp.Checksums != *it.Checksums {
		t.Fatalf("spooled %+v, streamed %+v", sp, it)
	}

	// A second version, conditional on the first, streamed over it.
	data2 := append(bytes.Clone(data[:5<<20]), "tail"...)
	body = &noSpool{t: t, r: bytes.NewReader(data2), dir: dir}
	it2, err := d.Write(ctx, "/streamed.bin", body, drive.Condition{IfMatch: it.Version.ID}, drive.WriteOptions{Size: int64(len(data2))})
	if err != nil {
		t.Fatal(err)
	}
	if it2.Size != int64(len(data2)) || *it2.Checksums != sumsOf(data2) {
		t.Fatalf("second write: %+v", it2)
	}
	if versions, _ = d.Versions(ctx, drive.ByID(it.ID)); len(versions) != 2 {
		t.Fatalf("versions after the second write: %d, want 2", len(versions))
	}
	// A stale condition is refused before a byte is read.
	stale := &countReader{r: bytes.NewReader(data)}
	var pe *drive.PreconditionError
	if _, err := d.Write(ctx, "/streamed.bin", stale, drive.Condition{IfMatch: it.Version.ID}, drive.WriteOptions{Size: int64(len(data))}); !errors.As(err, &pe) {
		t.Fatalf("stale write: %v, want a precondition error", err)
	}
	if stale.n != 0 {
		t.Fatalf("stale write read %d bytes of its body", stale.n)
	}

	// Small with a known size: one put, straight from memory.
	small := "a small known body\n"
	s, err := d.Write(ctx, "/small.txt", strings.NewReader(small), drive.Condition{}, drive.WriteOptions{Size: int64(len(small))})
	if err != nil || s.Size != int64(len(small)) || *s.Checksums != sumsOf([]byte(small)) {
		t.Fatalf("small: %+v, %v", s, err)
	}
	if got := read(t, ctx, d, drive.ByID(s.ID), drive.At{}); got != small {
		t.Fatalf("small read back %q", got)
	}
}

type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestStreamedWriteWrongLength: a body shorter or longer than its size
// fails the write and leaves nothing: no file for a create, the old
// version for an overwrite.
func TestStreamedWriteWrongLength(t *testing.T) {
	d, _ := streamDrive(t)
	ctx := system()
	data := bigBody(t)

	for _, tc := range []struct {
		name string
		body []byte
		size int64
	}{
		{"short, in parts", data[:10<<20], int64(len(data))},
		{"long, in parts", data, int64(len(data)) - 100},
		{"short, one part", data[:100], 200},
		{"long, one part", data[:300], 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.Write(ctx, "/wrong.bin", bytes.NewReader(tc.body), drive.Condition{}, drive.WriteOptions{Size: tc.size}); err == nil {
				t.Fatal("wrote, want an error")
			}
			if _, err := d.Stat(ctx, drive.ByPath("/wrong.bin"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
				t.Fatalf("after a failed create: %v, want not found", err)
			}
		})
	}

	// Over an existing file: the old version stays current.
	it := write(t, ctx, d, "/keep.bin", "the old content\n", drive.Condition{})
	if _, err := d.Write(ctx, "/keep.bin", bytes.NewReader(data[:1000]), drive.Condition{}, drive.WriteOptions{Size: int64(len(data))}); err == nil {
		t.Fatal("wrote a short body, want an error")
	}
	if got := read(t, ctx, d, drive.ByID(it.ID), drive.At{}); got != "the old content\n" {
		t.Fatalf("after a failed overwrite: %q", got)
	}
	if versions, _ := d.Versions(ctx, drive.ByID(it.ID)); len(versions) != 1 {
		t.Fatalf("versions after a failed overwrite: %d, want 1", len(versions))
	}
}

// TestStreamedWriteChecksumMismatch: checksums the caller gives are
// checked once the streamed body has gone by; a mismatch is ErrChecksum
// and the version written goes, as if nothing had been written.
func TestStreamedWriteChecksumMismatch(t *testing.T) {
	d, _ := streamDrive(t)
	ctx := system()
	data := bigBody(t)
	wrong := &drive.Checksums{MD5: strings.Repeat("0", 32)}

	if _, err := d.Write(ctx, "/new.bin", bytes.NewReader(data), drive.Condition{}, drive.WriteOptions{Size: int64(len(data)), Checksums: wrong}); !errors.Is(err, drive.ErrChecksum) {
		t.Fatalf("create: %v, want ErrChecksum", err)
	}
	if _, err := d.Stat(ctx, drive.ByPath("/new.bin"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("after a mismatched create: %v, want not found", err)
	}

	it := write(t, ctx, d, "/old.bin", "the old content\n", drive.Condition{})
	if _, err := d.Write(ctx, "/old.bin", bytes.NewReader(data), drive.Condition{}, drive.WriteOptions{Size: int64(len(data)), Checksums: wrong}); !errors.Is(err, drive.ErrChecksum) {
		t.Fatalf("overwrite: %v, want ErrChecksum", err)
	}
	if got := read(t, ctx, d, drive.ByID(it.ID), drive.At{}); got != "the old content\n" {
		t.Fatalf("after a mismatched overwrite: %q", got)
	}
	if versions, _ := d.Versions(ctx, drive.ByID(it.ID)); len(versions) != 1 {
		t.Fatalf("versions after a mismatched overwrite: %d, want 1", len(versions))
	}

	// The right checksums go through.
	right := sumsOf(data)
	if _, err := d.Write(ctx, "/old.bin", bytes.NewReader(data), drive.Condition{}, drive.WriteOptions{Size: int64(len(data)), Checksums: &right}); err != nil {
		t.Fatal(err)
	}
}

// TestWriteFrom: a write from a file the caller holds copies nothing,
// and leaves the same item as any other write of the bytes.
func TestWriteFrom(t *testing.T) {
	d, dir := streamDrive(t)
	ctx := system()
	data := bigBody(t)
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var fw drive.FileWriter = d
	it, err := fw.WriteFrom(ctx, "/from.bin", f, int64(len(data)), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "drive-write-*")); len(m) > 0 {
		t.Fatalf("spooled to %v", m)
	}
	if it.Size != int64(len(data)) || it.Checksums == nil || *it.Checksums != sumsOf(data) {
		t.Fatalf("item %+v", it)
	}
	if got := read(t, ctx, d, drive.ByID(it.ID), drive.At{}); got != string(data) {
		t.Fatal("read back differs")
	}
	if versions, _ := d.Versions(ctx, drive.ByID(it.ID)); len(versions) != 1 {
		t.Fatalf("versions %d, want 1", len(versions))
	}

	// A checksum the caller gives is checked before anything is written.
	wrong := &drive.Checksums{SHA256: strings.Repeat("0", 64)}
	if _, err := fw.WriteFrom(ctx, "/from2.bin", f, int64(len(data)), drive.Condition{}, drive.WriteOptions{Checksums: wrong}); !errors.Is(err, drive.ErrChecksum) {
		t.Fatalf("%v, want ErrChecksum", err)
	}
	if _, err := d.Stat(ctx, drive.ByPath("/from2.bin"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("after a mismatch: %v, want not found", err)
	}

	// A small file goes in one put.
	small := []byte("small\n")
	sp := filepath.Join(t.TempDir(), "small.txt")
	if err := os.WriteFile(sp, small, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := os.Open(sp)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	s, err := fw.WriteFrom(ctx, "/small.txt", sf, int64(len(small)), drive.Condition{}, drive.WriteOptions{})
	if err != nil || s.Size != int64(len(small)) {
		t.Fatalf("small: %+v, %v", s, err)
	}
	if got := read(t, ctx, d, drive.ByID(s.ID), drive.At{}); got != string(small) {
		t.Fatalf("small read back %q", got)
	}
}

// TestCopyPassesSize: drive.Copy gives the target the source item's size,
// so a copy between icbfs drives streams; the cache directory stays
// empty of spools.
func TestCopyPassesSize(t *testing.T) {
	d, dir := streamDrive(t)
	ctx := system()
	data := bigBody(t)
	src, err := d.Write(ctx, "/a.bin", bytes.NewReader(data), drive.Condition{}, drive.WriteOptions{Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "drive-write-*")); len(m) > 0 {
		t.Fatalf("spooled to %v", m)
	}
	out, err := drive.Copy(ctx, d, "d", drive.ByID(src.ID), drive.At{}, d, "/b.bin", drive.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "drive-write-*")); len(m) > 0 {
		t.Fatalf("the copy spooled to %v", m)
	}
	if out.Size != src.Size || *out.Checksums != *src.Checksums {
		t.Fatalf("copy %+v, source %+v", out, src)
	}
}

// TestStreamedWriteGiB is the measurement behind ROADMAP P1: a 1 GiB
// body with its size known, generated as it is read, never spools and
// holds about one part in memory. Only with ICBFS_TEST_BIG=1: a gigabyte
// through MinIO is not for every run. It prints what it saw.
func TestStreamedWriteGiB(t *testing.T) {
	if os.Getenv("ICBFS_TEST_BIG") == "" {
		t.Skip("ICBFS_TEST_BIG not set")
	}
	d, dir := streamDrive(t)
	ctx := system()
	const size = int64(1 << 30)
	gen := &patternReader{n: size}
	body := &noSpool{t: t, r: gen, dir: dir}

	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				peak = max(peak, ms.HeapInuse)
			}
		}
	}()
	start := time.Now()
	it, err := d.Write(ctx, "/gib.bin", body, drive.Condition{}, drive.WriteOptions{Size: size})
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	body.check()
	if it.Size != size {
		t.Fatalf("size %d", it.Size)
	}
	t.Logf("1 GiB streamed in %s; peak heap in use %d MiB; checksums %+v", time.Since(start).Round(time.Millisecond), peak>>20, *it.Checksums)
}

// patternReader is n bytes of a pattern, made as they are read.
type patternReader struct {
	n, off int64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	n := int(min(int64(len(b)), p.n-p.off))
	for i := 0; i < n; i++ {
		b[i] = byte((p.off + int64(i)) % 251)
	}
	p.off += int64(n)
	return n, nil
}
