package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

const maxCASRetries = 20

// Filesystem is the core, access-layer-independent filesystem logic: it
// turns object-store primitives plus the directory block format into
// lookup/mkdir/create/read/write/link/etc. operations.
type Filesystem struct {
	store   objstore.Store
	rootKey string
}

// New builds a Filesystem backed by store, rooted at the named filesystem's
// well-known root key (see ARCHITECTURE.md: root blocks are discovered by
// naming convention, not a random UUID).
func New(store objstore.Store, fsName string) *Filesystem {
	return &Filesystem{store: store, rootKey: "root/" + fsName}
}

// RootKey returns the block key identifying this filesystem's root.
func (f *Filesystem) RootKey() string { return f.rootKey }

// Bootstrap ensures the root block exists, creating an empty one if this
// is a brand new filesystem.
func (f *Filesystem) Bootstrap(ctx context.Context, mode, uid, gid uint32) error {
	if _, err := f.store.Head(ctx, f.rootKey); err == nil {
		return nil
	}
	empty, err := (&block.Block{}).Encode()
	if err != nil {
		return err
	}
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1}
	_, err = f.store.Put(ctx, f.rootKey, bytes.NewReader(empty), metadataFromAttr(attr), "")
	return err
}

// readBlock fetches and decodes the directory block at key, along with its
// own object metadata (needed so callers can preserve it across a
// read-modify-write).
func (f *Filesystem) readBlock(ctx context.Context, key string) (*block.Block, *objstore.Object, error) {
	body, obj, err := f.store.Get(ctx, key)
	if err != nil {
		return nil, nil, mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, err
	}
	blk, err := block.Decode(data)
	if err != nil {
		return nil, nil, err
	}
	return blk, obj, nil
}

// updateBlock applies mutate to the directory block at key under
// compare-and-swap, retrying on a lost race (ARCHITECTURE.md's
// concurrency decision: conditional writes via ETag, not a lock service).
func (f *Filesystem) updateBlock(ctx context.Context, key string, mutate func(*block.Block) error) error {
	for attempt := 0; attempt < maxCASRetries; attempt++ {
		blk, obj, err := f.readBlock(ctx, key)
		if err != nil {
			return err
		}
		if err := mutate(blk); err != nil {
			return err // semantic error (e.g. ErrExists/ErrNotFound); no retry
		}
		data, err := blk.Encode()
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, key, bytes.NewReader(data), obj.Metadata, obj.ETag)
		if err == nil {
			return nil
		}
		if objstore.IsPreconditionFailed(err) {
			continue // someone else wrote first; re-read and retry
		}
		return err
	}
	return fmt.Errorf("updateBlock %s: exceeded %d CAS retries", key, maxCASRetries)
}

func attrFromObject(key string, obj *objstore.Object) Attr {
	return Attr{
		Mode:  parseMetaUint(obj.Metadata, metaMode),
		Uid:   parseMetaUint(obj.Metadata, metaUid),
		Gid:   parseMetaUint(obj.Metadata, metaGid),
		Nlink: parseMetaUint(obj.Metadata, metaNlink),
		Size:  obj.Size,
		Mtime: obj.LastModified,
		Btime: Btime(key),
	}
}

// Stat returns the attributes of the node identified by key (a UUID, or
// this filesystem's root key).
func (f *Filesystem) Stat(ctx context.Context, key string) (Attr, error) {
	obj, err := f.store.Head(ctx, key)
	if err != nil {
		return Attr{}, mapNotFound(err)
	}
	return attrFromObject(key, obj), nil
}

// Lookup finds name within the directory at dirKey.
func (f *Filesystem) Lookup(ctx context.Context, dirKey, name string) (block.Entry, Attr, error) {
	blk, _, err := f.readBlock(ctx, dirKey)
	if err != nil {
		return block.Entry{}, Attr{}, err
	}
	entry, ok := blk.Find(name)
	if !ok {
		return block.Entry{}, Attr{}, ErrNotFound
	}
	attr, err := f.Stat(ctx, entry.UUID)
	if err != nil {
		return block.Entry{}, Attr{}, err
	}
	return entry, attr, nil
}

