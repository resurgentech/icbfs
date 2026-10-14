package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/notify"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// maxTreeRetries bounds the whole-operation retry loop for the tree-aware
// insert/remove helpers and the file-metadata CAS helpers: on a lost race
// (a concurrent writer touching the same node), the whole top-down
// operation is retried from scratch rather than retried node-by-node. See
// insertEntry/removeEntry/adjustNlink/updateFileMetadata.
const maxTreeRetries = 20

// maxEntriesPerBlock/maxChildrenPerBlock are deliberately small so tests
// can exercise splitting without creating thousands of files. Real
// tuning (per docs/architecture.md: around mutation cost, not object size) is
// a separate, later concern.
const (
	maxEntriesPerBlock  = 8
	maxChildrenPerBlock = 8
)

// DefaultMaxFileSize is the largest file a filesystem holds unless it
// was created with another limit (SetCreationMaxFileSize): 1 TiB. Writes
// and truncates past a filesystem's limit fail with ErrFileTooLarge.
//
// Content is staged in a local scratch file, not memory, so the limit
// isn't about RAM (an earlier, in-memory design was OOM-killed by
// pjdfstest's 2 GiB writes and capped at 2 GiB for it); it bounds what a
// machine with the file open needs in local disk, and what one upload
// carries.
const DefaultMaxFileSize = 1 << 40

var errRetry = errors.New("icbfs: lost a race, retry the whole operation")

// Filesystem is the core, access-layer-independent filesystem logic: it
// turns object-store primitives plus the directory block format into
// lookup/mkdir/create/read/write/link/etc. operations.
//
// id and archived are only known after Bootstrap resolves them against
// the master block (see docs/architecture.md's master block section) — until then, a Filesystem returned by New isn't
// usable for anything that touches the store.
type Filesystem struct {
	store          objstore.Store
	fsName         string
	id             string // 4-hex-digit prefix, set by Bootstrap
	archived       bool   // set by Bootstrap; see ErrArchived
	readOnly       bool   // set by BootstrapReadOnly; see ErrReadOnly
	primaryWindows bool   // set by Bootstrap from the stored master-block value; see PrimaryWindows
	maxFileSize    int64  // set by Bootstrap from the stored master-block value; see MaxFileSize
	newMaxFileSize uint64 // the max file size a filesystem gets if Bootstrap creates it; see SetCreationMaxFileSize
	lockingEnabled bool   // off by default; see EnableLocking

	// lockNotify is nil unless EnableChangeNotifications was
	// called — read concurrently by every in-flight AcquireLockRange
	// call (never written after setup), so it's set once, before
	// concurrent use begins, same usage contract as lockingEnabled.
	lockNotify <-chan notify.Signal

	// cache is the shared, LRU-bounded local disk cache every OpenFile
	// session seeds its own private scratch file from — set by New's own default, overridable via
	// SetCacheOptions before Bootstrap.
	cache *diskCache

	// opens is this process's open files; see openSessions.
	openMu sync.Mutex
	opens  openSessions

	// usage remembers DiskUsage's answer on a read-only filesystem.
	usageMu    sync.Mutex
	usage      int64
	usageKnown bool
}

// New builds a Filesystem for the named filesystem. Call Bootstrap
// before using it for anything else — New itself does no I/O and
// doesn't know the filesystem's ID yet.
func New(store objstore.Store, fsName string) *Filesystem {
	// A real, non-optional default —
	// unlike Locking/Change-notifications, every OpenFile session goes
	// through this, so there's no sensible "off" state to default to.
	// os.TempDir() rather than a fixed path: multiple Filesystem
	// instances in the same process (common in this project's own test
	// suite) each get their own cache directory, no collisions.
	cacheDir, err := os.MkdirTemp("", "icbfs-cache-")
	if err != nil {
		// Astronomically unlikely on any real system (a writable temp
		// directory is assumed throughout this project, e.g. every
		// test already relies on one) — fall back to a fixed
		// subdirectory of the temp root rather than making New
		// fallible over this.
		cacheDir = filepath.Join(os.TempDir(), "icbfs-cache-fallback")
	}
	return &Filesystem{store: store, fsName: fsName, cache: newDiskCache(cacheDir, defaultCacheMaxBytes)}
}

// SetCacheOptions overrides the local disk cache's directory and
// total size budget — cmd/icbfs's
// -cache-dir/-cache-size flags call this before Bootstrap; tests and
// other callers that don't call it at all get New's own default
// (a fresh os.TempDir() subdirectory, 512 MiB). Safe to call again
// later, but only takes effect for OpenFile sessions opened
// afterward — in-flight sessions keep using whatever cache they
// already started with.
func (f *Filesystem) SetCacheOptions(dir string, maxBytes int64) {
	f.cache = newDiskCache(dir, maxBytes)
}

// EnableLocking turns on the Locking feature for this
// Filesystem — off by default. Unlike archived/size, this is a
// mount-time, local, in-memory choice, not a stored master-block
// property: per docs/architecture.md's Locking section, Locking has real
// costs (an extra .lock object, and B5's escalation using it) "most
// mounts won't use," so it's opt-in per mount, not a shared,
// persisted-per-filesystem setting every other mount of the same
// filesystem is forced into. Call this any time after New, before
// relying on locking being available — there's no ordering requirement
// relative to Bootstrap.
func (f *Filesystem) EnableLocking(enabled bool) {
	f.lockingEnabled = enabled
}

// EnableChangeNotifications wires this Filesystem's blocking lock
// acquisition (AcquireLockRange) to accelerate via Change
// notifications: a matching signal on signals makes a
// waiting acquire retry immediately instead of waiting out its
// current backoff interval. nil (the default, if this is never
// called) disables the accelerator entirely — AcquireLockRange falls
// back to pure polling; this is never a load-bearing dependency for Locking's own
// correctness, only a latency improvement when available.
//
// signals should be this Filesystem's own subscription (e.g. from
// notify.Broadcaster.Subscribe, if the mount also has Change
// notifications enabled — this is optional on top of two
// independently-optional features) to the same backend stream the FUSE
// watch dispatch observes; it must be an independent subscription, not the
// same single-consumption channel, or the two would steal signals
// from each other.
func (f *Filesystem) EnableChangeNotifications(signals <-chan notify.Signal) {
	f.lockNotify = signals
}

// PrimaryWindows reports whether this filesystem was created as
// primary-Windows (true) or primary-POSIX (false, the default) — see
// docs/architecture.md's "Windows compatibility: primary mode" section.
// Set once at creation, immutable
// afterward; only meaningful after Bootstrap.
func (f *Filesystem) PrimaryWindows() bool { return f.primaryWindows }

