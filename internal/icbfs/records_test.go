package icbfs

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/pb"
)

// TestRecordsRoundTrip: each record kind decodes to what was encoded.
func TestRecordsRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 17, 12, 0, 0, 123456789, time.UTC)
	te := TrashEntry{EntryID: "e", ItemID: "0000-x", Type: TypeDir, Name: "n", ParentID: "p", Path: "/a/n",
		DeletedAt: at, DeletedBy: "uid:7", Size: 42, Version: "v", Batch: "b", Origin: []byte("o"), Pass: "pass"}
	data, err := encodeTrashEntry(te)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeTrashEntry(data); err != nil || !reflect.DeepEqual(got, te) {
		t.Fatalf("trash entry = %+v, %v; want %+v", got, err, te)
	}
	nv := NamedVersion{Name: "n", Time: at, Scope: "/a", CreatedBy: "me", CreatedAt: at.Add(time.Second)}
	data, _ = encodeNamedVersion(nv)
	if got, err := decodeNamedVersion(data); err != nil || !reflect.DeepEqual(got, nv) {
		t.Fatalf("named version = %+v, %v; want %+v", got, err, nv)
	}
	// A name made for "now" has no time of its own (it takes its object's).
	data, _ = encodeNamedVersion(NamedVersion{Name: "now", CreatedAt: at})
	if got, err := decodeNamedVersion(data); err != nil || !got.Time.IsZero() {
		t.Fatalf("named version for now = %+v, %v", got, err)
	}
	or := OriginRecord{Drive: "d", ID: "i", ItemID: "0000-x", Version: "v", Path: "/p"}
	data, _ = encodeOriginRecord(or)
	if got, err := decodeOriginRecord(data); err != nil || got != or {
		t.Fatalf("origin record = %+v, %v; want %+v", got, err, or)
	}
}

// TestOtherLayoutsRefused: every decoder refuses any layout but its own,
// a newer one with ErrNewerFormat and one with none with
// ErrUnknownFormat, rather than reading what it can of it.
func TestOtherLayoutsRefused(t *testing.T) {
	enc := func(m proto.Message) []byte {
		data, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	decoders := map[string]func(layout uint32) error{
		"trash entry": func(layout uint32) error {
			_, err := decodeTrashEntry(enc(&pb.TrashEntry{EntryId: "e", Layout: layout}))
			return err
		},
		"named version": func(layout uint32) error {
			_, err := decodeNamedVersion(enc(&pb.NamedVersion{Name: "n", Layout: layout}))
			return err
		},
		"origin record": func(layout uint32) error {
			_, err := decodeOriginRecord(enc(&pb.OriginRecord{Drive: "d", Layout: layout}))
			return err
		},
		"metadata": func(layout uint32) error {
			_, err := decodeFileMetadata(enc(&pb.Metadata{Mode: 0644, Layout: layout}))
			return err
		},
	}
	for name, decode := range decoders {
		if err := decode(2); !errors.Is(err, ErrNewerFormat) {
			t.Errorf("%s at layout 2: err = %v, want ErrNewerFormat", name, err)
		}
		if err := decode(0); !errors.Is(err, ErrUnknownFormat) {
			t.Errorf("%s at layout 0: err = %v, want ErrUnknownFormat", name, err)
		}
		if err := decode(1); err != nil {
			t.Errorf("%s at layout 1: %v", name, err)
		}
	}
}

// TestNewerFormatsRefusedInStore: a filesystem whose format version is
// newer than this build's isn't opened, a newer master block isn't read,
// and a newer lock object isn't written over; the store is left as it was.
func TestNewerFormatsRefusedInStore(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	key := writeNew(t, ctx, f, root, "a", "x")

	// A lock object in a newer layout.
	f.EnableLocking(true)
	lockKey := lockObjectKey(key)
	if _, err := store.Put(ctx, lockKey, bytes.NewReader(mustMarshal(t, &pb.Lock{Layout: 2})), nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.TryAcquireLock(ctx, key, "h", time.Minute); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("lock over a newer lock object: %v, want ErrNewerFormat", err)
	}

	// The filesystem's format version raised past this build's.
	if err := updateFilesystemEntry(ctx, store, "test", func(e *pb.FilesystemEntry) { e.FormatVersion = FormatVersion + 1 }); err != nil {
		t.Fatal(err)
	}
	if err := New(store, "test").Attach(ctx); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("attach a newer filesystem: %v, want ErrNewerFormat", err)
	}
	if err := New(store, "test").BootstrapReadOnly(ctx); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("read-only open of a newer filesystem: %v, want ErrNewerFormat", err)
	}
	if err := New(store, "test").Bootstrap(ctx, 1<<30, 0755, 0, 0, false); !errors.Is(err, ErrNewerFormat) {
		t.Fatalf("bootstrap a newer filesystem: %v, want ErrNewerFormat", err)
	}
	// And one that says no format at all.
	if err := updateFilesystemEntry(ctx, store, "test", func(e *pb.FilesystemEntry) { e.FormatVersion = 0 }); err != nil {
		t.Fatal(err)
	}
	if err := New(store, "test").Attach(ctx); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("attach a filesystem of format 0: %v, want ErrUnknownFormat", err)
	}
	if err := updateFilesystemEntry(ctx, store, "test", func(e *pb.FilesystemEntry) { e.FormatVersion = FormatVersion + 1 }); err != nil {
		t.Fatal(err)
	}
	info, err := DescribeFilesystem(ctx, store, "test")
	if err != nil || info.FormatVersion != FormatVersion+1 {
		t.Fatalf("describe = %+v, %v", info, err)
	}

	// The master block itself in a newer layout.
	if _, err := store.Put(ctx, masterBlockKey, bytes.NewReader(mustMarshal(t, &pb.MasterBlock{Layout: 2})), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ListFilesystems(ctx, store); !errors.Is(err, ErrNewerFormat) || !strings.Contains(err.Error(), "master block") {
		t.Fatalf("list with a newer master block: %v, want ErrNewerFormat", err)
	}
}

// TestNewFilesystemFormatVersion: a filesystem is created with this
// build's format version and its objects in this build's layouts.
func TestNewFilesystemFormatVersion(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	mb, _, err := readMaster(ctx, store)
	if err != nil || mb.Layout != masterLayout || len(mb.Filesystems) != 1 || mb.Filesystems[0].FormatVersion != FormatVersion {
		t.Fatalf("master block = %v, %v", mb, err)
	}
	if _, _, err := f.Mkdir(ctx, f.RootKey(), "d", 0755, 0, 0); err != nil {
		t.Fatal(err)
	}
	body, _, err := store.Get(ctx, f.RootKey())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(body)
	body.Close()
	var b pb.Block
	if err := proto.Unmarshal(buf.Bytes(), &b); err != nil || b.Layout != 1 {
		t.Fatalf("root block layout = %d, %v", b.Layout, err)
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
