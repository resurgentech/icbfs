package icbfs

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestRecordsReadLegacyJSON: records written as JSON, before they were
// Protobuf, still read.
func TestRecordsReadLegacyJSON(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	te := TrashEntry{EntryID: "e", ItemID: "0000-x", Type: TypeFile, Name: "n", DeletedAt: at, Size: 3, Origin: []byte("o")}
	data, _ := json.Marshal(te)
	if got, err := decodeTrashEntry(data); err != nil || !reflect.DeepEqual(got, te) {
		t.Fatalf("legacy trash entry = %+v, %v", got, err)
	}
	nv := NamedVersion{Name: "n", Time: at, CreatedAt: at}
	data, _ = json.Marshal(nv)
	if got, err := decodeNamedVersion(data); err != nil || !reflect.DeepEqual(got, nv) {
		t.Fatalf("legacy named version = %+v, %v", got, err)
	}
	or := OriginRecord{Drive: "d", ID: "i", ItemID: "0000-x"}
	data, _ = json.Marshal(or)
	if got, err := decodeOriginRecord(data); err != nil || got != or {
		t.Fatalf("legacy origin record = %+v, %v", got, err)
	}
}

// TestNewerLayoutsRefused: every decoder refuses a layout newer than it
// knows, with ErrNewerFormat, rather than reading what it can of it.
func TestNewerLayoutsRefused(t *testing.T) {
	enc := func(m proto.Message) []byte {
		data, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	decoders := map[string]func() error{
		"trash entry": func() error {
			_, err := decodeTrashEntry(enc(&pb.TrashEntry{EntryId: "e", Layout: 2}))
			return err
		},
		"named version": func() error {
			_, err := decodeNamedVersion(enc(&pb.NamedVersion{Name: "n", Layout: 2}))
			return err
		},
		"origin record": func() error {
			_, err := decodeOriginRecord(enc(&pb.OriginRecord{Drive: "d", Layout: 2}))
			return err
		},
		"metadata": func() error {
			_, err := decodeFileMetadata(enc(&pb.Metadata{Mode: 0644, Layout: 2}))
			return err
		},
	}
	for name, decode := range decoders {
		if err := decode(); !errors.Is(err, ErrNewerFormat) {
			t.Errorf("%s at layout 2: err = %v, want ErrNewerFormat", name, err)
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

	// A legacy JSON trash entry, beside a new one, both listed.
	old := TrashEntry{EntryID: "legacy", ItemID: f.id + "-old", Name: "old", ParentID: root, DeletedAt: time.Now().Add(-time.Hour).UTC()}
	data, _ := json.Marshal(old)
	if _, err := store.PutIfAbsent(ctx, f.trashKey(old), bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Unlink(ctx, root, "a"); err != nil {
		t.Fatal(err)
	}
	trash, err := f.ListTrash(ctx, TrashOptions{})
	if err != nil || len(trash) != 2 || trash[0].ItemID != key || trash[1].EntryID != "legacy" {
		t.Fatalf("trash = %+v, %v; want the new entry, then the legacy one", trash, err)
	}

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
