package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// maxTreeRetries bounds the whole-operation retry loop for the tree-aware
// insert/remove helpers and the file-metadata CAS helpers: on a lost race
// (a concurrent writer touching the same node), the whole top-down
// operation is retried from scratch rather than retried node-by-node. See
// insertEntry/removeEntry/adjustNlink/updateFileMetadata.
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
//
// id and archived are only known after Bootstrap resolves them against
// the master block (see ARCHITECTURE.md's Multiple filesystems per
// bucket section) — until then, a Filesystem returned by New isn't
// usable for anything that touches the store.
type Filesystem struct {
	store          objstore.Store
	fsName         string
	id             string // 4-hex-digit prefix, set by Bootstrap
	archived       bool   // set by Bootstrap; see ErrArchived
	lockingEnabled bool   // off by default; see EnableLocking, task B7
}

// New builds a Filesystem for the named filesystem. Call Bootstrap
// before using it for anything else — New itself does no I/O and
// doesn't know the filesystem's ID yet.
func New(store objstore.Store, fsName string) *Filesystem {
	return &Filesystem{store: store, fsName: fsName}
}

// EnableLocking turns on the Locking feature (tasks B2-B6) for this
// Filesystem — off by default. Unlike archived/size, this is a
// mount-time, local, in-memory choice, not a stored master-block
// property: per ARCHITECTURE.md's Locking section, Locking has real
// costs (an extra .lock object, and B5's escalation using it) "most
// mounts won't use," so it's opt-in per mount, not a shared,
// persisted-per-filesystem setting every other mount of the same
// filesystem is forced into. Call this any time after New, before
// relying on locking being available — there's no ordering requirement
// relative to Bootstrap.
func (f *Filesystem) EnableLocking(enabled bool) {
	f.lockingEnabled = enabled
}

// RootKey returns the block key identifying this filesystem's root:
// "<id>-root-<name>", a derived key, not stored anywhere separately —
// see ARCHITECTURE.md. Only meaningful after Bootstrap.
func (f *Filesystem) RootKey() string { return f.id + "-root-" + f.fsName }

// newKey mints a fresh, filesystem-scoped object key: this filesystem's
// ID prefix plus a new UUIDv7. Every object this filesystem creates
// (files, directories, their .metadata, future .lock objects) gets a key
// through this, never a bare NewUUIDv7() — that's what makes a single
// prefix-scoped listing capture this filesystem's entire footprint (see
// ARCHITECTURE.md).
func (f *Filesystem) newKey() (string, error) {
	raw, err := NewUUIDv7()
	if err != nil {
		return "", err
	}
	return f.id + "-" + raw, nil
}

// prefix returns the ID-prefix every object this filesystem owns is
// stored under (root included) — what ListByPrefix is scoped to for
// du/df and Prune.
func (f *Filesystem) prefix() string { return f.id + "-" }

// Prefix is prefix, exported for callers outside this package that
// need to scope their own operations to this filesystem's objects —
// e.g. task E4's MinIO Change notifications adapter, which scopes its
// server-side ListenBucketNotification subscription to exactly this.
func (f *Filesystem) Prefix() string { return f.prefix() }

// checkWritable is called at the top of every mutating operation. See
// ErrArchived and ARCHITECTURE.md's Multiple filesystems per bucket
// section: this is checked once per operation on an already-open
// Filesystem, not continuously against the master block — an Archive
// call made after Bootstrap resolved archived=false has no effect on
// this Filesystem until the next Bootstrap (e.g. the next mount).
func (f *Filesystem) checkWritable() error {
	if f.archived {
		return ErrArchived
	}
	return nil
}

// Bootstrap resolves this filesystem's ID and archived status against
// the master block (registering a new entry with the given declared
// size if this name has never been seen in this bucket before — size is
// ignored if the filesystem already exists, same as mode/uid/gid are
// ignored for an already-existing root, below), then ensures the root
// block exists, creating an empty one if this is a brand new filesystem.
func (f *Filesystem) Bootstrap(ctx context.Context, size uint64, mode, uid, gid uint32) error {
	id, archived, err := registerFilesystem(ctx, f.store, f.fsName, size)
	if err != nil {
		return err
	}
	f.id = id
	f.archived = archived

	rootKey := f.RootKey()
	if _, err := f.store.Head(ctx, rootKey); err == nil {
		return nil
	}
	empty, err := (&block.Block{Kind: block.Leaf}).Encode()
	if err != nil {
		return err
	}
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Mtime: time.Now()}
	_, err = f.store.Put(ctx, rootKey, bytes.NewReader(empty), dirMetadataFromAttr(attr), "")
	return err
}

