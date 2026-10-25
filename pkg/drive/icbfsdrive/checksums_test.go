package icbfsdrive

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func sumsOf(data []byte) drive.Checksums {
	m, s := md5.Sum(data), sha256.Sum256(data)
	return drive.Checksums{MD5: hex.EncodeToString(m[:]), SHA256: hex.EncodeToString(s[:])}
}

// contentReads counts the reads of key's content that reach the store.
type contentReads struct {
	objstore.Store
	key atomic.Value
	n   atomic.Int64
}

func (s *contentReads) count(key string) {
	if k, _ := s.key.Load().(string); k == key {
		s.n.Add(1)
	}
}

func (s *contentReads) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *objstore.Object, error) {
	s.count(key)
	return s.Store.GetVersion(ctx, key, versionID)
}

func (s *contentReads) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.count(key)
	return s.Store.Get(ctx, key)
}

// TestWriteChecksums: a write through the drive has both checksums of
// what it wrote, on the item and on each version; checksums the caller
// gives are checked, and a mismatch writes nothing.
func TestWriteChecksums(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "sums", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	one, two := []byte("first version\n"), []byte("second version, longer\n")
	write(t, ctx, d, "/doc.txt", string(one), drive.Condition{})
	it := write(t, ctx, d, "/doc.txt", string(two), drive.Condition{})
	if it.Checksums == nil || *it.Checksums != sumsOf(two) {
		t.Fatalf("item checksums %+v, want %+v", it.Checksums, sumsOf(two))
	}
	versions, err := d.Versions(ctx, drive.ByPath("/doc.txt"))
	if err != nil || len(versions) != 2 {
		t.Fatalf("versions %+v, %v", versions, err)
	}
	for i, want := range [][]byte{two, one} {
		if c := versions[i].Checksums; c == nil || *c != sumsOf(want) {
			t.Fatalf("version %d checksums %+v, want %+v", i, c, sumsOf(want))
		}
	}
	page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Checksums == nil || *page.Items[0].Checksums != sumsOf(two) {
		t.Fatalf("listed: %+v, %v", page.Items, err)
	}

	bad := sumsOf([]byte("something else"))
	for _, given := range []drive.Checksums{{MD5: bad.MD5}, {SHA256: bad.SHA256}} {
		_, err := d.Write(ctx, "/doc.txt", strings.NewReader("third"), drive.Condition{}, drive.WriteOptions{Checksums: &given})
		if !errors.Is(err, drive.ErrChecksum) {
			t.Fatalf("a wrong checksum: %v; want ErrChecksum", err)
		}
		_, err = d.Write(ctx, "/new.txt", strings.NewReader("third"), drive.Condition{}, drive.WriteOptions{Checksums: &given})
		if !errors.Is(err, drive.ErrChecksum) {
			t.Fatalf("a wrong checksum, new file: %v; want ErrChecksum", err)
		}
	}
	if got := read(t, ctx, d, drive.ByPath("/doc.txt"), drive.At{}); got != string(two) {
		t.Fatalf("after refused writes: %q", got)
	}
	if exists(t, d, "/new.txt", time.Time{}) {
		t.Fatal("a refused write made a file")
	}
	right := sumsOf([]byte("third"))
	right.MD5 = strings.ToUpper(right.MD5)
	if _, err := d.Write(ctx, "/doc.txt", strings.NewReader("third"), drive.Condition{}, drive.WriteOptions{Checksums: &right}); err != nil {
		t.Fatalf("the right checksums: %v", err)
	}
}

