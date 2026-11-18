package icbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

var uidShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestDB5_FilesystemUIDIsNeverShared (interloc docs/STATES.md DB-5, the
// mirror's own id): every filesystem made gets a random id of its own,
// written once at its registration: two filesystems differ; one pruned
// and made again under the same name in the same slot differs; one whose
// whole bucket (prefix) was erased and made again differs, though its
// slot id is the same 0000; Bootstrap (tests' and tools' create) writes
// one too; the id does not change when the master block is rewritten
// for something else (an archive), and Info, ListFilesystems and
// FilesystemUID all give it.
func TestDB5_FilesystemUIDIsNeverShared(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seen := map[string]string{}
	note := func(what, uid string) {
		t.Helper()
		if !uidShape.MatchString(uid) {
			t.Fatalf("%s: uid %q is not 32 hex digits", what, uid)
		}
		if was, ok := seen[uid]; ok {
			t.Fatalf("%s: uid %s is %s's too", what, uid, was)
		}
		seen[uid] = what
	}

	a, err := CreateFilesystem(ctx, store, DefaultSettings("a"))
	if err != nil {
		t.Fatal(err)
	}
	note("a", a.UID)
	b, err := CreateFilesystem(ctx, store, DefaultSettings("b"))
	if err != nil {
		t.Fatal(err)
	}
	note("b", b.UID)
	boot := New(store, "boot")
	if err := boot.Bootstrap(ctx, 1<<30, 0o755, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	bi, err := DescribeFilesystem(ctx, store, "boot")
	if err != nil {
		t.Fatal(err)
	}
	note("boot", bi.UID)

	// Read back the same, and kept across another rewrite of the block.
	if err := Archive(ctx, store, "a"); err != nil {
		t.Fatal(err)
	}
	all, err := ListFilesystems(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range all {
		if in.Name == "a" && (in.UID != a.UID || !in.Archived) {
			t.Fatalf("a after its archive: uid %s archived %v, want %s true", in.UID, in.Archived, a.UID)
		}
	}
	if u, err := FilesystemUID(ctx, store, "a"); err != nil || u != a.UID {
		t.Fatalf("FilesystemUID(a) = %q, %v; want %s", u, err, a.UID)
	}

	// Pruned and made again under the same name, in the same slot.
	if err := Prune(ctx, store, "a"); err != nil {
		t.Fatal(err)
	}
	a2, err := CreateFilesystem(ctx, store, DefaultSettings("a"))
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID != a.ID {
		t.Fatalf("a made again in slot %s, want the freed slot %s (the case the uid is for)", a2.ID, a.ID)
	}
	note("a made again", a2.UID)

	// The whole bucket erased and the filesystem made again: slot 0000
	// and the same declared creation date (a mirror's), another uid.
	if err := store.EraseAll(ctx, ""); err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings("a")
	s.Created = a.Created
	a3, err := CreateFilesystem(ctx, store, s)
	if err != nil {
		t.Fatal(err)
	}
	if a3.ID != "0000" {
		t.Fatalf("after erasing the bucket a was made in slot %s, want 0000", a3.ID)
	}
	note("a after the bucket was erased", a3.UID)
}

// writeEntryWithoutUID puts a master block whose entry for name has no
// uid: a filesystem registered before icbfs wrote one.
func writeEntryWithoutUID(t *testing.T, ctx context.Context, store objstore.Store, name string) {
	t.Helper()
	if _, err := CreateFilesystem(ctx, store, DefaultSettings(name)); err != nil {
		t.Fatal(err)
	}
	mb, obj, err := readMaster(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range mb.Filesystems {
		if e.Name == name {
			e.Uid = ""
		}
	}
	data, err := encodeMaster(mb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, masterBlockKey, bytes.NewReader(data), nil, obj.ETag); err != nil {
		t.Fatal(err)
	}
}

// raceStore is a store whose first conditional write of the master block
// lets another caller give the filesystem its id first.
type raceStore struct {
	objstore.Store
	before func()
	puts   atomic.Int32
}

func (s *raceStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if key == masterBlockKey {
		if s.puts.Add(1) == 1 && s.before != nil {
			s.before()
		}
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

// readOnlyStore refuses every write of the master block.
type readOnlyStore struct{ objstore.Store }

func (s readOnlyStore) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	return nil, objstore.ErrReadOnly
}

// TestDB18_AnOlderFilesystemIsGivenItsUIDOnce (interloc docs/STATES.md
// DB-18): a filesystem registered before icbfs wrote an id is given one
// by FilesystemUID, once: the write is conditional on the master block
// it read, so a second caller that gave one first wins and both get the
// same; a later call reads it and writes nothing; a store that refuses
// the write gives its error and no id; and code that does not know the
// field (an older icbfs) keeps it when it decodes, changes and encodes
// the block again.
func TestDB18_AnOlderFilesystemIsGivenItsUIDOnce(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	writeEntryWithoutUID(t, ctx, base, "old")
	if info, err := DescribeFilesystem(ctx, base, "old"); err != nil || info.UID != "" {
		t.Fatalf("set up: old has uid %q (%v), want none", info.UID, err)
	}

	// Read-only: the error, no id, nothing written.
	if u, err := FilesystemUID(ctx, readOnlyStore{base}, "old"); !errors.Is(err, objstore.ErrReadOnly) || u != "" {
		t.Fatalf("FilesystemUID over a read-only store = %q, %v; want ErrReadOnly and no id", u, err)
	}
	if info, _ := DescribeFilesystem(ctx, base, "old"); info.UID != "" {
		t.Fatalf("a refused write left uid %q", info.UID)
	}

	// Two callers: the other writes first; ours is refused by its
	// condition, reads the block again and takes the other's.
	var other string
	rs := &raceStore{Store: base}
	rs.before = func() {
		var err error
		if other, err = FilesystemUID(ctx, base, "old"); err != nil {
			t.Errorf("the other caller: %v", err)
		}
	}
	mine, err := FilesystemUID(ctx, rs, "old")
	if err != nil {
		t.Fatal(err)
	}
	if !uidShape.MatchString(other) || mine != other {
		t.Fatalf("two callers got %q and %q; want one id, the first's", mine, other)
	}
	if n := rs.puts.Load(); n != 1 {
		t.Fatalf("our caller wrote the master block %d times; want 1 (refused by its condition), then a read", n)
	}

	// A later call only reads.
	rs2 := &raceStore{Store: base}
	if again, err := FilesystemUID(ctx, rs2, "old"); err != nil || again != mine {
		t.Fatalf("FilesystemUID again = %q, %v; want %s", again, err, mine)
	}
	if n := rs2.puts.Load(); n != 0 {
		t.Fatalf("a filesystem with an id: %d writes of the master block, want 0", n)
	}
	if _, err := FilesystemUID(ctx, base, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FilesystemUID of no filesystem: %v, want ErrNotFound", err)
	}

	// Older code keeps the field: decode the block with FilesystemEntry as
	// it was before uid (field 10), archive the entry as updateFilesystemEntry
	// does, encode it, and the id is still there.
	_, obj, err := readMaster(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := base.Get(ctx, masterBlockKey)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	oldBlock := oldMasterBlockType(t)
	m := oldBlock.New()
	if err := proto.Unmarshal(data, m.Interface()); err != nil {
		t.Fatal(err)
	}
	list := m.Get(m.Descriptor().Fields().ByName("filesystems")).List()
	for i := 0; i < list.Len(); i++ {
		e := list.Get(i).Message()
		ed := e.Descriptor()
		if ed.Fields().ByName("uid") != nil {
			t.Fatal("the older type knows uid")
		}
		if e.Get(ed.Fields().ByName("name")).String() == "old" {
			e.Set(ed.Fields().ByName("archived"), protoreflect.ValueOfBool(true))
		}
	}
	rewritten, err := proto.Marshal(m.Interface())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Put(ctx, masterBlockKey, bytes.NewReader(rewritten), nil, obj.ETag); err != nil {
		t.Fatal(err)
	}
	info, err := DescribeFilesystem(ctx, base, "old")
	if err != nil {
		t.Fatal(err)
	}
	if info.UID != mine || !info.Archived {
		t.Fatalf("after older code rewrote the block: uid %q archived %v; want %s true", info.UID, info.Archived, mine)
	}
}

// oldMasterBlockType is MasterBlock as code from before the uid field
// has it: the same file descriptor with FilesystemEntry's uid removed.
func oldMasterBlockType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := protodesc.ToFileDescriptorProto(pb.File_spec_proto_icbfs_v1_master_proto)
	fdp.Name = proto.String("old/" + fdp.GetName())
	fdp.Package = proto.String("old." + fdp.GetPackage())
	for _, msg := range fdp.MessageType {
		if msg.GetName() != "FilesystemEntry" {
			continue
		}
		var kept []*descriptorpb.FieldDescriptorProto
		for _, f := range msg.Field {
			if f.GetName() != "uid" {
				kept = append(kept, f)
			}
		}
		msg.Field = kept
	}
	for _, msg := range fdp.MessageType {
		for _, f := range msg.Field {
			pkg := string(pb.File_spec_proto_icbfs_v1_master_proto.Package())
			if f.GetTypeName() == "."+pkg+".FilesystemEntry" {
				f.TypeName = proto.String(".old." + pkg + ".FilesystemEntry")
			}
		}
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	md := fd.Messages().ByName("MasterBlock")
	if md == nil {
		t.Fatal("no MasterBlock in the older descriptor")
	}
	return dynamicpb.NewMessageType(md)
}