// DiskUsage sums the size of every file under this filesystem's prefix —
// the "du" half of ARCHITECTURE.md's du/df design. Already free in the
// sense that it's just a prefix-scoped listing, same mechanism df's
// "Used" uses, not a tree walk fetching every file's content.
func (f *Filesystem) DiskUsage(ctx context.Context) (int64, error) {
	objs, err := f.store.ListByPrefix(ctx, f.prefix())
	if err != nil {
		return 0, err
	}
	var total int64
	for _, o := range objs {
		total += o.Size
	}
	return total, nil
}

// StatFS returns this filesystem's declared capacity ("Total") and
// current usage ("Used"), per ARCHITECTURE.md's du/df design: Total is
// the master block entry's declared size field, Used is the same
// prefix-scoped listing DiskUsage performs (so it includes every object
// this filesystem owns, root and .metadata objects included, not just
// file content — matching what Prune would actually delete).
func (f *Filesystem) StatFS(ctx context.Context) (total, used uint64, err error) {
	mb, _, err := readMaster(ctx, f.store)
	if err != nil {
		return 0, 0, mapNotFound(err)
	}
	found := false
	for _, e := range mb.Filesystems {
		if e.Name == f.fsName {
			total = e.Size
			found = true
			break
		}
	}
	if !found {
		return 0, 0, ErrNotFound
	}
	usedSigned, err := f.DiskUsage(ctx)
	if err != nil {
		return 0, 0, err
	}
	return total, uint64(usedSigned), nil
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

// dirAttrFromObject assembles a directory's Attr from its own block
// object's native metadata (see Attr's doc comment).
func dirAttrFromObject(key string, obj *objstore.Object) Attr {
	mtime := parseMetaTime(obj.Metadata, dirMetaMtime)
	if mtime.IsZero() {
		mtime = obj.LastModified // fallback for objects written before mtime tracking existed
	}
	return Attr{
		Mode:  parseMetaUint(obj.Metadata, dirMetaMode),
		Uid:   parseMetaUint(obj.Metadata, dirMetaUid),
		Gid:   parseMetaUint(obj.Metadata, dirMetaGid),
		Nlink: parseMetaUint(obj.Metadata, dirMetaNlink),
		Size:  obj.Size,
		Mtime: mtime,
		Ctime: obj.LastModified,
		Btime: Btime(key),
	}
}

// combineFileAttr assembles a file/symlink's Attr from its two parts,
// already fetched by the caller: metaData is .metadata's raw body,
// metaLastModified is .metadata's own version timestamp, and contentObj
// is the content object's Head/Get result.
func combineFileAttr(key string, metaData []byte, metaLastModified time.Time, contentObj *objstore.Object) (Attr, error) {
	attr, err := decodeFileMetadata(metaData)
	if err != nil {
		return Attr{}, err
	}
	ctime := metaLastModified
	if contentObj.LastModified.After(ctime) {
		ctime = contentObj.LastModified
	}
	attr.Size = contentObj.Size
	attr.Mtime = contentObj.LastModified
	attr.Ctime = ctime
	attr.Btime = Btime(key)
	return attr, nil
}

// statFile fetches a file/symlink's two objects concurrently — per
// ARCHITECTURE.md, this is two requests either way, so the latency cost
// is max() of the two, not the sum.
func (f *Filesystem) statFile(ctx context.Context, key string) (Attr, error) {
	var metaData []byte
	var metaLastModified time.Time
	var contentObj *objstore.Object
	var metaErr, contentErr error

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		body, obj, err := f.store.Get(ctx, metadataObjectKey(key))
		if err != nil {
			metaErr = mapNotFound(err)
			return
		}
		defer body.Close()
		metaData, metaErr = io.ReadAll(body)
		metaLastModified = obj.LastModified
	}()
	go func() {
		defer wg.Done()
		contentObj, contentErr = f.store.Head(ctx, key)
		contentErr = mapNotFound(contentErr)
	}()
	wg.Wait()

	if metaErr != nil {
		return Attr{}, metaErr
	}
	if contentErr != nil {
		return Attr{}, contentErr
	}
	return combineFileAttr(key, metaData, metaLastModified, contentObj)
}