// TestChecksumsOnDemand: a mount's part-edit of a big file leaves the new
// version without checksums (it would mean reading the whole file);
// asking computes them, once, and filling in an old version's changes
// nothing a user sees: ctime, mtime, versions, the past.
func TestChecksumsOnDemand(t *testing.T) {
	store := &contentReads{Store: miniotest.Store(t)}
	d := newDrive(t, store, icbfs.Settings{Name: "ondemand", Size: 1 << 30, RootMode: 0o777, MaxFileSize: 1 << 30}, "")
	ctx := system()
	fsys := d.Filesystem()
	big := bytes.Repeat([]byte("0123456789abcdef"), (9<<20)/16) // past one part
	it := write(t, ctx, d, "/big.bin", string(big), drive.Condition{})
	store.key.Store(it.ID)

	// The part-edit, as a mount makes it.
	of, err := fsys.Open(context.Background(), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := of.WriteAt([]byte("EDIT"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := of.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	of.Close()
	edited := append([]byte(nil), big...)
	copy(edited[100:], "EDIT")
	st, err := d.Stat(ctx, drive.ByPath("/big.bin"), drive.At{})
	if err != nil || st.Checksums != nil {
		t.Fatalf("after a part-edit: checksums %+v, %v; want none", st.Checksums, err)
	}
	pause := time.Now()
	time.Sleep(1100 * time.Millisecond)
	// Another version on top, so the edited one is an old one.
	write(t, ctx, d, "/big.bin", "small now", drive.Condition{})
	versions, err := d.Versions(ctx, drive.ByPath("/big.bin"))
	if err != nil || len(versions) != 3 || versions[1].Checksums != nil {
		t.Fatalf("versions %+v, %v; want 3, the middle without checksums", versions, err)
	}
	before, err := d.Stat(ctx, drive.ByPath("/big.bin"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}

	reads := store.n.Load()
	got, err := d.Checksums(ctx, drive.ByPath("/big.bin"), versions[1].ID)
	if err != nil || got != sumsOf(edited) {
		t.Fatalf("on demand: %+v, %v; want %+v", got, err, sumsOf(edited))
	}
	if n := store.n.Load() - reads; n != 1 {
		t.Fatalf("computing them read content %d times; want 1", n)
	}
	reads = store.n.Load()
	if again, err := d.Checksums(ctx, drive.ByPath("/big.bin"), versions[1].ID); err != nil || again != got {
		t.Fatalf("again: %+v, %v", again, err)
	}
	if n := store.n.Load() - reads; n != 0 {
		t.Fatalf("asking again read content %d times; want 0", n)
	}

	after, err := d.Stat(ctx, drive.ByPath("/big.bin"), drive.At{})
	if err != nil || !after.Changed.Equal(before.Changed) || !after.Modified.Equal(before.Modified) {
		t.Fatalf("changed %v -> %v, modified %v -> %v; want both unchanged", before.Changed, after.Changed, before.Modified, after.Modified)
	}
	now, err := d.Versions(ctx, drive.ByPath("/big.bin"))
	if err != nil || len(now) != 3 || now[1].Checksums == nil || *now[1].Checksums != got || now[0].Checksums == nil || *now[0].Checksums != sumsOf([]byte("small now")) {
		t.Fatalf("versions after: %+v, %v", now, err)
	}
	if past := read(t, ctx, d, drive.ByPath("/big.bin"), drive.At{Time: pause}); past != string(edited) {
		t.Fatalf("the past changed: %d bytes", len(past))
	}
}

// TestMountFlushChecksums: a file a mount writes in one piece has its
// checksums from the flush.
func TestMountFlushChecksums(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "flush", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := context.Background()
	fsys := d.Filesystem()
	key, _, etag, err := fsys.Create(ctx, fsys.RootKey(), "m.txt", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	of := fsys.NewOpenFile(key, etag, nil)
	if err := of.WriteAt([]byte("written by a mount"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := of.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	of.Close()
	st, err := d.Stat(system(), drive.ByPath("/m.txt"), drive.At{})
	if err != nil || st.Checksums == nil || *st.Checksums != sumsOf([]byte("written by a mount")) {
		t.Fatalf("checksums %+v, %v", st.Checksums, err)
	}
}

// TestChecksumsKeepSourceTimeOrder: filling in checksums of a mirrored
// file isn't a change to its history, so the mirror's next, dated version
// still goes on in order.
func TestChecksumsKeepSourceTimeOrder(t *testing.T) {
	d, _ := sourceDrive(t, "sumorder")
	ctx := system()
	it, err := d.Write(at(ctx, 2021), "/a.txt", strings.NewReader("a"), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// What filling in a version's checksums writes, now.
	if err := d.Filesystem().SetChecksums(context.Background(), it.ID, "an-older-version", icbfs.Checksums{SHA256: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(at(ctx, 2022), "/a.txt", strings.NewReader("b"), drive.Condition{}, drive.WriteOptions{ModTime: year(2022)}); err != nil {
		t.Fatalf("the next dated version after checksums were recorded: %v", err)
	}
}
