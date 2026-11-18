package icbfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// masterBlockKey is the one, fixed, well-known object per bucket that
// registers every filesystem in it — see docs/architecture.md's master block section.
const masterBlockKey = "_master"

// ErrArchived is every mutating operation's answer on an archived
// filesystem. Reads still work: archiving is so it can be backed up once
// more before it's pruned.
var ErrArchived = errors.New("filesystem is archived: read-only")

// ErrReadOnly is returned by any mutating operation on a filesystem
// opened with Filesystem.BootstrapReadOnly — the same error the
// objstore.AsOf view underneath returns for a write.
var ErrReadOnly = objstore.ErrReadOnly

// bootstrapMaster creates the bucket's master block if it doesn't exist.
//
// It uses PutIfAbsent, not Head then Put: with Head then Put, two first
// callers could both see nothing and both write an empty block, and the
// later write could erase a filesystem the other had just registered. With
// PutIfAbsent one create wins and the others' precondition failures just
// mean the block exists.
func bootstrapMaster(ctx context.Context, store objstore.Store) error {
	empty, err := encodeMaster(&pb.MasterBlock{})
	if err != nil {
		return err
	}
	_, err = store.PutIfAbsent(ctx, masterBlockKey, bytes.NewReader(empty), nil)
	if objstore.IsPreconditionFailed(err) {
		return nil
	}
	return err
}

// Layouts this package reads and writes; see the fields in
// spec/proto/icbfs/v1/master.proto. FormatVersion is the format of a
// filesystem's objects as a whole, recorded in its master-block entry: a
// filesystem is created at it, and raised to formatVersionRefs at its
// first stub (raiseFormat). One in a format this build doesn't read
// (newer than maxFormatVersion) isn't opened at all.
const (
	masterLayout  = 1
	FormatVersion = 1
)

// encodeMaster serializes mb in this package's layout.
func encodeMaster(mb *pb.MasterBlock) ([]byte, error) {
	mb.Layout = masterLayout
	return proto.Marshal(mb)
}

func readMaster(ctx context.Context, store objstore.Store) (*pb.MasterBlock, *objstore.Object, error) {
	body, obj, err := store.Get(objstore.CacheContent(ctx), masterBlockKey)
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, err
	}
	var mb pb.MasterBlock
	if len(data) > 0 {
		if err := proto.Unmarshal(data, &mb); err != nil {
			return nil, nil, err
		}
		if err := block.CheckLayout("master block", mb.Layout, masterLayout); err != nil {
			return nil, nil, err
		}
	}
	return &mb, obj, nil
}

// registerFilesystem resolves name to its filesystem ID, registering it in
// the lowest free slot (or a new one at the end) the first time it's seen.
// For a name already registered it returns the existing ID and entry;
// entry's size, primary mode and max file size are used only at creation.
//
// The ID is the slot's position as 4 hex digits, not a stored field. The
// list only ever grows, never shrinks or reorders, so a slot's position,
// and the prefix every object of that filesystem carries, never changes.
func registerFilesystem(ctx context.Context, store objstore.Store, entry *pb.FilesystemEntry) (id string, stored *pb.FilesystemEntry, existed bool, err error) {
	if err := bootstrapMaster(ctx, store); err != nil {
		return "", nil, false, err
	}
	name := entry.Name
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
		mb, obj, err := readMaster(ctx, store)
		if err != nil {
			return "", nil, false, err
		}

		for i, e := range mb.Filesystems {
			if e.Name == name {
				return fsID(i), e, true, nil
			}
		}

		idx := -1
		for i, e := range mb.Filesystems {
			if e.Name == "" {
				idx = i
				break
			}
		}
		if idx == -1 {
			if len(mb.Filesystems) >= MaxFilesystems {
				return "", nil, false, fmt.Errorf("register filesystem %q: %w", name, ErrTooManyFilesystems)
			}
			idx = len(mb.Filesystems)
			mb.Filesystems = append(mb.Filesystems, &pb.FilesystemEntry{})
		}
		uid, err := newFilesystemUID()
		if err != nil {
			return "", nil, false, err
		}
		created := &pb.FilesystemEntry{Name: name, Size: entry.Size, PrimaryWindows: entry.PrimaryWindows, MaxFileSize: entry.MaxFileSize, DirBlockEntries: entry.DirBlockEntries, Created: entry.Created, FormatVersion: FormatVersion, Uid: uid}
		mb.Filesystems[idx] = created

		data, err := encodeMaster(mb)
		if err != nil {
			return "", nil, false, err
		}
		_, err = store.Put(ctx, masterBlockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		if err != nil {
			return "", nil, false, err
		}
		return fsID(idx), created, false, nil
	}
	return "", nil, false, fmt.Errorf("register filesystem %q: exceeded %d retries", name, maxTreeRetries)
}

