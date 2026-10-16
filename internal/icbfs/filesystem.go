package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"google.golang.org/protobuf/types/known/timestamppb"
	"io"
	"math/rand/v2"
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

// maxTreeRetries bounds the retry loop of the tree and metadata helpers
// (insertEntry, removeEntry, adjustNlink, updateFileMetadata): on a lost
// race the whole top-down operation is retried, not one node.
const maxTreeRetries = 20

// DefaultDirBlockEntries is how many entries a directory block holds
// before it splits, unless the filesystem was created with another number
// (Settings.DirBlockEntries): it favours reads. Against MinIO, with 2,000
// entries in one directory, 128 against 8 made a listing 16 times faster
// and a lookup 40% faster, and creates and unlinks no slower.
const DefaultDirBlockEntries = 128

// defaultDirBlockEntries is DefaultDirBlockEntries, as a var so this
// package's tests can split directories without thousands of files.
var defaultDirBlockEntries = DefaultDirBlockEntries

// minDirBlockEntries and maxDirBlockEntries bound Settings.DirBlockEntries.
const (
	minDirBlockEntries = 4
	maxDirBlockEntries = 4096
)

// DefaultMaxFileSize is the largest file a filesystem holds unless it was
// created with another limit (SetCreationMaxFileSize): 1 TiB. Writes and
// truncates past a filesystem's limit fail with ErrFileTooLarge.
//
// Content is staged in a local scratch file, not memory, so the limit
// bounds local disk per open file and the size of one upload, not RAM.
const DefaultMaxFileSize = 1 << 40

var errRetry = errors.New("icbfs: lost a race, retry the whole operation")

