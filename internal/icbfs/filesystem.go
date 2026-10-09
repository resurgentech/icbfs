package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// maxTreeRetries bounds the whole-operation retry loop for both the flat
// nlink CAS helper and the tree-aware insert/remove helpers: on a lost
// race (a concurrent writer touching the same node), the whole top-down
// operation is retried from scratch rather than retried node-by-node. See
// insertEntry/removeEntry.
const maxTreeRetries = 20

// maxEntriesPerBlock/maxChildrenPerBlock are deliberately small so tests
// can exercise splitting without creating thousands of files. Real
// tuning (per ARCHITECTURE.md: around mutation cost, not object size) is
// a separate, later concern.
const (
	maxEntriesPerBlock  = 8
	maxChildrenPerBlock = 8
)

var errRetry = errors.New("icbfs: lost a race, retry the whole operation")

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
	empty, err := (&block.Block{Kind: block.Leaf}).Encode()
	if err != nil {
		return err
	}
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Mtime: time.Now()}
	_, err = f.store.Put(ctx, f.rootKey, bytes.NewReader(empty), metadataFromAttr(attr), "")
	return err
}

// readBlock fetches and decodes the directory block at key, along with its
// own object metadata (needed so callers can preserve it, and CAS against
// its ETag, across a read-modify-write).
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

func attrFromObject(key string, obj *objstore.Object) Attr {
	mtime := parseMetaTime(obj.Metadata, metaMtime)
	if mtime.IsZero() {
		mtime = obj.LastModified // fallback for objects written before mtime tracking existed
	}
	return Attr{
		Mode:  parseMetaUint(obj.Metadata, metaMode),
		Uid:   parseMetaUint(obj.Metadata, metaUid),
		Gid:   parseMetaUint(obj.Metadata, metaGid),
		Nlink: parseMetaUint(obj.Metadata, metaNlink),
		Size:  obj.Size,
		Mtime: mtime,
		Ctime: obj.LastModified,
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

// --- Directory tree traversal (median-key B-tree; see internal/block) ---

type pathStep struct {
	key string
	blk *block.Block
	obj *objstore.Object
}

// descendForWrite walks from rootKey down to the leaf that would contain
// name, returning every node visited along the way (root-to-leaf order).
// Each step carries the ETag needed to CAS-protect a write to it.
func (f *Filesystem) descendForWrite(ctx context.Context, rootKey, name string) ([]pathStep, error) {
	var path []pathStep
	key := rootKey
	for {
		blk, obj, err := f.readBlock(ctx, key)
		if err != nil {
			return nil, err
		}
		path = append(path, pathStep{key, blk, obj})
		if blk.Kind == block.Leaf {
			return path, nil
		}
		key = blk.ChildFor(name).UUID
	}
}

// findInTree locates name within the (possibly multi-level) shard tree
// rooted at rootKey, without any intent to mutate it.
func (f *Filesystem) findInTree(ctx context.Context, rootKey, name string) (block.Entry, bool, error) {
	key := rootKey
	for {
		blk, _, err := f.readBlock(ctx, key)
		if err != nil {
			return block.Entry{}, false, err
		}
		if blk.Kind == block.Leaf {
			e, ok := blk.Find(name)
			return e, ok, nil
		}
		key = blk.ChildFor(name).UUID
	}
}

// collectEntries gathers every entry in the shard tree rooted at rootKey,
// across however many levels it currently has.
func (f *Filesystem) collectEntries(ctx context.Context, rootKey string) ([]block.Entry, error) {
	blk, _, err := f.readBlock(ctx, rootKey)
	if err != nil {
		return nil, err
	}
	if blk.Kind == block.Leaf {
		return blk.Entries, nil
	}
	var all []block.Entry
	for _, c := range blk.Children {
		sub, err := f.collectEntries(ctx, c.UUID)
		if err != nil {
			return nil, err
		}
		all = append(all, sub...)
	}
	return all, nil
}

// insertEntry adds name -> uuid to the directory rooted at rootKey,
// splitting nodes top-down as needed. On a lost race against a concurrent
// writer anywhere along the path, the whole operation is retried from
// scratch (any already-written split halves from the abandoned attempt
// are left as harmless orphans — a known simplification, not a
// correctness problem: nothing ever comes to reference them).
func (f *Filesystem) insertEntry(ctx context.Context, rootKey, name, uuid string, typ EntryType) error {
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		err := f.tryInsert(ctx, rootKey, name, uuid, typ)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errRetry) {
			return err
		}
	}
	return fmt.Errorf("insert %q into %s: exceeded %d retries", name, rootKey, maxTreeRetries)
}

