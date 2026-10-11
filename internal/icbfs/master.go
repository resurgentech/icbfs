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
// registers every filesystem in it — see ARCHITECTURE.md's Multiple
// filesystems per bucket section.
const masterBlockKey = "_master"

// ErrArchived is returned by any mutating operation on a filesystem
// whose master block entry has archived = true. Reads still succeed —
// archiving exists specifically so a filesystem can be read/backed up
// one last time before it's pruned, not to make it disappear outright.
var ErrArchived = errors.New("filesystem is archived: read-only")

// bootstrapMaster ensures the master block exists, creating an empty one
// if this bucket has never been used for icbfs before.
//
// This must use a true create-if-absent primitive (PutIfAbsent), not a
// Head-then-unconditional-Put — found by testing concurrent first-time
// registrations against real MinIO, not assumed: a Head-then-Put has a
// window where two concurrent callers both observe "doesn't exist yet"
// and both then write an unconditional empty master block, and whichever
// one's unconditional write lands *after* the other's subsequent
// registerFilesystem CAS write silently stomps it back to empty,
// discarding that registration. PutIfAbsent closes that window: at most
// one caller's create succeeds, every other caller's PutIfAbsent fails
// with a precondition error (expected and harmless here — it just means
// the block already exists, exactly what bootstrapping again is for).
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

// registerFilesystem resolves name to its filesystem ID, registering a
// new entry (reusing the lowest-indexed free slot, or appending) if this
// is the first time this name has been seen. Idempotent: calling it
// again for an already-registered name just returns that name's existing
// ID, archived status, and primary-mode (primaryWindows is otherwise
// ignored, same as size already was — both are "only used the first
// time a filesystem name is created," per Filesystem.Bootstrap's
// existing no-op-if-already-exists contract for root blocks).
//
// The ID is the slot's position, zero-padded to 4 hex digits — not a
// separately stored field. See ARCHITECTURE.md for why: the master
// block's list can only grow, never shrink or reorder, specifically so a
// slot's position (and therefore the ID every object in that filesystem
// is already permanently keyed with) never changes once assigned.
func registerFilesystem(ctx context.Context, store objstore.Store, name string, size uint64, primaryWindows bool) (id string, archived, storedPrimaryWindows bool, err error) {
	if err := bootstrapMaster(ctx, store); err != nil {
		return "", false, false, err
	}
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		mb, obj, err := readMaster(ctx, store)
		if err != nil {
			return "", false, false, err
		}

		for i, e := range mb.Filesystems {
			if e.Name == name {
				return fsID(i), e.Archived, e.PrimaryWindows, nil
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
		mb.Filesystems[idx] = &pb.FilesystemEntry{Name: name, Size: size, Archived: false, PrimaryWindows: primaryWindows}

		data, err := proto.Marshal(mb)
		if err != nil {
			return "", false, false, err
		}
		_, err = store.Put(ctx, masterBlockKey, bytes.NewReader(data), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		if err != nil {
			return "", false, false, err
		}
		return fsID(idx), false, primaryWindows, nil
	}
	return "", false, false, fmt.Errorf("register filesystem %q: exceeded %d retries", name, maxTreeRetries)
}

// fsID zero-pads a slot index to the 4-hex-digit filesystem ID format
// every object this filesystem owns is prefixed with.
func fsID(index int) string {
	return fmt.Sprintf("%04x", index)
}

// Archive marks fsName's master block entry as archived: the filesystem
// (and all its data) still exists, but every mutating Filesystem
// operation will refuse with ErrArchived from the next Bootstrap
// onward — see ErrArchived's doc comment on why reads are unaffected,
// and ARCHITECTURE.md's Multiple filesystems per bucket section on why
// this is checked at mount/session start rather than continuously: an
// already-open Filesystem does not notice an Archive call made after it
// bootstrapped, by design, not by oversight.
func Archive(ctx context.Context, store objstore.Store, fsName string) error {
	return updateFilesystemEntry(ctx, store, fsName, func(e *pb.FilesystemEntry) {
		e.Archived = true
	})
}

func updateFilesystemEntry(ctx context.Context, store objstore.Store, fsName string, mutate func(*pb.FilesystemEntry)) error {
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
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

// Resize changes fsName's declared capacity (StatFS's "Total", the
// `size` originally fixed at Bootstrap/creation time) to newSize.
// Unlike Locking's EnableLocking flag, declared size is a real,
// shared property of the filesystem itself — ARCHITECTURE.md's own
// master-block `size` field, same category as archived — so this
// takes effect for every mount of fsName immediately, not just the
// caller's own. See ASSUMPTIONS.md's D-cleanup entry: this closes the
// "no way to change it after the fact" gap that entry originally
// flagged.
func Resize(ctx context.Context, store objstore.Store, fsName string, newSize uint64) error {
	return updateFilesystemEntry(ctx, store, fsName, func(e *pb.FilesystemEntry) {
		e.Size = newSize
	})
}

// Prune physically deletes every object fsName owns (everything under
// its ID prefix, the same listing df/du use for "Used") and then blanks
// its master block entry, freeing the slot for reuse by a later Create.
// Does not require the filesystem to be archived first — callers are
// expected to archive, let any in-flight backup finish, and then prune,
// but Prune itself doesn't enforce that ordering.
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

	objs, err := store.ListByPrefix(ctx, prefix)
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := store.Delete(ctx, o.Key, ""); err != nil {
			return fmt.Errorf("prune %s: delete %s: %w", fsName, o.Key, err)
		}
	}

	return updateFilesystemEntry(ctx, store, fsName, func(e *pb.FilesystemEntry) {
		e.Name = ""
		e.Size = 0
		e.Archived = false
	})
}