// Stat returns the attributes of the node identified by key (a UUID, or
// this filesystem's root key). typ must be the node's actual type — a
// directory's attributes live in a completely different place than a
// file/symlink's (see Attr's doc comment), so the caller must already
// know which it's asking about.
func (f *Filesystem) Stat(ctx context.Context, key string, typ EntryType) (Attr, error) {
	if typ == TypeDir {
		obj, err := f.store.Head(ctx, key)
		if err != nil {
			return Attr{}, mapNotFound(err)
		}
		return dirAttrFromObject(key, obj), nil
	}
	return f.statFile(ctx, key)
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
		leftUUID, err := f.newKey()
		if err != nil {
			return err
		}
		rightUUID, err := f.newKey()
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
	rightUUID, err := f.newKey()
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
	attr, err := f.Stat(ctx, entry.UUID, entry.Type)
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
// Directories keep attributes as native object metadata on their own
// block object — see Attr's doc comment — unaffected by the .metadata
// split that applies to files/symlinks below.
func (f *Filesystem) Mkdir(ctx context.Context, dirKey, name string, mode, uid, gid uint32) (string, Attr, error) {
	if err := f.checkWritable(); err != nil {
		return "", Attr{}, err
	}
	newUUID, err := f.newKey()
	if err != nil {
		return "", Attr{}, err
	}
	empty, err := (&block.Block{Kind: block.Leaf}).Encode()
	if err != nil {
		return "", Attr{}, err
	}
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Mtime: time.Now()}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader(empty), dirMetadataFromAttr(attr), ""); err != nil {
		return "", Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, newUUID, TypeDir); err != nil {
		_ = f.store.Delete(ctx, newUUID, "") // best-effort cleanup of the orphaned block
		return "", Attr{}, err
	}
	attr.Btime = Btime(newUUID)
	return newUUID, attr, nil
}

// Create makes a new, empty regular file named name inside dirKey. The
// content object carries no attribute metadata at all — mode/uid/gid/
// nlink live entirely in the new metadataObjectKey side object.
// Create's third return value is the new (empty) content object's
// ETag — see ReadFile/WriteFile's doc comments on why callers need it
// (seeding an OpenFile without a redundant read-back of what was just
// written).
func (f *Filesystem) Create(ctx context.Context, dirKey, name string, mode, uid, gid uint32) (string, Attr, string, error) {
	if err := f.checkWritable(); err != nil {
		return "", Attr{}, "", err
	}
	newUUID, err := f.newKey()
	if err != nil {
		return "", Attr{}, "", err
	}
	contentObj, err := f.store.Put(ctx, newUUID, bytes.NewReader(nil), nil, "")
	if err != nil {
		return "", Attr{}, "", err
	}

	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1}
	metaData, err := encodeFileMetadata(attr)
	if err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, "", err
	}
	metaObj, err := f.store.Put(ctx, metadataObjectKey(newUUID), bytes.NewReader(metaData), nil, "")
	if err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, "", err
	}

	if err := f.insertEntry(ctx, dirKey, name, newUUID, TypeFile); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		_ = f.store.Delete(ctx, metadataObjectKey(newUUID), "")
		return "", Attr{}, "", err
	}
	attr.Btime = Btime(newUUID)
	attr.Mtime = contentObj.LastModified
	attr.Ctime = metaObj.LastModified
	return newUUID, attr, contentObj.ETag, nil
}

// Symlink creates a new symlink named name inside dirKey, pointing at
// target. The target string is stored as the object's content; mode/
// uid/gid/nlink live in metadataObjectKey, same as for a regular file.
func (f *Filesystem) Symlink(ctx context.Context, dirKey, name, target string, uid, gid uint32) (string, Attr, error) {
	if err := f.checkWritable(); err != nil {
		return "", Attr{}, err
	}
	newUUID, err := f.newKey()
	if err != nil {
		return "", Attr{}, err
	}
	contentObj, err := f.store.Put(ctx, newUUID, bytes.NewReader([]byte(target)), nil, "")
	if err != nil {
		return "", Attr{}, err
	}

	attr := Attr{Mode: 0777, Uid: uid, Gid: gid, Nlink: 1}
	metaData, err := encodeFileMetadata(attr)
	if err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, err
	}
	metaObj, err := f.store.Put(ctx, metadataObjectKey(newUUID), bytes.NewReader(metaData), nil, "")
	if err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, newUUID, TypeSymlink); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		_ = f.store.Delete(ctx, metadataObjectKey(newUUID), "")
		return "", Attr{}, err
	}
	attr.Size = int64(len(target))
	attr.Btime = Btime(newUUID)
	attr.Mtime = contentObj.LastModified
	attr.Ctime = metaObj.LastModified
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