func (f *Filesystem) tryInsert(ctx context.Context, rootKey, name, uuid string, typ EntryType) error {
	path, err := f.descendForWrite(ctx, rootKey, name)
	if err != nil {
		return err
	}
	leaf := path[len(path)-1]
	if err := leaf.blk.Insert(block.Entry{Name: name, UUID: uuid, Type: typ}); err != nil {
		if errors.Is(err, block.ErrExists) {
			return ErrExists
		}
		return err
	}
	return f.writeBackWithSplits(ctx, path, len(path)-1, leaf.blk)
}

// writeBackWithSplits writes path[idx].blk back (now holding one extra
// entry or child), splitting it and propagating a new sibling pointer
// upward if it overflows. idx == 0 is rootKey itself: since that key's
// identity must stay fixed (it's either the filesystem's well-known root
// name, or a directory's own UUID as referenced by its parent), a split
// there mints two brand new children and rewrites rootKey's own content
// as a fresh Internal node pointing at both — unlike every other level,
// which keeps its existing key for the left half and only mints a new
// UUID for the right half.
func (f *Filesystem) writeBackWithSplits(ctx context.Context, path []pathStep, idx int, blk *block.Block) error {
	overflowing := (blk.Kind == block.Leaf && len(blk.Entries) > maxEntriesPerBlock) ||
		(blk.Kind == block.Internal && len(blk.Children) > maxChildrenPerBlock)

	step := path[idx]
	if !overflowing {
		data, err := blk.Encode()
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, step.key, bytes.NewReader(data), step.obj.Metadata, step.obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			return errRetry
		}
		return err
	}

	var left, right *block.Block
	var sep string
	if blk.Kind == block.Leaf {
		left, right, sep = blk.SplitLeaf()
	} else {
		left, right, sep = blk.SplitInternal()
	}

	if idx == 0 {
		leftUUID, err := NewUUIDv7()
		if err != nil {
			return err
		}
		rightUUID, err := NewUUIDv7()
		if err != nil {
			return err
		}
		if err := f.putBlock(ctx, leftUUID, left, step.obj.Metadata, ""); err != nil {
			return err
		}
		if err := f.putBlock(ctx, rightUUID, right, step.obj.Metadata, ""); err != nil {
			return err
		}
		newRoot := &block.Block{Kind: block.Internal, Children: []block.Child{
			{MinKey: "", UUID: leftUUID},
			{MinKey: sep, UUID: rightUUID},
		}}
		err = f.putBlock(ctx, step.key, newRoot, step.obj.Metadata, step.obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			return errRetry
		}
		return err
	}

	// Not the tree's own root: keep this node's existing key for the left
	// half, mint a new UUID for the right half, and propagate a pointer
	// to it into the parent.
	if err := f.putBlock(ctx, step.key, left, step.obj.Metadata, step.obj.ETag); err != nil {
		if objstore.IsPreconditionFailed(err) {
			return errRetry
		}
		return err
	}
	rightUUID, err := NewUUIDv7()
	if err != nil {
		return err
	}
	if err := f.putBlock(ctx, rightUUID, right, step.obj.Metadata, ""); err != nil {
		return err
	}

	parent := path[idx-1].blk
	parent.InsertChild(block.Child{MinKey: sep, UUID: rightUUID})
	return f.writeBackWithSplits(ctx, path, idx-1, parent)
}

func (f *Filesystem) putBlock(ctx context.Context, key string, blk *block.Block, metadata map[string]string, ifMatch string) error {
	data, err := blk.Encode()
	if err != nil {
		return err
	}
	_, err = f.store.Put(ctx, key, bytes.NewReader(data), metadata, ifMatch)
	return err
}

// removeEntry deletes name from the directory rooted at rootKey. Merging
// underfull nodes back together is not implemented (see the block
// package doc) — this only ever shrinks the leaf that held name.
func (f *Filesystem) removeEntry(ctx context.Context, rootKey, name string) (block.Entry, error) {
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		removed, err := f.tryRemove(ctx, rootKey, name)
		if err == nil {
			return removed, nil
		}
		if !errors.Is(err, errRetry) {
			return block.Entry{}, err
		}
	}
	return block.Entry{}, fmt.Errorf("remove %q from %s: exceeded %d retries", name, rootKey, maxTreeRetries)
}