// partSize is the part size of a multipart content write (see
// OpenFile.uploadLocked): the smallest power-of-two MiB, at least 8 MiB,
// that fits a file of this filesystem's max size in objstore.MaxParts
// parts — 8 MiB for an 8 GiB limit, 128 MiB for the 1 TiB default. A
// file no bigger than one part is written with a single Put.
func (f *Filesystem) partSize() int64 {
	need := (f.maxFileSize + objstore.MaxParts - 1) / objstore.MaxParts
	p := int64(8 << 20)
	for p < need {
		p <<= 1
	}
	return p
}

// SetCreationMaxFileSize sets the largest file a new filesystem will
// hold, if Bootstrap is the one that creates it — like Bootstrap's size
// and primary mode, it's fixed at creation and ignored for a filesystem
// that already exists. 0, or never calling this, means
// DefaultMaxFileSize. Call before Bootstrap.
func (f *Filesystem) SetCreationMaxFileSize(n uint64) { f.newMaxFileSize = n }

// MaxFileSize is the largest file this filesystem holds, in bytes. Only
// meaningful after Bootstrap.
func (f *Filesystem) MaxFileSize() int64 { return f.maxFileSize }

// RootKey returns the block key identifying this filesystem's root:
// "<id>-root-<name>", a derived key, not stored anywhere separately —
// see docs/architecture.md. Only meaningful after Bootstrap.
// Store is the object store the filesystem is on (an as-of view, for a
// read-only past filesystem).
func (f *Filesystem) Store() objstore.Store { return f.store }

func (f *Filesystem) RootKey() string { return f.id + "-root-" + f.fsName }

// newKey mints a fresh, filesystem-scoped object key: this filesystem's
// ID prefix plus a new UUIDv7. Every object this filesystem creates
// (files, directories, their .metadata, future .lock objects) gets a key
// through this, never a bare NewUUIDv7() — that's what makes a single
// prefix-scoped listing capture this filesystem's entire footprint (see
// docs/architecture.md).
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
// e.g. the MinIO change-notifications adapter, which scopes its
// server-side ListenBucketNotification subscription to exactly this.
func (f *Filesystem) Prefix() string { return f.prefix() }

// checkWritable is called at the top of every mutating operation. See
// ErrArchived and docs/architecture.md's master block
// section: this is checked once per operation on an already-open
// Filesystem, not continuously against the master block — an Archive
// call made after Bootstrap resolved archived=false has no effect on
// this Filesystem until the next Bootstrap (e.g. the next mount).
func (f *Filesystem) checkWritable() error {
	if f.readOnly {
		return ErrReadOnly
	}
	if f.archived {
		return ErrArchived
	}
	return nil
}

// Bootstrap is create-or-attach in one call, for tests and tools that
// want a filesystem whatever state the bucket is in. Mounts never use it:
// they Attach, and filesystems are created explicitly (CreateFilesystem).
//
// Bootstrap resolves this filesystem's ID and archived status against
// the master block (registering a new entry with the given declared
// size if this name has never been seen in this bucket before — size is
// ignored if the filesystem already exists, same as mode/uid/gid are
// ignored for an already-existing root, below), then ensures the root
// block exists, creating an empty one if this is a brand new filesystem.
// primaryWindows is only consulted the first time f.fsName is created
// — same idempotency rule size/mode/uid/gid already follow — and is
// immutable afterward; see PrimaryWindows.
func (f *Filesystem) Bootstrap(ctx context.Context, size uint64, mode, uid, gid uint32, primaryWindows bool) error {
	id, entry, _, err := registerFilesystem(ctx, f.store, &pb.FilesystemEntry{
		Name: f.fsName, Size: size, PrimaryWindows: primaryWindows, MaxFileSize: f.newMaxFileSize,
		Created: timestamppb.Now()})
	if err != nil {
		return err
	}
	f.applyEntry(id, entry)
	if _, err := f.store.Head(ctx, f.RootKey()); err == nil {
		return nil
	}
	return f.createRoot(ctx, Attr{Mode: mode, Uid: uid, Gid: gid})
}

// applyEntry takes this filesystem's identity and fixed settings from
// its master-block entry.
func (f *Filesystem) applyEntry(id string, entry *pb.FilesystemEntry) {
	f.id = id
	f.archived = entry.Archived
	f.primaryWindows = entry.PrimaryWindows
	f.maxFileSize = DefaultMaxFileSize
	if entry.MaxFileSize != 0 && entry.MaxFileSize <= DefaultMaxFileSize {
		f.maxFileSize = int64(entry.MaxFileSize)
	}
}

