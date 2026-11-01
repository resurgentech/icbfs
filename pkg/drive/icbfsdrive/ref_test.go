package icbfsdrive

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// putStub writes, by hand, a stub as key's new current content version,
// naming targetKey's version targetVersion of size bytes, and records it
// in key's .metadata Refs, as a stub's writer keeps them.
func putStub(t *testing.T, ctx context.Context, store objstore.Store, key, targetKey, targetVersion string, size int64) *objstore.Object {
	t.Helper()
	body, err := proto.Marshal(&pb.RefRecord{Layout: 1, Key: targetKey, VersionId: targetVersion, Size: size})
	if err != nil {
		t.Fatal(err)
	}
	stub, err := store.Put(ctx, key, bytes.NewReader(body), map[string]string{
		icbfs.MetaRef: "1", icbfs.MetaRefKey: targetKey, icbfs.MetaRefVersion: targetVersion, icbfs.MetaRefSize: strconv.FormatInt(size, 10),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	mb, mobj, err := store.Get(ctx, key+".metadata")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(mb)
	mb.Close()
	var m pb.Metadata
	if err := proto.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Refs == nil {
		m.Refs = map[string]*pb.Ref{}
	}
	m.Refs[stub.ETag] = &pb.Ref{Key: targetKey, VersionId: targetVersion, Size: size}
	data, err = proto.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, key+".metadata", bytes.NewReader(data), nil, mobj.ETag); err != nil {
		t.Fatal(err)
	}
	return stub
}

// TestReadThroughStub: the drive reads a stub's target's bytes and
// reports its real size: now, by version, as of a time, in a listing, in
// its versions, and once the file is trashed.
func TestReadThroughStub(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "refs", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	data := strings.Repeat("the target's bytes, longer than a stub. ", 30)
	n := int64(len(data))
	target := write(t, ctx, d, "/target.txt", data, drive.Condition{})
	a := write(t, ctx, d, "/a.txt", "small", drive.Condition{})
	stub := putStub(t, context.Background(), store, a.ID, target.ID, target.Version.ID, n)

	if got := read(t, ctx, d, drive.ByPath("/a.txt"), drive.At{}); got != data {
		t.Fatalf("read now: %d bytes %q...", len(got), got[:min(len(got), 20)])
	}
	if got := read(t, ctx, d, drive.ByID(a.ID), drive.At{Version: stub.VersionID}); got != data {
		t.Fatalf("read the stub's version: %d bytes", len(got))
	}
	st, err := d.Stat(ctx, drive.ByPath("/a.txt"), drive.At{})
	if err != nil || st.Size != n || st.Version == nil || st.Version.Size != n || st.Version.ID != stub.VersionID {
		t.Fatalf("stat %+v (version %+v), %v; want size %d", st, st.Version, err, n)
	}
	st, err = d.Stat(ctx, drive.ByID(a.ID), drive.At{Version: stub.VersionID})
	if err != nil || st.Size != n || st.Version == nil || st.Version.Size != n {
		t.Fatalf("stat at the stub's version %+v, %v; want size %d", st, err, n)
	}
	page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if it.Name == "a.txt" && it.Size != n {
			t.Fatalf("listed size %d, want %d", it.Size, n)
		}
	}
	vs, err := d.Versions(ctx, drive.ByPath("/a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	checkVersions := func(vs []drive.Version) {
		t.Helper()
		found := false
		for _, v := range vs {
			if v.ID == stub.VersionID {
				found = true
				if v.Size != n {
					t.Fatalf("the stub's version size %d, want %d", v.Size, n)
				}
			} else if v.Size > int64(len("small")) {
				t.Fatalf("another version's size %d", v.Size)
			}
		}
		if !found {
			t.Fatalf("no stub version in %+v", vs)
		}
	}
	checkVersions(vs)

	// As of a time the stub was current.
	time.Sleep(1100 * time.Millisecond)
	at, err := store.ServerTime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	write(t, ctx, d, "/a.txt", "later", drive.Condition{})
	if got := read(t, ctx, d, drive.ByPath("/a.txt"), drive.At{Time: at}); got != data {
		t.Fatalf("read as of the stub's time: %d bytes %q...", len(got), got[:min(len(got), 20)])
	}
	if st, err := d.Stat(ctx, drive.ByPath("/a.txt"), drive.At{Time: at}); err != nil || st.Size != n {
		t.Fatalf("stat as of the stub's time: size %d, %v", st.Size, err)
	}

	// Trashed: by version, from the trash.
	if err := d.Delete(ctx, drive.ByPath("/a.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	st, err = d.Stat(ctx, drive.ByID(a.ID), drive.At{Version: stub.VersionID})
	if err != nil || st.Size != n || st.Version == nil || st.Version.Size != n {
		t.Fatalf("trashed stat at the stub's version %+v, %v; want size %d", st, err, n)
	}
	if got := read(t, ctx, d, drive.ByID(a.ID), drive.At{Version: stub.VersionID}); got != data {
		t.Fatalf("trashed read of the stub's version: %d bytes", len(got))
	}
	vs, err = d.Versions(ctx, drive.ByID(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	checkVersions(vs)
}