func (f *Filesystem) tryRemove(ctx context.Context, rootKey, name string) (block.Entry, error) {
	path, err := f.descendForWrite(ctx, rootKey, name)
	if err != nil {
		return block.Entry{}, err
	}
	leaf := path[len(path)-1]
	removed, err := leaf.blk.Remove(name)
	if err != nil {
		if errors.Is(err, block.ErrNotFound) {
			return block.Entry{}, ErrNotFound
		}
		return block.Entry{}, err
	}
	data, err := leaf.blk.Encode()
	if err != nil {
		return block.Entry{}, err
	}
	_, err = f.store.Put(ctx, leaf.key, bytes.NewReader(data), leaf.obj.Metadata, leaf.obj.ETag)
	if objstore.IsPreconditionFailed(err) {
		return block.Entry{}, errRetry
	}
	if err != nil {
		return block.Entry{}, err
	}
	return removed, nil
}

// --- Filesystem operations ---

// Lookup finds name within the directory at dirKey.
func (f *Filesystem) Lookup(ctx context.Context, dirKey, name string) (block.Entry, Attr, error) {
	entry, ok, err := f.findInTree(ctx, dirKey, name)
	if err != nil {
		return block.Entry{}, Attr{}, err
	}
	if !ok {
		return block.Entry{}, Attr{}, ErrNotFound
	}
	attr, err := f.Stat(ctx, entry.UUID)
	if err != nil {
		return block.Entry{}, Attr{}, err
	}
	return entry, attr, nil
}

// ReadDir lists the entries of the directory at dirKey, across however
// many shard-tree levels it currently has.
func (f *Filesystem) ReadDir(ctx context.Context, dirKey string) ([]block.Entry, error) {
	return f.collectEntries(ctx, dirKey)
}

// Mkdir creates a new, empty directory named name inside dirKey.
func (f *Filesystem) Mkdir(ctx context.Context, dirKey, name string, mode, uid, gid uint32) (string, Attr, error) {
	newUUID, err := NewUUIDv7()
	if err != nil {
		return "", Attr{}, err
	}
	empty, err := (&block.Block{Kind: block.Leaf}).Encode()
	if err != nil {
		return "", Attr{}, err
	}
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Mtime: time.Now()}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader(empty), metadataFromAttr(attr), ""); err != nil {
		return "", Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, newUUID, TypeDir); err != nil {
		_ = f.store.Delete(ctx, newUUID, "") // best-effort cleanup of the orphaned block
		return "", Attr{}, err
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
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Mtime: time.Now()}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader(nil), metadataFromAttr(attr), ""); err != nil {
		return "", Attr{}, err
	}
	if _, err := f.store.Put(ctx, nlinkKey(newUUID), strings.NewReader("1"), nil, ""); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, newUUID, TypeFile); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		_ = f.store.Delete(ctx, nlinkKey(newUUID), "")
		return "", Attr{}, err
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
	attr := Attr{Mode: 0777, Uid: uid, Gid: gid, Nlink: 1, Mtime: time.Now()}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader([]byte(target)), metadataFromAttr(attr), ""); err != nil {
		return "", Attr{}, err
	}
	if _, err := f.store.Put(ctx, nlinkKey(newUUID), strings.NewReader("1"), nil, ""); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, newUUID, TypeSymlink); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		_ = f.store.Delete(ctx, nlinkKey(newUUID), "")
		return "", Attr{}, err
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

// nlinkKey is the dedicated side object tracking a file/symlink's hard
// link count.
//
// This exists because of a hard limitation discovered empirically against
// real MinIO, not assumed: ETag on an S3-compatible store is a hash of
// the object's *body*, so a metadata-only update (nlink living in object
// metadata, per ARCHITECTURE.md's general metadata model) never changes
// the ETag when the body is unchanged — which means ETag-based CAS
// (Put/UpdateMetadata's ifMatch) provides zero protection for exactly the
// concurrent-nlink-update case it was meant to guard. It was also
// confirmed that MinIO does not enforce DeleteObject's If-Match at all,
// so a conditional delete isn't available as a fallback either.
//
// The fix: nlink for a hardlink-capable node lives in a tiny side object
// whose *body* is the decimal count, so every change is a real content
// write and ETag-based CAS (proven to work for content writes) is
// meaningful again. The main object's metadata nlink field is kept as a
// best-effort cache so Stat()/Getattr stay single-round-trip in the
// common (never-hardlinked) case; adjustNlink, below, is the only
// authoritative source, and the only thing that decides whether the main
// object is actually deleted.
func nlinkKey(uuid string) string { return uuid + ".nlink" }

const nlinkTombstone = "0"

