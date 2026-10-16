package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

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
	empty, err := proto.Marshal(&pb.MasterBlock{})
	if err != nil {
		return err
	}
	_, err = store.PutIfAbsent(ctx, masterBlockKey, bytes.NewReader(empty), nil)
	if objstore.IsPreconditionFailed(err) {
		return nil
	}
	return err
}

func readMaster(ctx context.Context, store objstore.Store) (*pb.MasterBlock, *objstore.Object, error) {
	body, obj, err := store.Get(ctx, masterBlockKey)
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
			idx = len(mb.Filesystems)
			mb.Filesystems = append(mb.Filesystems, &pb.FilesystemEntry{})
		}
		created := &pb.FilesystemEntry{Name: name, Size: entry.Size, PrimaryWindows: entry.PrimaryWindows, MaxFileSize: entry.MaxFileSize, DirBlockEntries: entry.DirBlockEntries, Created: entry.Created}
		mb.Filesystems[idx] = created

		data, err := proto.Marshal(mb)
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
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
		mb, obj, err := readMaster(ctx, store)
		if err != nil {
			return mapNotFound(err)
		}
		found := false
		for _, e := range mb.Filesystems {
			if e.Name == fsName {
				mutate(e)
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
		data, err := proto.Marshal(mb)
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
	if err := eraseAllVersions(ctx, store, prefix); err != nil {
		return fmt.Errorf("prune %s: %w", fsName, err)
	}

	return updateFilesystemEntry(ctx, store, fsName, func(e *pb.FilesystemEntry) {
		e.Name = ""
		e.Size = 0
		e.Archived = false
	})
}