const nlinkZero = 0

// adjustNlink changes uuid's hard link count by delta via a CAS read-
// modify-write of metadataObjectKey. If the count reaches zero, it
// writes nlink=0 under the same CAS check (so a concurrent Link racing
// to increment at the same moment necessarily loses the race at this Put
// and retries against the fresh state, never silently resurrecting a
// target this call just decided to delete), then deletes both the
// metadata object and the content object.
func (f *Filesystem) adjustNlink(ctx context.Context, uuid string, delta int) error {
	mk := metadataObjectKey(uuid)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		body, obj, err := f.store.Get(ctx, mk)
		if err != nil {
			return mapNotFound(err)
		}
		data, readErr := io.ReadAll(body)
		body.Close()
		if readErr != nil {
			return readErr
		}
		attr, err := decodeFileMetadata(data)
		if err != nil {
			return err
		}
		if attr.Nlink == nlinkZero {
			// Already tombstoned by a prior/concurrent Unlink; nothing to
			// resurrect, and nothing further for a decrement to do either.
			return ErrNotFound
		}
		newNlink := int(attr.Nlink) + delta

		if newNlink <= 0 {
			attr.Nlink = nlinkZero
			tomb, err := encodeFileMetadata(attr)
			if err != nil {
				return err
			}
			_, err = f.store.Put(ctx, mk, bytes.NewReader(tomb), nil, obj.ETag)
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
			_ = f.store.Delete(ctx, mk, "")
			_ = f.store.Delete(ctx, uuid, "")
			return nil
		}

		attr.Nlink = uint32(newNlink)
		updated, err := encodeFileMetadata(attr)
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, mk, bytes.NewReader(updated), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("adjust nlink %s: exceeded %d retries", uuid, maxTreeRetries)
}

// updateFileMetadata applies mutate to a file/symlink's metadataObjectKey
// under CAS, retrying on a lost race. This is what makes chmod/chown on a
// file real-CAS-protected rather than last-writer-wins: consolidating
// mode/uid/gid/nlink into one object (rather than leaving mode/uid/gid on
// native metadata, as the pre-.metadata design did) means a concurrent
// chmod and a concurrent Link now correctly serialize against each other
// through the same mechanism, as a side effect of the consolidation.
func (f *Filesystem) updateFileMetadata(ctx context.Context, uuid string, mutate func(*Attr)) (Attr, error) {
	mk := metadataObjectKey(uuid)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		body, obj, err := f.store.Get(ctx, mk)
		if err != nil {
			return Attr{}, mapNotFound(err)
		}
		data, readErr := io.ReadAll(body)
		body.Close()
		if readErr != nil {
			return Attr{}, readErr
		}
		attr, err := decodeFileMetadata(data)
		if err != nil {
			return Attr{}, err
		}
		mutate(&attr)
		newData, err := encodeFileMetadata(attr)
		if err != nil {
			return Attr{}, err
		}
		_, err = f.store.Put(ctx, mk, bytes.NewReader(newData), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		if err != nil {
			return Attr{}, err
		}
		return f.statFile(ctx, uuid)
	}
	return Attr{}, fmt.Errorf("update metadata %s: exceeded %d retries", uuid, maxTreeRetries)
}

// Link creates a hard link named name inside dirKey, pointing at the
// existing node targetUUID/targetType, incrementing its nlink via the
// CAS-protected metadataObjectKey object.
func (f *Filesystem) Link(ctx context.Context, dirKey, name, targetUUID string, targetType EntryType) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	if err := f.adjustNlink(ctx, targetUUID, +1); err != nil {
		return Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, targetUUID, targetType); err != nil {
		_ = f.adjustNlink(ctx, targetUUID, -1) // best-effort rollback
		return Attr{}, err
	}
	return f.Stat(ctx, targetUUID, targetType)
}