// ReadDir lists the entries of the directory at dirKey.
func (f *Filesystem) ReadDir(ctx context.Context, dirKey string) ([]block.Entry, error) {
	blk, _, err := f.readBlock(ctx, dirKey)
	if err != nil {
		return nil, err
	}
	return blk.Entries, nil
}

// Mkdir creates a new, empty directory named name inside dirKey.
func (f *Filesystem) Mkdir(ctx context.Context, dirKey, name string, mode, uid, gid uint32) (string, Attr, error) {
	newUUID, err := NewUUIDv7()
	if err != nil {
		return "", Attr{}, err
	}
	empty, err := (&block.Block{}).Encode()
	if err != nil {
		return "", Attr{}, err
	}
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader(empty), metadataFromAttr(attr), ""); err != nil {
		return "", Attr{}, err
	}

	err = f.updateBlock(ctx, dirKey, func(b *block.Block) error {
		return b.Insert(block.Entry{Name: name, UUID: newUUID, Type: TypeDir})
	})
	if err != nil {
		_ = f.store.Delete(ctx, newUUID) // best-effort cleanup of the orphaned block
		return "", Attr{}, mapExists(err)
	}
	attr.Btime = Btime(newUUID)
	return newUUID, attr, nil
}

// Create makes a new, empty regular file named name inside dirKey.
func (f *Filesystem) Create(ctx context.Context, dirKey, name string, mode, uid, gid uint32) (string, Attr, error) {
	newUUID, err := NewUUIDv7()
	if err != nil {
		return "", Attr{}, err
	}
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader(nil), metadataFromAttr(attr), ""); err != nil {
		return "", Attr{}, err
	}

	err = f.updateBlock(ctx, dirKey, func(b *block.Block) error {
		return b.Insert(block.Entry{Name: name, UUID: newUUID, Type: TypeFile})
	})
	if err != nil {
		_ = f.store.Delete(ctx, newUUID)
		return "", Attr{}, mapExists(err)
	}
	attr.Btime = Btime(newUUID)
	return newUUID, attr, nil
}

// Symlink creates a new symlink named name inside dirKey, pointing at
// target. The target string is stored as the object's content.
func (f *Filesystem) Symlink(ctx context.Context, dirKey, name, target string, uid, gid uint32) (string, Attr, error) {
	newUUID, err := NewUUIDv7()
	if err != nil {
		return "", Attr{}, err
	}
	attr := Attr{Mode: 0777, Uid: uid, Gid: gid, Nlink: 1}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader([]byte(target)), metadataFromAttr(attr), ""); err != nil {
		return "", Attr{}, err
	}

	err = f.updateBlock(ctx, dirKey, func(b *block.Block) error {
		return b.Insert(block.Entry{Name: name, UUID: newUUID, Type: TypeSymlink})
	})
	if err != nil {
		_ = f.store.Delete(ctx, newUUID)
		return "", Attr{}, mapExists(err)
	}
	attr.Size = int64(len(target))
	attr.Btime = Btime(newUUID)
	return newUUID, attr, nil
}