// BootstrapReadOnly opens an existing filesystem without writing
// anything, for a store that refuses writes: an objstore.AsOf view of a
// past time. Every mutating operation then fails with ErrReadOnly, and
// reads don't record access times. It fails with ErrNotFound if the
// filesystem doesn't exist in the store — for an AsOf view, if it hadn't
// been created yet at that time.
func (f *Filesystem) BootstrapReadOnly(ctx context.Context) error {
	mb, _, err := readMaster(ctx, f.store)
	if err != nil {
		return fmt.Errorf("filesystem %q: %w", f.fsName, mapNotFound(err))
	}
	found := false
	for i, e := range mb.Filesystems {
		if e.Name == f.fsName {
			f.applyEntry(fsID(i), e)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("filesystem %q: %w", f.fsName, ErrNotFound)
	}
	if _, err := f.store.Head(ctx, f.RootKey()); err != nil {
		return fmt.Errorf("filesystem %q root: %w", f.fsName, mapNotFound(err))
	}
	f.readOnly = true
	return nil
}

// ReadOnly reports whether this filesystem was opened with
// BootstrapReadOnly.
func (f *Filesystem) ReadOnly() bool { return f.readOnly }

// DiskUsage sums the size of every file under this filesystem's prefix —
// the "du" half of docs/architecture.md's master block section. Already free in the
// sense that it's just a prefix-scoped listing, same mechanism df's
// "Used" uses, not a tree walk fetching every file's content.
//
// On a read-only filesystem (an as-of view of the past) the answer can't
// change, and computing it means listing the filesystem's whole version
// history, so it's computed once and remembered.
func (f *Filesystem) DiskUsage(ctx context.Context) (int64, error) {
	if f.readOnly {
		f.usageMu.Lock()
		defer f.usageMu.Unlock()
		if !f.usageKnown {
			u, err := f.diskUsage(ctx)
			if err != nil {
				return 0, err // not remembered: the next call tries again
			}
			f.usage, f.usageKnown = u, true
		}
		return f.usage, nil
	}
	return f.diskUsage(ctx)
}

func (f *Filesystem) diskUsage(ctx context.Context) (int64, error) {
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
// current usage ("Used"), per docs/architecture.md's master block section: Total is
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
	blk.Collation = f.collation()
	return blk, obj, nil
}

// collation is the order every directory in this filesystem keeps its
// entries in: case-insensitive first for primary-Windows filesystems, so
// LookupFold can seek instead of scanning, and plain byte order
// otherwise. It's fixed when the filesystem is created, like
// primaryWindows itself; existing directories are never re-sorted.
func (f *Filesystem) collation() block.Collation {
	if f.primaryWindows {
		return block.Folded
	}
	return block.Exact
}

// dirAttrFromObject assembles a directory's Attr from its block object's
// native metadata — the storage directories used before they had their
// own .metadata object (see Attr's doc comment). Only read for a
// directory created before that change; its first attribute write
// migrates it (updateDirMetadata).
func dirAttrFromObject(key string, obj *objstore.Object) Attr {
	mtime := parseMetaTime(obj.Metadata, dirMetaMtime)
	if mtime.IsZero() {
		mtime = obj.LastModified // fallback for objects written before mtime tracking existed
	}
	var xattrs map[string][]byte
	if v, ok := obj.Metadata[dirMetaWinAttr]; ok {
		if bits, err := strconv.ParseUint(v, 10, 32); err == nil {
			xattrs = map[string][]byte{WindowsAttributesXattrKey: {byte(uint32(bits) & windowsAttrMask)}}
		}
	}
	return Attr{
		Xattrs: xattrs,
		Mode:   parseMetaUint(obj.Metadata, dirMetaMode),
		Uid:    parseMetaUint(obj.Metadata, dirMetaUid),
		Gid:    parseMetaUint(obj.Metadata, dirMetaGid),
		Nlink:  parseMetaUint(obj.Metadata, dirMetaNlink),
		Size:   obj.Size,
		Atime:  parseMetaTime(obj.Metadata, dirMetaAtime), // zero if never read (no fallback — unlike mtime, "never accessed" is a real, honest value here)
		Mtime:  mtime,
		Ctime:  obj.LastModified,
		Btime:  Btime(key),
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
	// ctime is the stored status-change time (see the ctime field in
	// metadata.proto), which a read's atime update deliberately leaves
	// alone, so .metadata's own LastModified is only the fallback for
	// objects written before it existed.
	ctime := attr.Ctime
	if ctime.IsZero() {
		ctime = metaLastModified
	}
	attr.Size = contentObj.Size
	// attr.Mtime, from decodeFileMetadata above, is only ever an
	// explicit override (SetAttr's mtime parameter) — real content
	// writes never go through it. No override means the zero value,
	// so the real, content-derived mtime is the right default; an
	// override, once set, stays authoritative until SetAttr clears or
	// replaces it (see SetAttr's own doc comment on why a later real
	// write doesn't automatically invalidate it).
	// attr.Btime from decodeFileMetadata is only an explicit override;
	// otherwise the real creation time is embedded in the UUIDv7 key.
	keyBtime := Btime(key)
	if attr.Btime.IsZero() {
		attr.Btime = keyBtime
	}
	// An explicit override only holds while the content is still the
	// version it was set against; any real write since has changed the
	// ETag, and mtime follows the content again.
	if !attr.Mtime.IsZero() && attr.mtimeOverrideETag != contentObj.ETag {
		attr.Mtime = time.Time{}
	}
	attr.mtimeOverrideETag = ""
	if attr.Mtime.IsZero() {
		attr.Mtime = contentObj.LastModified
		// S3's Last-Modified only has 1-second resolution and is floored,
		// so a write moments after creation can report an mtime earlier
		// than the key's creation time (millisecond precision). Real
		// content can't be modified before it was created, so that's the
		// better lower bound. Only applies to the derived mtime — an
		// explicit override is authoritative even when it's older — and
		// uses the key's time, not a Btime override, since this corrects
		// a storage artifact, not anything a caller asserted.
		if keyBtime.After(attr.Mtime) {
			attr.Mtime = keyBtime
		}
	}
	// A content write is a status change too, and it doesn't touch
	// .metadata: the content's own write time counts (floored the same
	// way as the derived mtime above).
	written := contentObj.LastModified
	if keyBtime.After(written) {
		written = keyBtime
	}
	if written.After(ctime) {
		ctime = written
	}
	attr.Ctime = ctime
	return attr, nil
}

// combineDirAttr assembles a directory's Attr from its .metadata body and
// its root block object. Unlike a file's, a directory's mtime is always
// explicit (set at creation and by SetAttr), since its root block need
// not change when its entries do; the block's own timestamp is only a
// fallback.
func combineDirAttr(key string, metaData []byte, metaLastModified time.Time, blockObj *objstore.Object) (Attr, error) {
	attr, err := decodeFileMetadata(metaData)
	if err != nil {
		return Attr{}, err
	}
	attr.mtimeOverrideETag = ""
	attr.Size = blockObj.Size
	if attr.Mtime.IsZero() {
		attr.Mtime = blockObj.LastModified
	}
	if attr.Btime.IsZero() {
		attr.Btime = Btime(key)
	}
	// Stored ctime as for a file (see combineFileAttr); entry changes
	// set it explicitly (touchDir), since in a multi-level directory they
	// needn't rewrite the root block.
	if attr.Ctime.IsZero() {
		attr.Ctime = metaLastModified
	}
	if blockObj.LastModified.After(attr.Ctime) {
		attr.Ctime = blockObj.LastModified
	}
	return attr, nil
}

// statDir fetches a directory's root block and .metadata concurrently, as
// statFile does for a file; a directory without .metadata (created before
// directories had one) falls back to its block's native metadata.
func (f *Filesystem) statDir(ctx context.Context, key string) (Attr, error) {
	var metaData []byte
	var metaLastModified time.Time
	var blockObj *objstore.Object
	var metaErr, blockErr error

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
		blockObj, blockErr = f.store.Head(ctx, key)
		blockErr = mapNotFound(blockErr)
	}()
	wg.Wait()

	if blockErr != nil {
		return Attr{}, blockErr
	}
	if errors.Is(metaErr, ErrNotFound) {
		return dirAttrFromObject(key, blockObj), nil
	}
	if metaErr != nil {
		return Attr{}, metaErr
	}
	return combineDirAttr(key, metaData, metaLastModified, blockObj)
}

// statFile fetches a file/symlink's two objects concurrently — per
// docs/architecture.md, this is two requests either way, so the latency cost
// is max() of the two, not the sum.
func (f *Filesystem) statFile(ctx context.Context, key string) (Attr, error) {
	if isStreamKey(key) {
		return f.statStream(ctx, key)
	}
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
		return f.statDir(ctx, key)
	}
	a, err := f.statFile(ctx, key)
	if err != nil {
		return Attr{}, err
	}
	return f.localOverlay(key, a), nil
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

// findFoldInTree finds name case-insensitively in the Folded shard tree
// rooted at rootKey: the entry named exactly name if there is one,
// otherwise the first (in sorted order) whose name matches it
// case-insensitively.
func (f *Filesystem) findFoldInTree(ctx context.Context, rootKey, name string) (block.Entry, bool, error) {
	var match block.Entry
	found := false
	_, err := f.scanFold(ctx, rootKey, name, func(e block.Entry) bool {
		if !found || e.Name == name {
			match, found = e, true
		}
		return e.Name == name
	})
	return match, found, err
}

// scanFold visits, in sorted order, every entry of the Folded shard tree
// at key whose name matches name case-insensitively, until visit returns
// true. Those entries are one contiguous run, so this reads only the
// blocks on the way down to where the run starts, plus any the run
// spills into. It returns true once the run is known to be over, so a
// caller iterating sibling subtrees stops there.
func (f *Filesystem) scanFold(ctx context.Context, key, name string, visit func(block.Entry) bool) (bool, error) {
	blk, _, err := f.readBlock(ctx, key)
	if err != nil {
		return false, err
	}
	if blk.Kind == block.Leaf {
		for _, e := range blk.Entries[blk.SeekFold(name):] {
			if block.CompareFold(e.Name, name) != 0 || visit(e) {
				return true, nil
			}
		}
		return false, nil
	}
	start := blk.ChildIndexFold(name)
	for i := start; i < len(blk.Children); i++ {
		// A later child's MinKey bounds everything in it from below: once
		// that sorts past name, so does the rest of the directory.
		if i > start && block.CompareFold(blk.Children[i].MinKey, name) > 0 {
			return true, nil
		}
		done, err := f.scanFold(ctx, blk.Children[i].UUID, name, visit)
		if err != nil || done {
			return done, err
		}
	}
	return false, nil
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
		newRoot := &block.Block{Kind: block.Internal, Collation: blk.Collation, Children: []block.Child{
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
	if err := f.checkNameLength(name); err != nil {
		return block.Entry{}, Attr{}, err
	}
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

// LookupFold is Lookup, matching name case-insensitively (as NTFS does;
// see block.CompareFold): the entry named exactly name when there is
// one, otherwise one whose name differs only in case. Storage stays
// case-preserving either way; this is only how a case-insensitive mount
// finds things.
//
// On a primary-Windows filesystem directories are sorted
// case-insensitively, so this costs one descent, the same as Lookup. On
// a primary-POSIX filesystem a name with no exact match means reading
// the whole directory.
func (f *Filesystem) LookupFold(ctx context.Context, dirKey, name string) (block.Entry, Attr, error) {
	if err := f.checkNameLength(name); err != nil {
		return block.Entry{}, Attr{}, err
	}
	var entry block.Entry
	var ok bool
	var err error
	if f.collation() == block.Folded {
		entry, ok, err = f.findFoldInTree(ctx, dirKey, name)
	} else {
		entry, ok, err = f.findInTree(ctx, dirKey, name)
		if err == nil && !ok {
			var entries []block.Entry
			entries, err = f.collectEntries(ctx, dirKey)
			for _, e := range entries {
				if block.EqualFold(e.Name, name) {
					entry, ok = e, true
					break
				}
			}
		}
	}
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

// Mkdir creates a new, empty directory named name inside dirKey: its
// .metadata object, then its (empty) root block, then the entry in the
// parent — same order and cleanup as Create.
func (f *Filesystem) Mkdir(ctx context.Context, dirKey, name string, mode, uid, gid uint32) (string, Attr, error) {
	if err := f.checkWritable(); err != nil {
		return "", Attr{}, err
	}
	if err := f.checkName(name); err != nil {
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
	now := time.Now()
	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Mtime: now, Ctime: now}
	metaData, err := encodeFileMetadata(attr)
	if err != nil {
		return "", Attr{}, err
	}
	if _, err := f.store.Put(ctx, metadataObjectKey(newUUID), bytes.NewReader(metaData), nil, ""); err != nil {
		return "", Attr{}, err
	}
	if _, err := f.store.Put(ctx, newUUID, bytes.NewReader(empty), nil, ""); err != nil {
		_ = f.store.Delete(ctx, metadataObjectKey(newUUID), "")
		return "", Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, newUUID, TypeDir); err != nil {
		_ = f.store.Delete(ctx, newUUID, "") // best-effort cleanup of the orphaned objects
		_ = f.store.Delete(ctx, metadataObjectKey(newUUID), "")
		return "", Attr{}, err
	}
	f.touchDir(ctx, dirKey)
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
	if err := f.checkName(name); err != nil {
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
	f.touchDir(ctx, dirKey)
	attr.Btime = Btime(newUUID)
	attr.Mtime = contentObj.LastModified
	attr.Ctime = metaObj.LastModified
	return newUUID, attr, contentObj.ETag, nil
}

// Mknod creates a FIFO, Unix domain socket, or char/block device entry
// — typ must be TypeFifo, TypeSocket,
// TypeCharDevice, or TypeBlockDevice; rdev is only meaningful (and only
// stored) for the two device types.
//
// Deliberately built as a thin variant of Create, not a separate
// mechanism: none of these types have any real byte-stream content
// (a FIFO's "content" is a transient kernel pipe object; a device
// node's is the real device, serviced by the kernel's own char/block
// driver tables; a Unix socket's is the kernel's AF_UNIX
// implementation) — the FUSE driver's only real job for any of them
// is reporting the right type/mode bit via Lookup/Getattr. So this
// reuses exactly the same two-object shape Create already
// established (an empty, never-touched-again content object plus a
// .metadata object) purely so Stat/SetAttr/TouchAtime keep working
// unchanged for these types too, with no special-casing needed
// anywhere else in this package.
//
// No OpenFile/ETag returned, unlike Create — these types are never
// opened for byte-level read/write through this driver (go-fuse's own
// NodeMknoder has no file-handle return either, for the same reason:
// the kernel intercepts real I/O against a special file before it
// ever reaches this filesystem's Open/Read/Write).
func (f *Filesystem) Mknod(ctx context.Context, dirKey, name string, mode uint32, typ EntryType, rdev, uid, gid uint32) (string, Attr, error) {
	if err := f.checkWritable(); err != nil {
		return "", Attr{}, err
	}
	if err := f.checkName(name); err != nil {
		return "", Attr{}, err
	}
	newUUID, err := f.newKey()
	if err != nil {
		return "", Attr{}, err
	}
	contentObj, err := f.store.Put(ctx, newUUID, bytes.NewReader(nil), nil, "")
	if err != nil {
		return "", Attr{}, err
	}

	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1}
	if typ == TypeCharDevice || typ == TypeBlockDevice {
		attr.Rdev = rdev
	}
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

	if err := f.insertEntry(ctx, dirKey, name, newUUID, typ); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		_ = f.store.Delete(ctx, metadataObjectKey(newUUID), "")
		return "", Attr{}, err
	}
	f.touchDir(ctx, dirKey)
	attr.Btime = Btime(newUUID)
	attr.Mtime = contentObj.LastModified
	attr.Ctime = metaObj.LastModified
	return newUUID, attr, nil
}

// Symlink creates a new symlink named name inside dirKey, pointing at
// target. The target string is stored as the object's content; mode/
// uid/gid/nlink live in metadataObjectKey, same as for a regular file.
func (f *Filesystem) Symlink(ctx context.Context, dirKey, name, target string, uid, gid uint32) (string, Attr, error) {
	if err := f.checkWritable(); err != nil {
		return "", Attr{}, err
	}
	if err := f.checkName(name); err != nil {
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
	f.touchDir(ctx, dirKey)
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
func (f *Filesystem) adjustNlink(ctx context.Context, uuid string, delta int) (deleted bool, err error) {
	mk := metadataObjectKey(uuid)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		body, obj, err := f.store.Get(ctx, mk)
		if err != nil {
			return false, mapNotFound(err)
		}
		data, readErr := io.ReadAll(body)
		body.Close()
		if readErr != nil {
			return false, readErr
		}
		attr, err := decodeFileMetadata(data)
		if err != nil {
			return false, err
		}
		if attr.Nlink == nlinkZero {
			// Already tombstoned by a prior/concurrent Unlink; nothing to
			// resurrect, and nothing further for a decrement to do either.
			return false, ErrNotFound
		}
		newNlink := int(attr.Nlink) + delta
		attr.Ctime = time.Now() // a link count change is a status change

		if newNlink <= 0 {
			attr.Nlink = nlinkZero
			tomb, err := encodeFileMetadata(attr)
			if err != nil {
				return false, err
			}
			_, err = f.store.Put(ctx, mk, bytes.NewReader(tomb), nil, obj.ETag)
			if objstore.IsPreconditionFailed(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			// We are the one who observed and serialized "really at
			// zero" via the CAS write above — no concurrent Link can
			// have raced past this point, since it would have observed
			// either our tombstone (and bailed, above) or lost its own
			// CAS race against it. Safe to physically delete now.
			f.deleteUnlessOpen(ctx, uuid, attr.Streams)
			return true, nil
		}

		attr.Nlink = uint32(newNlink)
		updated, err := encodeFileMetadata(attr)
		if err != nil {
			return false, err
		}
		_, err = f.store.Put(ctx, mk, bytes.NewReader(updated), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return false, err
	}
	return false, fmt.Errorf("adjust nlink %s: exceeded %d retries", uuid, maxTreeRetries)
}

// updateFileMetadata applies mutate to a file/symlink's metadataObjectKey
// under CAS, retrying on a lost race. This is what makes chmod/chown on a
// file real-CAS-protected rather than last-writer-wins: consolidating
// mode/uid/gid/nlink into one object (rather than leaving mode/uid/gid on
// native metadata, as the pre-.metadata design did) means a concurrent
// chmod and a concurrent Link now correctly serialize against each other
// through the same mechanism, as a side effect of the consolidation.
// Any change through it is a status change (see writeMetadata).
func (f *Filesystem) updateFileMetadata(ctx context.Context, uuid string, mutate func(*Attr)) (Attr, error) {
	if err := f.writeMetadata(ctx, uuid, TypeFile, true, mutate); err != nil {
		return Attr{}, err
	}
	return f.statFile(ctx, uuid)
}

// updateDirMetadata is updateFileMetadata for a directory.
func (f *Filesystem) updateDirMetadata(ctx context.Context, key string, mutate func(*Attr)) (Attr, error) {
	if err := f.writeMetadata(ctx, key, TypeDir, true, mutate); err != nil {
		return Attr{}, err
	}
	return f.statDir(ctx, key)
}

// updateMetadata is updateDirMetadata or updateFileMetadata, by type.
func (f *Filesystem) updateMetadata(ctx context.Context, key string, typ EntryType, mutate func(*Attr)) (Attr, error) {
	if typ == TypeDir {
		return f.updateDirMetadata(ctx, key, mutate)
	}
	return f.updateFileMetadata(ctx, key, mutate)
}

// writeMetadata applies mutate to key's .metadata under CAS, retrying on
// a lost race. statusChange stamps ctime with the time of the write;
// every change is one except a read's atime update (TouchAtime).
//
// A directory created before directories had .metadata is migrated
// first: its native block metadata becomes the initial .metadata body
// (created only if still absent, so a concurrent migration can't be
// overwritten).
func (f *Filesystem) writeMetadata(ctx context.Context, key string, typ EntryType, statusChange bool, mutate func(*Attr)) error {
	mk := metadataObjectKey(key)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		body, obj, err := f.store.Get(ctx, mk)
		if typ == TypeDir && objstore.IsNotFound(err) {
			blockObj, herr := f.store.Head(ctx, key)
			if herr != nil {
				return mapNotFound(herr)
			}
			legacy := dirAttrFromObject(key, blockObj)
			// Only stored fields go into .metadata; the rest are derived
			// at read time.
			initial := Attr{Mode: legacy.Mode, Uid: legacy.Uid, Gid: legacy.Gid, Nlink: legacy.Nlink,
				Atime: legacy.Atime, Mtime: legacy.Mtime, Ctime: legacy.Ctime, Xattrs: legacy.Xattrs}
			data, eerr := encodeFileMetadata(initial)
			if eerr != nil {
				return eerr
			}
			if _, perr := f.store.PutIfAbsent(ctx, mk, bytes.NewReader(data), nil); perr != nil && !objstore.IsPreconditionFailed(perr) {
				return perr
			}
			continue
		}
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
		mutate(&attr)
		if statusChange {
			attr.Ctime = time.Now()
		}
		newData, err := encodeFileMetadata(attr)
		if err != nil {
			return err
		}
		_, err = f.store.Put(ctx, mk, bytes.NewReader(newData), nil, obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("update metadata %s: exceeded %d retries", key, maxTreeRetries)
}

// touchDir records that an entry was added to or removed from the
// directory at key: its mtime and ctime become now. Best effort, like
// TouchAtime — by the time this runs the entry change itself has
// already happened, and failing that operation over a timestamp would
// misreport it.
func (f *Filesystem) touchDir(ctx context.Context, key string) {
	now := time.Now()
	_ = f.writeMetadata(ctx, key, TypeDir, true, func(a *Attr) {
		a.Mtime = now
	})
}

// touchCtime records a status change to the file or directory at key
// that changed nothing else in its .metadata (a rename). Best effort,
// like touchDir.
func (f *Filesystem) touchCtime(ctx context.Context, key string, typ EntryType) {
	if typ != TypeDir {
		typ = TypeFile
	}
	_ = f.writeMetadata(ctx, key, typ, true, func(*Attr) {})
}

// SetWindowsAttributes persists a file/symlink's Hidden/System/
// ReadOnly/Archive bits with full native fidelity, regardless of
// primary mode — docs/architecture.md's "the schema always carries the
// field" rule. Which representation (these stored bits, vs.
// deriving ReadOnly from the POSIX mode bit) is authoritative at
// *read* time is a decision for the caller, based on primary mode —
// not this method's concern, same division SetAttr already has
// between storing and interpreting mode/uid/gid.
func (f *Filesystem) SetWindowsAttributes(ctx context.Context, uuid string, typ EntryType, bits uint32) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateMetadata(ctx, uuid, typ, func(a *Attr) {
		if a.Xattrs == nil {
			a.Xattrs = make(map[string][]byte)
		}
		a.Xattrs[WindowsAttributesXattrKey] = []byte{byte(bits & windowsAttrMask)}
	})
}

// MarkWindowsDirSymlink records that symlink uuid is a Windows directory
// symlink (see WindowsDirSymlinkXattrKey).
func (f *Filesystem) MarkWindowsDirSymlink(ctx context.Context, uuid string) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateFileMetadata(ctx, uuid, func(a *Attr) {
		if a.Xattrs == nil {
			a.Xattrs = make(map[string][]byte)
		}
		a.Xattrs[WindowsDirSymlinkXattrKey] = []byte{1}
	})
}

// SetWindowsCreateMetadata stores a new file's or directory's Windows
// attribute bits and, when sddl is non-nil, its security descriptor, in
// one metadata write — what a Windows create needs, without a separate
// round trip for each.
func (f *Filesystem) SetWindowsCreateMetadata(ctx context.Context, uuid string, typ EntryType, bits uint32, sddl []byte) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateMetadata(ctx, uuid, typ, func(a *Attr) {
		if a.Xattrs == nil {
			a.Xattrs = make(map[string][]byte)
		}
		a.Xattrs[WindowsAttributesXattrKey] = []byte{byte(bits & windowsAttrMask)}
		if sddl != nil {
			a.Xattrs[WindowsACLXattrKey] = sddl
		}
	})
}

// SetWindowsACL persists a file's, symlink's or directory's native
// Windows security descriptor, given as its SDDL string representation
// (WindowsACLXattrKey's own doc comment explains the SDDL choice).
func (f *Filesystem) SetWindowsACL(ctx context.Context, uuid string, typ EntryType, sddl []byte) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateMetadata(ctx, uuid, typ, func(a *Attr) {
		if a.Xattrs == nil {
			a.Xattrs = make(map[string][]byte)
		}
		a.Xattrs[WindowsACLXattrKey] = sddl
	})
}

// SetXattr sets (or, with a nil value, removes) one extended attribute
// of a file, symlink or directory.
func (f *Filesystem) SetXattr(ctx context.Context, uuid string, typ EntryType, name string, value []byte) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateMetadata(ctx, uuid, typ, func(a *Attr) {
		if value == nil {
			delete(a.Xattrs, name)
			return
		}
		if a.Xattrs == nil {
			a.Xattrs = make(map[string][]byte)
		}
		a.Xattrs[name] = value
	})
}

// SetGenericReparseData persists a file's raw, non-symlink reparse
// point buffer byte-exact — see
// GenericReparseXattrKey's own doc comment for why this is stored
// opaquely, unlike the symlink case.
func (f *Filesystem) SetGenericReparseData(ctx context.Context, uuid string, buf []byte) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateFileMetadata(ctx, uuid, func(a *Attr) {
		if a.Xattrs == nil {
			a.Xattrs = make(map[string][]byte)
		}
		a.Xattrs[GenericReparseXattrKey] = buf
	})
}

// DeleteGenericReparseData removes a file's stored non-symlink
// reparse point buffer, if any — FSCTL_DELETE_REPARSE_POINT's real
// effect, reverting the file back to
// an ordinary one with no reparse point at all.
func (f *Filesystem) DeleteGenericReparseData(ctx context.Context, uuid string) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateFileMetadata(ctx, uuid, func(a *Attr) {
		delete(a.Xattrs, GenericReparseXattrKey)
	})
}

// Link creates a hard link named name inside dirKey, pointing at the
// existing node targetUUID/targetType, incrementing its nlink via the
// CAS-protected metadataObjectKey object.
func (f *Filesystem) Link(ctx context.Context, dirKey, name, targetUUID string, targetType EntryType) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	if err := f.checkName(name); err != nil {
		return Attr{}, err
	}
	if _, err := f.adjustNlink(ctx, targetUUID, +1); err != nil {
		return Attr{}, err
	}

	if err := f.insertEntry(ctx, dirKey, name, targetUUID, targetType); err != nil {
		_, _ = f.adjustNlink(ctx, targetUUID, -1) // best-effort rollback
		return Attr{}, err
	}
	f.touchDir(ctx, dirKey)
	return f.Stat(ctx, targetUUID, targetType)
}