// adjustNlink changes uuid's hard link count by delta via the CAS-
// protected side object described above. If the count reaches zero, it
// writes the tombstone value under the same CAS check (so a concurrent
// Link racing to increment at the same moment necessarily loses the race
// at this Put and retries against the fresh state, never silently
// resurrecting a target this call just decided to delete), then deletes
// both the side object and the main object.
func (f *Filesystem) adjustNlink(ctx context.Context, uuid string, delta int) error {
	nk := nlinkKey(uuid)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		body, obj, err := f.store.Get(ctx, nk)
		if err != nil {
			return mapNotFound(err)
		}
		data, readErr := io.ReadAll(body)
		body.Close()
		if readErr != nil {
			return readErr
		}
		count, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if count <= 0 {
			// Already tombstoned by a prior/concurrent Unlink; nothing to
			// resurrect, and nothing further for a decrement to do either.
			return ErrNotFound
		}
		newCount := count + delta

		if newCount <= 0 {
			_, err := f.store.Put(ctx, nk, strings.NewReader(nlinkTombstone), nil, obj.ETag)
			if objstore.IsPreconditionFailed(err) {
				continue
			}
			if err != nil {
				return err
			}
			// We are the one who observed and serialized "really at
			// zero" via the CAS write above — no concurrent Link can
			// have raced past this point, since it would have observed
			// either our tombstone (and bailed, above) or lost its own
			// CAS race against it. Safe to physically delete now.
			_ = f.store.Delete(ctx, nk, "")
			_ = f.store.Delete(ctx, uuid, "")
			return nil
		}

		_, err = f.store.Put(ctx, nk, strings.NewReader(strconv.Itoa(newCount)), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		if err != nil {
			return err
		}
		// Best-effort cache refresh, not itself CAS-protected: the side
		// object above is already the sole source of truth for
		// correctness, so a race here can only make Stat() briefly
		// report a stale count, never corrupt the real decision.
		if cur, err := f.store.Head(ctx, uuid); err == nil {
			attr := attrFromObject(uuid, cur)
			attr.Nlink = uint32(newCount)
			_, _ = f.store.UpdateMetadata(ctx, uuid, metadataFromAttr(attr), "")
		}
		return nil
	}
	return fmt.Errorf("adjust nlink %s: exceeded %d retries", uuid, maxTreeRetries)
}

// Link creates a hard link named name inside dirKey, pointing at the
// existing node targetUUID/targetType, incrementing its nlink via the
// CAS-protected side object.
func (f *Filesystem) Link(ctx context.Context, dirKey, name, targetUUID string, targetType EntryType) (Attr, error) {
	if err := f.adjustNlink(ctx, targetUUID, +1); err != nil {
		return Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, targetUUID, targetType); err != nil {
		_ = f.adjustNlink(ctx, targetUUID, -1) // best-effort rollback
		return Attr{}, err
	}
	return f.Stat(ctx, targetUUID)
}

// Unlink removes a non-directory entry named name from dirKey,
// decrementing the target's nlink via the CAS-protected side object and
// deleting its blob once the count reaches zero.
func (f *Filesystem) Unlink(ctx context.Context, dirKey, name string) error {
	entry, ok, err := f.findInTree(ctx, dirKey, name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if entry.Type == TypeDir {
		return ErrIsDir
	}

	if _, err := f.removeEntry(ctx, dirKey, name); err != nil {
		return err
	}

	if err := f.adjustNlink(ctx, entry.UUID, -1); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// Rmdir removes an empty directory named name from dirKey.
func (f *Filesystem) Rmdir(ctx context.Context, dirKey, name string) error {
	entry, ok, err := f.findInTree(ctx, dirKey, name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if entry.Type != TypeDir {
		return ErrNotDir
	}

	children, err := f.collectEntries(ctx, entry.UUID)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return ErrNotEmpty
	}

	if _, err := f.removeEntry(ctx, dirKey, name); err != nil {
		return err
	}
	return f.store.Delete(ctx, entry.UUID, "")
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
// existing mode/uid/gid/nlink and stamping a fresh mtime (this is a
// content write, by definition).
func (f *Filesystem) WriteFile(ctx context.Context, key string, data []byte, attr Attr) (Attr, error) {
	attr.Size = int64(len(data))
	attr.Mtime = time.Now()
	if _, err := f.store.Put(ctx, key, bytes.NewReader(data), metadataFromAttr(attr), ""); err != nil {
		return Attr{}, err
	}
	return f.Stat(ctx, key)
}

// SetAttr applies the given field changes (any of which may be nil/unset)
// to the node at key. A mode/uid/gid-only change is a metadata-only
// update (ARCHITECTURE.md: this rides the version history as ctime, for
// free, and never touches the parent directory, and does not disturb
// mtime); a size change rewrites content via WriteFile, which does bump
// mtime.
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

	obj, err := f.store.UpdateMetadata(ctx, key, metadataFromAttr(attr), "")
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