// newFilesystemUID is a new filesystem's own id (FilesystemEntry.uid):
// 16 random bytes as 32 lowercase hex digits.
func newFilesystemUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("making the filesystem's id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// FilesystemUID is the filesystem name's own id (Info.UID), giving it one
// first if it was registered before icbfs wrote one (interloc
// docs/STATES.md DB-18). The one write is conditional on the version of
// the master block it read: if anyone changed the block meanwhile, it is
// read again and the id another caller gave is taken; the write is never
// made over a block it did not read. A store that refuses the write (one
// opened read-only) gives its error, never an id that is not stored.
// ErrNotFound if there is no filesystem called name.
func FilesystemUID(ctx context.Context, store objstore.Store, name string) (string, error) {
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
		mb, obj, err := readMaster(ctx, store)
		if err != nil {
			if errors.Is(mapNotFound(err), ErrNotFound) {
				return "", fmt.Errorf("no filesystem %q in this bucket: %w", name, ErrNotFound)
			}
			return "", err
		}
		var e *pb.FilesystemEntry
		for _, x := range mb.Filesystems {
			if x.Name == name {
				e = x
				break
			}
		}
		if e == nil {
			return "", fmt.Errorf("no filesystem %q in this bucket: %w", name, ErrNotFound)
		}
		if e.Uid != "" {
			return e.Uid, nil
		}
		if e.Uid, err = newFilesystemUID(); err != nil {
			return "", err
		}
		data, err := encodeMaster(mb)
		if err != nil {
			return "", err
		}
		_, err = store.Put(ctx, masterBlockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue // changed since it was read: read it again
		}
		if err != nil {
			return "", fmt.Errorf("giving filesystem %q its id: %w", name, err)
		}
		return e.Uid, nil
	}
	return "", fmt.Errorf("giving filesystem %q its id: exceeded %d retries", name, maxTreeRetries)
}

// MaxFilesystems is how many filesystems one bucket holds at once: as
// many as a 4-hex-digit ID can name. A pruned one's slot is reused.
const MaxFilesystems = 1 << 16

// fsID zero-pads a slot index to the 4-hex-digit filesystem ID format
// every object this filesystem owns is prefixed with.
func fsID(index int) string {
	return fmt.Sprintf("%04x", index)
}

// Archive marks fsName archived: its data stays, but mutating operations
// refuse with ErrArchived from each Filesystem's next Bootstrap. One
// already open doesn't notice.
func Archive(ctx context.Context, store objstore.Store, fsName string) error {
	return updateFilesystemEntry(ctx, store, fsName, func(e *pb.FilesystemEntry) {
		e.Archived = true
	})
}

func updateFilesystemEntry(ctx context.Context, store objstore.Store, fsName string, mutate func(*pb.FilesystemEntry)) error {
	return updateFilesystemEntryIf(ctx, store, fsName, func(e *pb.FilesystemEntry) bool { mutate(e); return true })
}

// updateFilesystemEntryIf is updateFilesystemEntry whose mutate can
// decline (false), and then the master block isn't written.
func updateFilesystemEntryIf(ctx context.Context, store objstore.Store, fsName string, mutate func(*pb.FilesystemEntry) bool) error {
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
		mb, obj, err := readMaster(ctx, store)
		if err != nil {
			return mapNotFound(err)
		}
		found, changed := false, false
		for _, e := range mb.Filesystems {
			if e.Name == fsName {
				changed = mutate(e)
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
		if !changed {
			return nil
		}
		data, err := encodeMaster(mb)
		if err != nil {
			return err
		}
		_, err = store.Put(ctx, masterBlockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("update filesystem entry %q: exceeded %d retries", fsName, maxTreeRetries)
}

// Resize changes fsName's declared capacity (StatFS's "Total"). It's stored
// in the master block, so every mount sees it at once.
func Resize(ctx context.Context, store objstore.Store, fsName string, newSize uint64) error {
	return updateFilesystemEntry(ctx, store, fsName, func(e *pb.FilesystemEntry) {
		e.Size = newSize
	})
}

// Prune deletes every version of every object under fsName's prefix
// (content, trash, names) and blanks its master block entry, freeing the
// slot. It doesn't itself require the filesystem to be archived; callers
// archive first (icbfs-cli and the API refuse otherwise).
func Prune(ctx context.Context, store objstore.Store, fsName string) error {
	mb, _, err := readMaster(ctx, store)
	if err != nil {
		return mapNotFound(err)
	}
	idx := -1
	for i, e := range mb.Filesystems {
		if e.Name == fsName {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ErrNotFound
	}
	prefix := fsID(idx) + "-"

	// Every version, not just the current objects: a filesystem that
	// later reuses this slot (and ID prefix) must start with no past.
	if err := store.EraseAll(ctx, prefix); err != nil {
		return fmt.Errorf("prune %s: %w", fsName, err)
	}
	// Its uploads in parts, outside its prefix (uploads.go).
	if err := store.EraseAll(ctx, fsID(idx)+".upload-"); err != nil {
		return fmt.Errorf("prune %s: uploads: %w", fsName, err)
	}
	if err := store.EraseAll(ctx, uploadMarks+fsID(idx)); err != nil {
		return fmt.Errorf("prune %s: its uploads' mark: %w", fsName, err)
	}

	return updateFilesystemEntry(ctx, store, fsName, func(e *pb.FilesystemEntry) {
		e.Name = ""
		e.Size = 0
		e.Archived = false
	})
}