// Readlink returns a symlink's target.
func (f *Filesystem) Readlink(ctx context.Context, key string) (string, error) {
	body, _, err := f.store.Get(ctx, key)
	if err != nil {
		return "", mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Link creates a hard link named name inside dirKey, pointing at the
// existing node targetUUID/targetType. nlink on the target is incremented.
//
// This is not CAS-protected against a concurrent Link/Unlink racing on the
// same target's nlink — a known simplification at this stage, not a
// correctness guarantee.
func (f *Filesystem) Link(ctx context.Context, dirKey, name, targetUUID string, targetType EntryType) (Attr, error) {
	attr, err := f.Stat(ctx, targetUUID)
	if err != nil {
		return Attr{}, err
	}
	attr.Nlink++
	if _, err := f.store.UpdateMetadata(ctx, targetUUID, metadataFromAttr(attr)); err != nil {
		return Attr{}, err
	}

	err = f.updateBlock(ctx, dirKey, func(b *block.Block) error {
		return b.Insert(block.Entry{Name: name, UUID: targetUUID, Type: targetType})
	})
	if err != nil {
		// best-effort rollback of the nlink bump
		attr.Nlink--
		_, _ = f.store.UpdateMetadata(ctx, targetUUID, metadataFromAttr(attr))
		return Attr{}, mapExists(err)
	}
	return attr, nil
}

// Unlink removes a non-directory entry named name from dirKey, decrementing
// the target's nlink and deleting its blob once nlink reaches zero.
func (f *Filesystem) Unlink(ctx context.Context, dirKey, name string) error {
	var removed block.Entry
	err := f.updateBlock(ctx, dirKey, func(b *block.Block) error {
		e, ok := b.Find(name)
		if !ok {
			return ErrNotFound
		}
		if e.Type == TypeDir {
			return ErrIsDir
		}
		_, err := b.Remove(name)
		removed = e
		return err
	})
	if err != nil {
		return mapNotFoundOrIsDir(err)
	}

	attr, err := f.Stat(ctx, removed.UUID)
	if err != nil {
		return err
	}
	if attr.Nlink <= 1 {
		return f.store.Delete(ctx, removed.UUID)
	}
	attr.Nlink--
	_, err = f.store.UpdateMetadata(ctx, removed.UUID, metadataFromAttr(attr))
	return err
}

// Rmdir removes an empty directory named name from dirKey.
func (f *Filesystem) Rmdir(ctx context.Context, dirKey, name string) error {
	blk, _, err := f.readBlock(ctx, dirKey)
	if err != nil {
		return err
	}
	entry, ok := blk.Find(name)
	if !ok {
		return ErrNotFound
	}
	if entry.Type != TypeDir {
		return ErrNotDir
	}

	child, _, err := f.readBlock(ctx, entry.UUID)
	if err != nil {
		return err
	}
	if len(child.Entries) > 0 {
		return ErrNotEmpty
	}

	err = f.updateBlock(ctx, dirKey, func(b *block.Block) error {
		_, err := b.Remove(name)
		return err
	})
	if err != nil {
		return mapNotFoundOrIsDir(err)
	}
	return f.store.Delete(ctx, entry.UUID)
}

// ReadFile returns a regular file's whole content.
func (f *Filesystem) ReadFile(ctx context.Context, key string) ([]byte, Attr, error) {
	body, obj, err := f.store.Get(ctx, key)
	if err != nil {
		return nil, Attr{}, mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, Attr{}, err
	}
	return data, attrFromObject(key, obj), nil
}

// WriteFile replaces a regular file's whole content, preserving its
// existing mode/uid/gid/nlink.
func (f *Filesystem) WriteFile(ctx context.Context, key string, data []byte, attr Attr) (Attr, error) {
	attr.Size = int64(len(data))
	if _, err := f.store.Put(ctx, key, bytes.NewReader(data), metadataFromAttr(attr), ""); err != nil {
		return Attr{}, err
	}
	return f.Stat(ctx, key)
}

// SetAttr applies the given field changes (any of which may be nil/unset)
// to the node at key. A mode/uid/gid-only change is a metadata-only
// update (ARCHITECTURE.md: this rides the version history for free and
// never touches the parent directory); a size change rewrites content.
func (f *Filesystem) SetAttr(ctx context.Context, key string, mode, uid, gid *uint32, size *int64) (Attr, error) {
	attr, err := f.Stat(ctx, key)
	if err != nil {
		return Attr{}, err
	}
	if mode != nil {
		attr.Mode = *mode
	}
	if uid != nil {
		attr.Uid = *uid
	}
	if gid != nil {
		attr.Gid = *gid
	}

	if size != nil && *size != attr.Size {
		data, _, err := f.ReadFile(ctx, key)
		if err != nil {
			return Attr{}, err
		}
		resized := make([]byte, *size)
		copy(resized, data)
		return f.WriteFile(ctx, key, resized, attr)
	}

	obj, err := f.store.UpdateMetadata(ctx, key, metadataFromAttr(attr))
	if err != nil {
		return Attr{}, err
	}
	return attrFromObject(key, obj), nil
}

func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if objstore.IsNotFound(err) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

func mapExists(err error) error {
	if errors.Is(err, block.ErrExists) {
		return ErrExists
	}
	return err
}

func mapNotFoundOrIsDir(err error) error {
	if errors.Is(err, block.ErrNotFound) || errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, ErrIsDir) {
		return ErrIsDir
	}
	return err
}