// Unlink removes a non-directory entry named name from dirKey,
// decrementing the target's nlink via the CAS-protected metadataObjectKey
// object and deleting its blob once the count reaches zero.
func (f *Filesystem) Unlink(ctx context.Context, dirKey, name string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if err := f.checkNameLength(name); err != nil {
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

	before, _ := f.store.Head(ctx, entry.UUID) // for the trash entry, if this was the last name
	if _, err := f.removeEntry(ctx, dirKey, name); err != nil {
		return err
	}
	f.touchDir(ctx, dirKey)

	deleted, err := f.adjustNlink(ctx, entry.UUID, -1)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if deleted {
		var size int64
		var version string
		if before != nil {
			size, version = before.Size, before.VersionID
		}
		f.recordTrash(ctx, entry.UUID, entry.Type, dirKey, name, size, version)
	}
	return nil
}

// Rmdir removes an empty directory named name from dirKey.
func (f *Filesystem) Rmdir(ctx context.Context, dirKey, name string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if err := f.checkNameLength(name); err != nil {
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

	dirAttr, err := f.statDir(ctx, entry.UUID)
	if err != nil {
		return err
	}
	if _, err := f.removeEntry(ctx, dirKey, name); err != nil {
		return err
	}
	f.touchDir(ctx, dirKey)
	if err := f.store.Delete(ctx, entry.UUID, ""); err != nil {
		return err
	}
	f.deleteStreamObjects(ctx, entry.UUID, dirAttr.Streams)
	// Absent for a directory created before directories had .metadata.
	if err := f.store.Delete(ctx, metadataObjectKey(entry.UUID), ""); err != nil && !objstore.IsNotFound(err) {
		return err
	}
	f.recordTrash(ctx, entry.UUID, TypeDir, dirKey, name, 0, "")
	return nil
}

// dirContains reports whether ancestorUUID's own subtree includes
// targetUUID anywhere, ancestorUUID itself included — used by Rename
// to refuse moving a directory into one of its own descendants
// (POSIX rename(2)'s EINVAL: "newpath contains a path prefix of
// oldpath"). This tree has no parent pointers to walk upward from
// targetUUID instead, so the check walks downward from ancestorUUID —
// an accepted cost here, same as DiskUsage/Prune's own full recursive
// walks elsewhere in this file.
func (f *Filesystem) dirContains(ctx context.Context, ancestorUUID, targetUUID string) (bool, error) {
	if ancestorUUID == targetUUID {
		return true, nil
	}
	entries, err := f.collectEntries(ctx, ancestorUUID)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Type != TypeDir {
			continue
		}
		found, err := f.dirContains(ctx, e.UUID, targetUUID)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// Rename moves/renames the entry named oldName inside oldDirKey to
// newName inside newDirKey (same directory if oldDirKey == newDirKey),
// matching POSIX rename(2) semantics, here once for every access
// layer.
//
// replace mirrors POSIX's implicit-overwrite default (true) vs.
// RENAME_NOREPLACE (false, FUSE's NodeRenamer flags; WinFsp's Rename
// callback's own replaceIfExist parameter maps the same way) — see the
// existing-target handling below for the exact rules (matching
// rename(2): a file can't replace a non-empty directory or vice versa;
// an already-hardlinked-to-the-same-object source/target is always a
// no-op regardless of replace).
//
// Known simplification, same "last writer wins" tradeoff already
// accepted elsewhere in this file (e.g. SetAttr's ftruncate): the
// remove-then-insert pair below is not one atomic cross-directory
// transaction (each half is independently CAS-retried, not both
// together) — a concurrent reader could briefly observe the entry at
// neither location. A real, documented limitation, not silently
// assumed away; a distributed cross-directory transaction is out of
// scope for this project's design (see docs/architecture.md's Locking
// section for the general class of simplification this follows).
func (f *Filesystem) Rename(ctx context.Context, oldDirKey, oldName, newDirKey, newName string, replace bool) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if err := f.checkNameLength(oldName); err != nil {
		return err
	}
	if err := f.checkName(newName); err != nil {
		return err
	}
	if oldDirKey == newDirKey && oldName == newName {
		return nil
	}

	entry, ok, err := f.findInTree(ctx, oldDirKey, oldName)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}

	if entry.Type == TypeDir {
		contains, err := f.dirContains(ctx, entry.UUID, newDirKey)
		if err != nil {
			return err
		}
		if contains {
			return ErrInvalidRename
		}
	}

	existing, existingOK, err := f.findInTree(ctx, newDirKey, newName)
	if err != nil {
		return err
	}
	if existingOK {
		if existing.UUID == entry.UUID {
			// oldName and newName are two existing hardlinks to the
			// same object (or, within one directory, literally the
			// same entry) — rename(2) does nothing and succeeds,
			// regardless of replace.
			return nil
		}
		if !replace {
			return ErrExists
		}
		if existing.Type == TypeDir {
			if entry.Type != TypeDir {
				return ErrIsDir
			}
			children, err := f.collectEntries(ctx, existing.UUID)
			if err != nil {
				return err
			}
			if len(children) > 0 {
				return ErrNotEmpty
			}
			if _, err := f.removeEntry(ctx, newDirKey, newName); err != nil {
				return err
			}
			if err := f.store.Delete(ctx, existing.UUID, ""); err != nil {
				return err
			}
			if err := f.store.Delete(ctx, metadataObjectKey(existing.UUID), ""); err != nil && !objstore.IsNotFound(err) {
				return err
			}
			f.recordTrash(ctx, existing.UUID, TypeDir, newDirKey, newName, 0, "")
		} else {
			if entry.Type == TypeDir {
				return ErrNotDir
			}
			before, _ := f.store.Head(ctx, existing.UUID)
			if _, err := f.removeEntry(ctx, newDirKey, newName); err != nil {
				return err
			}
			deleted, err := f.adjustNlink(ctx, existing.UUID, -1)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if deleted {
				var size int64
				var version string
				if before != nil {
					size, version = before.Size, before.VersionID
				}
				f.recordTrash(ctx, existing.UUID, existing.Type, newDirKey, newName, size, version)
			}
		}
	}

	if _, err := f.removeEntry(ctx, oldDirKey, oldName); err != nil {
		return err
	}
	if err := f.insertEntry(ctx, newDirKey, newName, entry.UUID, entry.Type); err != nil {
		// Best-effort rollback: put the old entry back so a failed
		// rename doesn't silently lose the source name on top of
		// whatever already failed.
		_ = f.insertEntry(ctx, oldDirKey, oldName, entry.UUID, entry.Type)
		return err
	}
	f.touchDir(ctx, oldDirKey)
	if newDirKey != oldDirKey {
		f.touchDir(ctx, newDirKey)
	}
	f.touchCtime(ctx, entry.UUID, entry.Type) // as Linux filesystems do
	return nil
}

// ReadFile returns a regular file's whole content and its attributes,
// fetching the content and metadataObjectKey objects concurrently.
// ReadFile also returns the content object's current ETag, so a caller
// about to edit-then-write-back (see OpenFile) can condition that write
// on not having changed since this read — see docs/architecture.md's
// Concurrency section.
func (f *Filesystem) ReadFile(ctx context.Context, key string) ([]byte, Attr, string, error) {
	if isStreamKey(key) {
		body, obj, err := f.store.Get(ctx, key)
		if err != nil {
			return nil, Attr{}, "", mapNotFound(err)
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		if err != nil {
			return nil, Attr{}, "", err
		}
		return data, Attr{Nlink: 1, Size: obj.Size, Mtime: obj.LastModified, Ctime: obj.LastModified}, obj.ETag, nil
	}
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
	return f.writeFileFrom(ctx, key, bytes.NewReader(data), ifMatch)
}

// writeFileFrom is WriteFile's streaming counterpart: OpenFile.Flush passes a local scratch file's own
// *os.File directly (via io.NewSectionReader) rather than a fully
// materialized []byte, so a large file's flush never needs to hold
// its entire content in RAM at once — objstore.Store.Put already
// accepts io.Reader natively (it always has; whole-object buffering
// was never a constraint this package's own backend interface
// imposed, only something OpenFile's own earlier design chose to do
// above it).
func (f *Filesystem) writeFileFrom(ctx context.Context, key string, body io.Reader, ifMatch string) (Attr, string, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, "", err
	}
	contentObj, err := f.store.Put(ctx, key, body, nil, ifMatch)
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
// For a file or symlink, a size change rewrites content through an
// OpenFile session (which bumps mtime, correctly, since it's a real
// content change); a
// mode/uid/gid change goes through updateFileMetadata, which — unlike
// the directory case — *is* real-CAS-protected, a quiet benefit of the
// .metadata consolidation.
//
// mtime is an explicit override (Windows' SetFileTime, POSIX
// utimensat — a pure metadata call, no content write at all) — see
// combineFileAttr's own doc comment for a file/symlink, or
// combineDirAttr for a directory. For a file, the override is
// tied to the content version it was set against and lapses on the
// next real write, so mtime follows content again — at the cost of one
// Head request per explicit set, nothing on the write path.
//
// btime is an explicit creation-time override (Windows' SetFileTime
// CreationTime); unset, creation time is the one embedded in the UUIDv7
// key.
func (f *Filesystem) SetAttr(ctx context.Context, key string, typ EntryType, mode, uid, gid *uint32, size *int64, atime, mtime, btime *time.Time) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	// A negative size is invalid (truncate(2) says EINVAL); one past this
	// filesystem's limit is "file too large" (EFBIG) — pjdfstest probes
	// truncate(path, 999999999999999) and accepts either.
	if size != nil && *size < 0 {
		return Attr{}, ErrInvalidArgument
	}
	if size != nil && *size > f.maxFileSize {
		return Attr{}, ErrFileTooLarge
	}
	if typ == TypeDir {
		if mode == nil && uid == nil && gid == nil && atime == nil && mtime == nil && btime == nil {
			return f.statDir(ctx, key)
		}
		return f.updateDirMetadata(ctx, key, func(a *Attr) {
			if mode != nil {
				a.Mode = *mode
			}
			if uid != nil {
				a.Uid = *uid
			}
			if gid != nil {
				a.Gid = *gid
			}
			if atime != nil {
				a.Atime = *atime
			}
			if mtime != nil {
				a.Mtime = *mtime
			}
			if btime != nil {
				a.Btime = *btime
			}
		})
	}

	if size != nil {
		// Through an OpenFile session, so the content is staged on local
		// disk (a grow is a sparse extend there), and the write is the
		// same CAS-retried flush as any other edit.
		o, err := f.Open(ctx, key)
		if err != nil {
			return Attr{}, err
		}
		err = o.Truncate(*size)
		if err == nil {
			_, err = o.Flush(ctx)
		}
		o.Close()
		if err != nil {
			return Attr{}, err
		}
	}

	var contentETag string
	if mtime != nil {
		// Tie the override to the current content version — see
		// combineFileAttr.
		head, err := f.store.Head(ctx, key)
		if err != nil {
			return Attr{}, mapNotFound(err)
		}
		contentETag = head.ETag
	}
	if mode != nil || uid != nil || gid != nil || atime != nil || mtime != nil || btime != nil {
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
			if atime != nil {
				a.Atime = *atime
			}
			if mtime != nil {
				// a.Mtime, decoded fresh from this same object by
				// updateFileMetadata just above, is already only ever
				// the override (never the real content-derived time —
				// see combineFileAttr) — so plainly overwriting it here
				// is exactly "set the override," no special-casing
				// needed to avoid clobbering something else.
				a.Mtime = *mtime
				a.mtimeOverrideETag = contentETag
			}
			if btime != nil {
				a.Btime = *btime // likewise only ever the override here
			}
		})
	}

	return f.Stat(ctx, key, typ)
}

// TouchAtime bumps key's recorded last-access time to now — called by
// access layers after a real read(2)/readdir(2), never from Stat/
// Getattr itself, matching POSIX semantics (stat(2) must not itself
// update atime). Deliberately strict (not relatime-approximated): a
// file's first read in each open session, and every directory listing,
// triggers this, accepting the cost of a metadata write rather than
// batching or approximating it.
//
// A lost CAS race against a concurrent chmod/chown/write (the
// underlying updateFileMetadata/UpdateMetadata calls both retry a
// bounded number of times, same as any other metadata update) just
// means this read's atime update is superseded by the other writer's
// own version of the object — acceptable for a field whose own
// documented cost/benefit tradeoff already accepts far worse
// imprecision than that.
func (f *Filesystem) TouchAtime(ctx context.Context, key string, typ EntryType) error {
	if isStreamKey(key) || f.readOnly {
		return nil // a stream has no .metadata of its own; a read-only view records nothing
	}
	now := time.Now()
	return f.writeMetadata(ctx, key, typ, false, func(a *Attr) {
		a.Atime = now
	})
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