// Unlink removes a non-directory entry named name from dirKey,
// decrementing the target's nlink via the CAS-protected metadataObjectKey
// object and deleting its blob once the count reaches zero.
func (f *Filesystem) Unlink(ctx context.Context, dirKey, name string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
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
	if err := f.checkWritable(); err != nil {
		return err
	}
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

// ReadFile returns a regular file's whole content and its attributes,
// fetching the content and metadataObjectKey objects concurrently.
// ReadFile also returns the content object's current ETag, so a caller
// about to edit-then-write-back (see OpenFile) can condition that write
// on not having changed since this read — see ARCHITECTURE.md's Locking
// section and ROADMAP.md's task B1.
func (f *Filesystem) ReadFile(ctx context.Context, key string) ([]byte, Attr, string, error) {
	var data []byte
	var metaData []byte
	var metaLastModified time.Time
	var contentObj *objstore.Object
	var contentErr, metaErr error

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		body, obj, err := f.store.Get(ctx, key)
		if err != nil {
			contentErr = mapNotFound(err)
			return
		}
		defer body.Close()
		data, contentErr = io.ReadAll(body)
		contentObj = obj
	}()
	go func() {
		defer wg.Done()
		body, obj, err := f.store.Get(ctx, metadataObjectKey(key))
		if err != nil {
			metaErr = mapNotFound(err)
			return
		}
		defer body.Close()
		metaData, metaErr = io.ReadAll(body)
		metaLastModified = obj.LastModified
	}()
	wg.Wait()

	if contentErr != nil {
		return nil, Attr{}, "", contentErr
	}
	if metaErr != nil {
		return nil, Attr{}, "", metaErr
	}
	attr, err := combineFileAttr(key, metaData, metaLastModified, contentObj)
	if err != nil {
		return nil, Attr{}, "", err
	}
	return data, attr, contentObj.ETag, nil
}

// WriteFile replaces a regular file's whole content. The content object
// carries no attribute metadata — mode/uid/gid/nlink live in
// metadataObjectKey and are untouched by a content write, which is what
// makes the content object's own LastModified a correct, uncorrupted
// mtime (see Attr's doc comment).
//
// ifMatch conditions the write on the content object's current ETag,
// exactly like objstore.Store.Put's own ifMatch — empty means
// unconditional. Returns the new ETag on success, so a caller doing a
// sequence of writes (see OpenFile) can condition the next one on this
// one without a redundant read-back.
func (f *Filesystem) WriteFile(ctx context.Context, key string, data []byte, ifMatch string) (Attr, string, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, "", err
	}
	contentObj, err := f.store.Put(ctx, key, bytes.NewReader(data), nil, ifMatch)
	if err != nil {
		return Attr{}, "", err
	}
	attr, err := f.statFile(ctx, key)
	if err != nil {
		return Attr{}, "", err
	}
	return attr, contentObj.ETag, nil
}

// SetAttr applies the given field changes (any of which may be nil/unset)
// to the node at key. typ must be the node's actual type (see Stat).
//
// For a directory, mode/uid/gid is a metadata-only native-object-metadata
// update (not CAS-protected — "last writer wins" is an accepted
// simplification for directories, see Attr's doc comment); size is
// ignored, since truncating a directory block doesn't mean anything.
//
// For a file or symlink, a size change rewrites content via WriteFile
// (which bumps mtime, correctly, since it's a real content change); a
// mode/uid/gid change goes through updateFileMetadata, which — unlike
// the directory case — *is* real-CAS-protected, a quiet benefit of the
// .metadata consolidation.
func (f *Filesystem) SetAttr(ctx context.Context, key string, typ EntryType, mode, uid, gid *uint32, size *int64) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	if typ == TypeDir {
		attr, err := f.Stat(ctx, key, TypeDir)
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
		obj, err := f.store.UpdateMetadata(ctx, key, dirMetadataFromAttr(attr), "")
		if err != nil {
			return Attr{}, err
		}
		return dirAttrFromObject(key, obj), nil
	}

	if size != nil {
		data, _, _, err := f.ReadFile(ctx, key)
		if err != nil {
			return Attr{}, err
		}
		if *size != int64(len(data)) {
			resized := make([]byte, *size)
			copy(resized, data)
			// Unconditional: ftruncate's "last writer wins" is an
			// accepted simplification here, same as directory SetAttr
			// above — task B1's CAS protection is for OpenFile's
			// buffered-write path (the common case of a process
			// actually editing a file), not every call that happens to
			// replace a file's content.
			if _, _, err := f.WriteFile(ctx, key, resized, ""); err != nil {
				return Attr{}, err
			}
		}
	}

	if mode != nil || uid != nil || gid != nil {
		return f.updateFileMetadata(ctx, key, func(a *Attr) {
			if mode != nil {
				a.Mode = *mode
			}
			if uid != nil {
				a.Uid = *uid
			}
			if gid != nil {
				a.Gid = *gid
			}
		})
	}

	return f.Stat(ctx, key, typ)
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