// retryPause waits before attempt (1 and up) of a retry loop: a random
// pause, up to 5 ms doubling each attempt to at most 200 ms, so writers
// that lost the same race don't all retry at once and collide again.
func retryPause(ctx context.Context, attempt int) {
	if attempt == 0 {
		return
	}
	limit := 5 * time.Millisecond << min(attempt-1, 6)
	if limit > 200*time.Millisecond {
		limit = 200 * time.Millisecond
	}
	t := time.NewTimer(time.Duration(rand.Int64N(int64(limit))))
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// Filesystem is the filesystem logic every access layer shares: object-
// store primitives and directory blocks turned into lookup, create, read,
// write, link, rename and the rest.
//
// Its ID and archived state are known only after Bootstrap (or Attach)
// resolves them against the master block; until then it can't touch the
// store.
type Filesystem struct {
	store              objstore.Store
	fsName             string
	id                 string // 4-hex-digit prefix, set by Bootstrap
	archived           bool   // set by Bootstrap; see ErrArchived
	readOnly           bool   // set by BootstrapReadOnly; see ErrReadOnly
	primaryWindows     bool   // set by Bootstrap from the stored master-block value; see PrimaryWindows
	maxFileSize        int64  // set by Bootstrap from the stored master-block value; see MaxFileSize
	newMaxFileSize     uint64 // the max file size a filesystem gets if Bootstrap creates it; see SetCreationMaxFileSize
	dirBlockEntries    int    // from the master-block entry; see DefaultDirBlockEntries
	newDirBlockEntries uint32 // what a filesystem gets if Bootstrap creates it; see SetCreationDirBlockEntries
	lockingEnabled     bool   // off by default; see EnableLocking
	posixACLs          bool   // new items inherit default ACLs; see EnablePosixACLs
	atime              AtimeMode
	noDirAtime         bool // see SetAtime

	// lockNotify is nil unless EnableChangeNotifications was called. It's
	// set once before concurrent use and only read after, as lockingEnabled
	// is.
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
	// Every OpenFile goes through the cache, so there's always one. A fresh
	// temp directory each, so Filesystems in one process (tests) don't
	// share one.
	cacheDir, err := os.MkdirTemp("", "icbfs-cache-")
	if err != nil {
		// Only if the temp directory isn't writable: fall back to a fixed
		// directory rather than make New fallible.
		cacheDir = filepath.Join(os.TempDir(), "icbfs-cache-fallback")
	}
	return &Filesystem{store: store, fsName: fsName, cache: newDiskCache(cacheDir, defaultCacheMaxBytes)}
}

// SetCacheOptions sets the local disk cache's directory and size budget
// (the mounts' -cache-dir and -cache-size). Without it, New's default is a
// fresh temp directory and 512 MiB. It affects only files opened afterward.
func (f *Filesystem) SetCacheOptions(dir string, maxBytes int64) {
	f.cache = newDiskCache(dir, maxBytes)
}

// EnableLocking turns locking on for this Filesystem; it's off by default.
// It's a per-mount choice, not a stored setting, because locking costs an
// extra .lock object and round trips that most mounts don't need. It can be
// called any time after New.
func (f *Filesystem) EnableLocking(enabled bool) {
	f.lockingEnabled = enabled
}

// EnableChangeNotifications lets a blocking lock acquire (AcquireLockRange)
// retry as soon as a signal names the lock's object, instead of waiting out
// its backoff. Without it (nil), acquiring just polls; the signals only
// ever cut latency, never decide correctness.
//
// signals must be this Filesystem's own subscription (for example from
// notify.Broadcaster.Subscribe), not a channel another consumer also reads,
// or the two would steal signals from each other.
func (f *Filesystem) EnableChangeNotifications(signals <-chan notify.Signal) {
	f.lockNotify = signals
}

// PrimaryWindows reports whether this filesystem was created
// primary-Windows (true) or primary-POSIX (false). It's fixed at creation,
// and known after Bootstrap.
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

// SetCreationDirBlockEntries sets how many entries a new filesystem's
// directory blocks hold before they split, if Bootstrap creates it; 0 means
// DefaultDirBlockEntries. Call before Bootstrap.
func (f *Filesystem) SetCreationDirBlockEntries(n uint32) { f.newDirBlockEntries = n }

// MaxFileSize is the largest file this filesystem holds, in bytes. Only
// meaningful after Bootstrap.
func (f *Filesystem) MaxFileSize() int64 { return f.maxFileSize }

// Store is the object store the filesystem is on (an as-of view, for a
// read-only past filesystem).
func (f *Filesystem) Store() objstore.Store { return f.store }

// RootKey is the key of this filesystem's root block, "<id>-root-<name>":
// derived, never stored. Known after Bootstrap.
func (f *Filesystem) RootKey() string { return f.id + "-root-" + f.fsName }

// newKey mints a key for a new object: this filesystem's ID prefix and a
// new UUIDv7. Every object the filesystem creates gets its key here, so one
// prefix listing finds all of them.
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

// Prefix is the key prefix of every object this filesystem owns, for
// callers that scope to it, such as the MinIO notification adapter's
// server-side filter.
func (f *Filesystem) Prefix() string { return f.prefix() }

// checkWritable is called at the top of every mutating operation. It checks
// the archived state Bootstrap read, not the master block: an archive made
// after this Filesystem was opened takes effect at its next Bootstrap (the
// next mount).
func (f *Filesystem) checkWritable() error {
	if f.readOnly {
		return ErrReadOnly
	}
	if f.archived {
		return ErrArchived
	}
	return nil
}

// Bootstrap is create-or-attach in one call, for tests and tools that want
// a filesystem whatever state the bucket is in. Mounts never use it: they
// Attach, and filesystems are created explicitly (CreateFilesystem).
//
// It resolves the filesystem's ID and archived state against the master
// block, registering the name with the given size and primary mode if it's
// new, and creates an empty root with mode, uid and gid if there is none.
// For a filesystem that already exists, size, primary mode, mode, uid and
// gid are all ignored.
func (f *Filesystem) Bootstrap(ctx context.Context, size uint64, mode, uid, gid uint32, primaryWindows bool) error {
	id, entry, _, err := registerFilesystem(ctx, f.store, &pb.FilesystemEntry{
		Name: f.fsName, Size: size, PrimaryWindows: primaryWindows, MaxFileSize: f.newMaxFileSize,
		DirBlockEntries: f.newDirBlockEntries, Created: timestamppb.Now()})
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
	f.dirBlockEntries = dirBlockEntries(entry)
}

// dirBlockEntries is entry's directory block size, or the default.
func dirBlockEntries(entry *pb.FilesystemEntry) int {
	if n := entry.DirBlockEntries; n >= minDirBlockEntries && n <= maxDirBlockEntries {
		return int(n)
	}
	return defaultDirBlockEntries
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

// DiskUsage sums the size of every object under this filesystem's prefix:
// one prefix listing, not a tree walk.
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

// StatFS returns this filesystem's declared capacity ("Total", from the
// master block) and usage ("Used", DiskUsage: every object it owns, roots
// and .metadata included, which is what Prune would delete).
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
// native metadata, where directories kept their attributes before they had
// a .metadata object. It's read only for such a directory; its first
// attribute write migrates it (updateDirMetadata).
func dirAttrFromObject(key string, obj *objstore.Object) Attr {
	mtime := parseMetaTime(obj.Metadata, dirMetaMtime)
	if mtime.IsZero() {
		mtime = obj.LastModified // fallback for objects written before mtime tracking existed
	}
	var winAttrs uint32
	hasWinAttrs := false
	if v, ok := obj.Metadata[dirMetaWinAttr]; ok {
		if bits, err := strconv.ParseUint(v, 10, 32); err == nil {
			winAttrs, hasWinAttrs = uint32(bits)&windowsAttrMask, true
		}
	}
	return Attr{
		WindowsAttrs:    winAttrs,
		HasWindowsAttrs: hasWinAttrs,
		Mode:            parseMetaUint(obj.Metadata, dirMetaMode),
		Uid:             parseMetaUint(obj.Metadata, dirMetaUid),
		Gid:             parseMetaUint(obj.Metadata, dirMetaGid),
		Nlink:           parseMetaUint(obj.Metadata, dirMetaNlink),
		Size:            obj.Size,
		Atime:           parseMetaTime(obj.Metadata, dirMetaAtime), // zero if never read (no fallback — unlike mtime, "never accessed" is a real, honest value here)
		Mtime:           mtime,
		Ctime:           obj.LastModified,
		Btime:           Btime(key),
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
	// attr.Mtime and attr.Btime from decodeFileMetadata are only explicit
	// overrides (SetAttr); real content writes never set them. Without an
	// override, mtime is the content's last write and btime the time in the
	// UUIDv7 key.
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
		// S3's Last-Modified has 1-second resolution and is floored, so a
		// write just after creation can report an mtime before the key's
		// (millisecond) creation time. Content can't change before it
		// exists, so the creation time is the lower bound. This corrects
		// only the derived mtime, never an explicit override, and uses the
		// key's time, not a btime override.
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

// Stat returns the attributes of the node at key (an item's key, or the
// root key). typ must be the node's type: a directory's attributes are read
// differently from a file's.
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
		for blk.Past(name) {
			key = blk.Right
			if blk, obj, err = f.readBlock(ctx, key); err != nil {
				return nil, err
			}
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
		for blk.Past(name) {
			if blk, _, err = f.readBlock(ctx, blk.Right); err != nil {
				return block.Entry{}, false, err
			}
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
// spills into, following right links as walkChain does.
func (f *Filesystem) scanFold(ctx context.Context, key, name string, visit func(block.Entry) bool) (bool, error) {
	return f.scanFoldChain(ctx, key, "", name, visit)
}

// scanFoldChain is scanFold over the block at key and the right siblings
// that cover names below upper ("" for no bound). It returns true once
// the run is known to be over.
func (f *Filesystem) scanFoldChain(ctx context.Context, key, upper, name string, visit func(block.Entry) bool) (bool, error) {
	for {
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
		} else {
			start := blk.ChildIndexFold(name)
			for i := start; i < len(blk.Children); i++ {
				// A later child's MinKey bounds everything in it from
				// below: once that sorts past name, so does the rest.
				if i > start && block.CompareFold(blk.Children[i].MinKey, name) > 0 {
					return true, nil
				}
				done, err := f.scanFoldChain(ctx, blk.Children[i].UUID, childUpper(blk, i, upper), name, visit)
				if err != nil || done {
					return done, err
				}
			}
		}
		if !followRight(blk, upper) {
			return false, nil
		}
		if block.CompareFold(blk.HighKey, name) > 0 {
			return true, nil // the next block starts past the run
		}
		key = blk.Right
	}
}

// walkChain calls leaf for every leaf of the shard tree at key, in order:
// the block at key, every block under it, and the right siblings that
// cover names below upper ("" for no bound), which a parent may not list
// yet after a split. leaf returning true stops the walk.
func (f *Filesystem) walkChain(ctx context.Context, key, upper string, leaf func(*block.Block) bool) (bool, error) {
	for {
		blk, _, err := f.readBlock(ctx, key)
		if err != nil {
			return false, err
		}
		if blk.Kind == block.Leaf {
			if leaf(blk) {
				return true, nil
			}
		} else {
			for i, c := range blk.Children {
				stop, err := f.walkChain(ctx, c.UUID, childUpper(blk, i, upper), leaf)
				if err != nil || stop {
					return stop, err
				}
			}
		}
		if !followRight(blk, upper) {
			return false, nil
		}
		key = blk.Right
	}
}

// childUpper is the bound below which child i of blk covers names: the
// next child's MinKey, or for the last child, blk's own bound.
func childUpper(blk *block.Block, i int, upper string) string {
	if i+1 < len(blk.Children) {
		return blk.Children[i+1].MinKey
	}
	if blk.HighKey != "" {
		return blk.HighKey
	}
	return upper
}

// followRight reports whether a walk bounded by upper goes on to blk's
// right sibling: there is one, and it starts below upper, so the parent
// doesn't already lead to it.
func followRight(blk *block.Block, upper string) bool {
	if blk.Right == "" || blk.HighKey == "" {
		return false
	}
	return upper == "" || blk.Collation.Compare(blk.HighKey, upper) < 0
}

// collectEntries gathers every entry in the shard tree rooted at rootKey,
// across however many levels it currently has.
func (f *Filesystem) collectEntries(ctx context.Context, rootKey string) ([]block.Entry, error) {
	var all []block.Entry
	_, err := f.walkChain(ctx, rootKey, "", func(blk *block.Block) bool {
		all = append(all, blk.Entries...)
		return false
	})
	return all, err
}

// insertEntry adds name -> uuid to the directory rooted at rootKey,
// splitting nodes top-down as needed. A lost race anywhere along the path
// retries the whole operation; split halves written by the abandoned
// attempt are left unreferenced, which is harmless.
func (f *Filesystem) insertEntry(ctx context.Context, rootKey, name, uuid string, typ EntryType) error {
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
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
	return f.writeBack(ctx, rootKey, path[len(path)-1], leaf.blk)
}

// writeBack writes step's block back as blk (now one entry or child
// larger), conditional on the version read, splitting it if it overflows.
//
// A split is a B-link split: the right half goes to a new key first, then
// one conditional write turns the block into its left half, bounded at the
// separator and linked to the right half. That write commits the split:
// from then on a lookup for a name in the right half moves right to it.
// Pointing the parent at the right half (linkChild) only shortens that
// path, and can be retried on its own. The directory's own key (rootKey)
// can't move, so it splits into two new blocks and becomes an internal
// node over them.
func (f *Filesystem) writeBack(ctx context.Context, rootKey string, step pathStep, blk *block.Block) error {
	overflowing := (blk.Kind == block.Leaf && len(blk.Entries) > f.dirBlockEntries) ||
		(blk.Kind == block.Internal && len(blk.Children) > f.dirBlockEntries)
	if !overflowing {
		err := f.putBlock(ctx, step.key, blk, step.obj.Metadata, step.obj.ETag)
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
	rightKey, err := f.newKey()
	if err != nil {
		return err
	}
	if err := f.putBlock(ctx, rightKey, right, step.obj.Metadata, ""); err != nil {
		return err
	}
	left.Right = rightKey

	if step.key == rootKey {
		leftKey, err := f.newKey()
		if err != nil {
			return err
		}
		if err := f.putBlock(ctx, leftKey, left, step.obj.Metadata, ""); err != nil {
			return err
		}
		top := &block.Block{Kind: block.Internal, Collation: blk.Collation, Children: []block.Child{
			{MinKey: "", UUID: leftKey},
			{MinKey: sep, UUID: rightKey},
		}}
		err = f.putBlock(ctx, rootKey, top, step.obj.Metadata, step.obj.ETag)
		if objstore.IsPreconditionFailed(err) {
			return errRetry
		}
		return err
	}

	err = f.putBlock(ctx, step.key, left, step.obj.Metadata, step.obj.ETag)
	if objstore.IsPreconditionFailed(err) {
		return errRetry // the right half is left unreferenced, which is harmless
	}
	if err != nil {
		return err
	}
	if linkParents {
		f.linkChild(ctx, rootKey, step.key, block.Child{MinKey: sep, UUID: rightKey})
	}
	return nil
}

// linkParents is false only in tests that check blocks reachable through
// right links alone.
var linkParents = true

// linkChild points the parent of block leftKey at its new right sibling
// c, after a split. Best effort: the sibling is already reachable through
// leftKey's right link, so giving up costs only a longer path.
func (f *Filesystem) linkChild(ctx context.Context, rootKey, leftKey string, c block.Child) {
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
		if err := f.tryLinkChild(ctx, rootKey, leftKey, c); !errors.Is(err, errRetry) {
			return
		}
	}
}

// tryLinkChild descends toward c.MinKey to the internal node that leads to
// leftKey for it, and adds c there (splitting that node if it overflows).
func (f *Filesystem) tryLinkChild(ctx context.Context, rootKey, leftKey string, c block.Child) error {
	key := rootKey
	for {
		blk, obj, err := f.readBlock(ctx, key)
		if err != nil {
			return err
		}
		for blk.Past(c.MinKey) {
			key = blk.Right
			if blk, obj, err = f.readBlock(ctx, key); err != nil {
				return err
			}
		}
		if blk.Kind == block.Leaf {
			return nil // no parent leads to leftKey: reachable through right links only
		}
		next := blk.ChildFor(c.MinKey)
		switch next.UUID {
		case c.UUID:
			return nil // already linked
		case leftKey:
			blk.InsertChild(c)
			return f.writeBack(ctx, rootKey, pathStep{key, blk, obj}, blk)
		}
		key = next.UUID
	}
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
		retryPause(ctx, attempt)
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
	if err := f.inheritACLs(ctx, dirKey, &attr, true); err != nil {
		return "", Attr{}, err
	}
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

// Create makes a new, empty regular file named name in dirKey. Its
// attributes go in its .metadata object; the content object carries none.
// The third result is the empty content's ETag, so a caller can open the
// file for writing without reading it back.
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

	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Ctime: time.Now()}
	if err := f.inheritACLs(ctx, dirKey, &attr, false); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, "", err
	}
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

// Mknod creates a FIFO, a Unix socket, or a character or block device (typ
// TypeFifo, TypeSocket, TypeCharDevice or TypeBlockDevice); rdev is stored
// only for devices.
//
// It has the same two objects as a file (an empty content object that is
// never written again, and .metadata), so Stat, SetAttr and TouchAtime work
// unchanged. The kernel serves these types' I/O itself, so there is no
// OpenFile and no ETag.
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

	attr := Attr{Mode: mode, Uid: uid, Gid: gid, Nlink: 1, Ctime: time.Now()}
	if typ == TypeCharDevice || typ == TypeBlockDevice {
		attr.Rdev = rdev
	}
	if err := f.inheritACLs(ctx, dirKey, &attr, false); err != nil {
		_ = f.store.Delete(ctx, newUUID, "")
		return "", Attr{}, err
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

	attr := Attr{Mode: 0777, Uid: uid, Gid: gid, Nlink: 1, Ctime: time.Now()}
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

// adjustNlink changes uuid's link count by delta, a conditional
// read-modify-write of its .metadata. At zero it first writes nlink=0 under
// the same condition, so a concurrent Link either sees the tombstone or
// loses the race and retries; it never revives an item this call is
// deleting. Then it deletes the item's objects.
func (f *Filesystem) adjustNlink(ctx context.Context, uuid string, delta int) (deleted bool, err error) {
	mk := metadataObjectKey(uuid)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
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
			// This call wrote the zero, so no Link can get past it:
			// deleting is safe now.
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

// updateFileMetadata applies mutate to a file's or symlink's .metadata
// under compare-and-swap, retrying on a lost race, so concurrent chmod,
// chown and link serialize instead of losing each other. Any change through
// it is a status change (see writeMetadata).
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
	return f.writeMetadataIf(ctx, key, typ, statusChange, func(a *Attr) bool { mutate(a); return true })
}

// writeMetadataIf is writeMetadata whose mutate can decline (false), and
// then nothing is written. A write that isn't a status change keeps the
// record's ctime: one with none stored gets its current one, the
// record's own LastModified, so the write can't move it.
func (f *Filesystem) writeMetadataIf(ctx context.Context, key string, typ EntryType, statusChange bool, mutate func(*Attr) bool) error {
	mk := metadataObjectKey(key)
	for attempt := 0; attempt < maxTreeRetries; attempt++ {
		retryPause(ctx, attempt)
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
				Atime: legacy.Atime, Mtime: legacy.Mtime, Ctime: legacy.Ctime,
				WindowsAttrs: legacy.WindowsAttrs, HasWindowsAttrs: legacy.HasWindowsAttrs}
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
		if !mutate(&attr) {
			return nil
		}
		if statusChange {
			attr.Ctime = time.Now()
		} else if attr.Ctime.IsZero() {
			attr.Ctime = obj.LastModified
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

// SetWindowsAttributes stores a file's or symlink's ReadOnly, Hidden,
// System and Archive bits, whatever the primary mode. Whether they or the
// POSIX mode is authoritative when read is the caller's decision, by
// primary mode.
func (f *Filesystem) SetWindowsAttributes(ctx context.Context, uuid string, typ EntryType, bits uint32) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateMetadata(ctx, uuid, typ, func(a *Attr) {
		a.WindowsAttrs, a.HasWindowsAttrs = bits&windowsAttrMask, true
	})
}

// MarkWindowsDirSymlink records that symlink uuid is a Windows directory
// symlink (see Attr.WindowsDirSymlink).
func (f *Filesystem) MarkWindowsDirSymlink(ctx context.Context, uuid string) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateFileMetadata(ctx, uuid, func(a *Attr) {
		a.WindowsDirSymlink = true
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
		a.WindowsAttrs, a.HasWindowsAttrs = bits&windowsAttrMask, true
		if sddl != nil {
			a.WindowsSDDL = sddl
		}
	})
}

// SetWindowsACL stores a file's, symlink's or directory's Windows
// security descriptor, as SDDL (see Attr.WindowsACL).
func (f *Filesystem) SetWindowsACL(ctx context.Context, uuid string, typ EntryType, sddl []byte) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateMetadata(ctx, uuid, typ, func(a *Attr) {
		a.WindowsSDDL = sddl
	})
}

// SetAppData sets (or, with a nil value, removes) one entry of the data
// icbfs's own tools keep about an item (Attr.App), never visible through
// a mount.
func (f *Filesystem) SetAppData(ctx context.Context, uuid string, typ EntryType, key string, value []byte) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateMetadata(ctx, uuid, typ, func(a *Attr) {
		if value == nil {
			delete(a.App, key)
			return
		}
		if a.App == nil {
			a.App = make(map[string][]byte)
		}
		a.App[key] = value
	})
}

// SetGenericReparseData stores a file's raw, non-symlink reparse point
// buffer as is (see Attr.GenericReparse).
func (f *Filesystem) SetGenericReparseData(ctx context.Context, uuid string, buf []byte) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateFileMetadata(ctx, uuid, func(a *Attr) {
		a.Reparse = buf
	})
}

// DeleteGenericReparseData removes a file's non-symlink reparse point
// buffer, if any (FSCTL_DELETE_REPARSE_POINT), making it an ordinary file
// again.
func (f *Filesystem) DeleteGenericReparseData(ctx context.Context, uuid string) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	return f.updateFileMetadata(ctx, uuid, func(a *Attr) {
		a.Reparse = nil
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

// dirContains reports whether targetUUID is ancestorUUID or anywhere under
// it, so Rename can refuse to move a directory into itself (EINVAL). There
// are no parent pointers, so it walks down from ancestorUUID.
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

// Rename moves the entry oldName in oldDirKey to newName in newDirKey (the
// same directory if the keys are equal), with rename(2)'s semantics.
//
// replace false is RENAME_NOREPLACE (FUSE's flag; WinFsp's replaceIfExist).
// Otherwise an existing target is replaced as rename(2) says: a file can't
// replace a non-empty directory or the other way round, and two names for
// the same object are a no-op either way.
//
// Remove and insert are separate conditional writes, not one transaction,
// so a concurrent reader can briefly see the entry in neither place.
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
			// Two names for the same object (or, in one directory, the same
			// entry): rename(2) does nothing and succeeds, whatever replace
			// says.
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

// ReadFile returns a regular file's whole content and attributes, fetching
// content and .metadata concurrently, and the content's ETag, so a caller
// that edits and writes back can make that write conditional on it.
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

// WriteFile replaces a regular file's whole content. Attributes are in
// .metadata and untouched, so the content object's LastModified is the
// file's mtime.
//
// ifMatch makes the write conditional on the content's current ETag, as
// objstore.Store.Put's does; empty means unconditional. It returns the new
// ETag, so the next write can be conditional on this one without a read.
func (f *Filesystem) WriteFile(ctx context.Context, key string, data []byte, ifMatch string) (Attr, string, error) {
	return f.writeFileFrom(ctx, key, bytes.NewReader(data), ifMatch)
}

// writeFileFrom is WriteFile from a reader: OpenFile.Flush passes its
// scratch file, so a large file is never held in memory.
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

// SetAttr applies the given changes (each may be nil) to the node at key.
// typ must be the node's type (see Stat).
//
// For a directory, every change goes to its .metadata under
// compare-and-swap; size is ignored.
//
// For a file or symlink, a size change rewrites content through an OpenFile
// session (a real content change, so mtime moves); mode, uid and gid go
// through updateFileMetadata.
//
// mtime is an explicit override (SetFileTime, utimensat), with no content
// write. For a file it's tied to the content version it was set on and
// lapses at the next write, so mtime follows content again; that costs one
// Head per explicit set, nothing on the write path.
//
// btime is an explicit creation-time override (SetFileTime's CreationTime);
// unset, creation time is the one in the UUIDv7 key.
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
				chmodACL(a, *mode)
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
				chmodACL(a, *mode)
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
				// a.Mtime here is only ever the override (see
				// combineFileAttr), so setting it is setting the override.
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

// TouchAtime sets key's access time to now. Access layers call it after a
// real read or directory listing, never from stat, which must not change
// atime. It's strict, not relatime: a file's first read in each open
// session, and every listing, costs a metadata write.
//
// A lost race against a concurrent chmod, chown or write just means this
// access time is superseded by that writer's version, which is fine for a
// best-effort field.
func (f *Filesystem) TouchAtime(ctx context.Context, key string, typ EntryType) error {
	if isStreamKey(key) || f.readOnly {
		return nil // a stream has no .metadata of its own; a read-only view records nothing
	}
	if f.atime == AtimeNever || (typ == TypeDir && f.noDirAtime) {
		return nil
	}
	now := time.Now()
	if f.atime == AtimeRelative {
		a, err := f.Stat(ctx, key, typ)
		if err != nil {
			return err
		}
		if !relatimeDue(a, now) {
			return nil
		}
	}
	return f.writeMetadataIf(ctx, key, typ, false, func(a *Attr) bool {
		a.Atime = now
		return true
	})
}

// AtimeMode is when reads record an access time, as Linux's mount
// options: AtimeStrict on every read (strictatime), AtimeRelative only
// when the recorded one is no newer than the last change or a day old
// (relatime, Linux's default), AtimeNever not at all (noatime).
type AtimeMode int

const (
	AtimeStrict AtimeMode = iota
	AtimeRelative
	AtimeNever
)

// SetAtime sets when reads record access times; dirs false records none
// for directories (nodiratime). The default is AtimeStrict, with dirs.
func (f *Filesystem) SetAtime(mode AtimeMode, dirs bool) {
	f.atime, f.noDirAtime = mode, !dirs
}

// relatimeDue is relatime's rule: record an access if the item was never
// read, or its access time is no newer than its last modification or
// status change, or a day old.
func relatimeDue(a Attr, now time.Time) bool {
	return a.Atime.IsZero() || !a.Atime.After(a.Mtime) || !a.Atime.After(a.Ctime) || now.Sub(a.Atime) >= 24*time.Hour
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
