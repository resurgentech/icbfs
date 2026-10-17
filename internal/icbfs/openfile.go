package icbfs

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// contentWriteRetryBudget bounds how long OpenFile.Flush retries a lost
// race before escalating to a lock. It's a time, not a count, because a
// retry resends the whole file, so a count would give large and small files
// very different windows. A var so tests can shrink it.
var contentWriteRetryBudget = 5 * time.Second

// lockEscalationLeaseTTL is the lease Flush's escalation takes: ample for
// one read-modify-write under load. Flush releases it as soon as it's done,
// so the length only matters if the holder dies mid-escalation.
const lockEscalationLeaseTTL = 30 * time.Second

// ErrWriteContention is Flush's answer when the retry budget runs out on a
// mount without locking, or when, with locking, even the escalated write
// can't land (for example, ctx ends before the lock is taken).
var ErrWriteContention = errors.New("content write: exceeded retry budget and lock escalation under contention")

// content is a file's content while it's open in this process: a local
// scratch file every open of the file reads and writes, as on a local
// disk, flushed back as one conditional whole-object write that, on a
// lost race, is re-applied onto the current content (docs/architecture.md's
// Concurrency section). It lives here, not in an access layer, so FUSE and
// WinFsp share the retry and escalation logic. The last close of the file
// ends it.
//
// Below it, a shared LRU cache of fetched versions (diskCache) saves
// re-downloading unchanged content; content never modifies a cached
// version, and a successful Flush replaces its entry.
type content struct {
	fsys *Filesystem
	key  string
	refs int // the OpenFiles using it; guarded by fsys.openMu

	mu    sync.Mutex
	file  *os.File // the local scratch file
	size  int64
	etag  string
	dirty bool

	// baseVersion and baseSize identify the stored version the scratch file
	// was seeded from (etag is its ETag). Outside the touched ranges, the
	// scratch file still holds exactly those bytes, so a multipart flush
	// can copy those ranges on the server (partsLocked). Empty when
	// unknown, as right after Create; then nothing is copied.
	baseVersion string
	baseSize    int64

	// touched is every byte range written since the last flush, through
	// any open. The writes themselves are already merged in the scratch
	// file, in order, so a rebase needs only these ranges read back from
	// it, not a log of writes.
	touched byteRanges

	// truncated is set when Truncate has fixed the size since the last
	// flush, so a rebase onto newer content keeps that size instead of the
	// base's.
	truncated bool

	// lastWrite is when the content last changed — the mtime Stat reports
	// until it's flushed (see Filesystem.localOverlay).
	lastWrite time.Time
}

// OpenFile is one open of a file: a handle on the file's content, shared
// with every other open of it in this process, so each sees the others'
// writes at once. Each open has its own identity for escalation locks and
// records an access once.
type OpenFile struct {
	fsys   *Filesystem
	key    string
	holder string // this open's unique identity for escalation locks
	c      *content

	mu           sync.Mutex
	closed       bool
	atimeTouched bool // see TouchAtime
}

// newOpenFileHolder mints a unique identity for an open's escalation
// locks.
func (f *Filesystem) newOpenFileHolder() (string, error) {
	id, err := NewUUIDv7()
	return f.LockHolder(id), err
}

// Open returns an OpenFile on key's content: the content another open in
// this process already has, or key's current content and ETag, from the
// cache or the store. A Head learns the current ETag; only a cache miss on
// it fetches the content.
//
// Content already open is brought up to the stored version if it has no
// unflushed writes and another client has written since, so an open
// still sees what was written elsewhere before it (close-to-open). With
// unflushed writes, the next flush merges them onto the stored version.
func (f *Filesystem) Open(ctx context.Context, key string) (*OpenFile, error) {
	head, err := f.store.Head(ctx, key)
	if err != nil {
		return nil, mapNotFound(err)
	}
	holder, err := f.newOpenFileHolder()
	if err != nil {
		return nil, err
	}
	if c := f.shareContent(key); c != nil {
		c.mu.Lock()
		if !c.dirty && c.etag != head.ETag {
			err = c.rebaseLocked(ctx)
		}
		c.mu.Unlock()
		o := &OpenFile{fsys: f, key: key, holder: holder, c: c}
		f.holdOpen(o)
		if err != nil {
			o.Close()
			return nil, err
		}
		return o, nil
	}
	fetch := func(ctx context.Context) (io.ReadCloser, error) {
		body, _, err := f.store.Get(ctx, key)
		if err != nil {
			return nil, mapNotFound(err)
		}
		return body, nil
	}
	file, size, err := f.cache.seedScratch(ctx, key, head.ETag, fetch)
	if err != nil {
		return nil, err
	}
	c := f.installContent(&content{fsys: f, key: key, file: file, size: size, etag: head.ETag,
		baseVersion: head.VersionID, baseSize: size})
	o := &OpenFile{fsys: f, key: key, holder: holder, c: c}
	f.holdOpen(o)
	return o, nil
}

// NewOpenFile seeds an OpenFile from content the caller just wrote (its
// key, ETag and data; right after Create, nothing), with no read back.
func (f *Filesystem) NewOpenFile(key, etag string, data []byte) *OpenFile {
	holder, err := f.newOpenFileHolder()
	if err != nil {
		// Only if the system's random source fails. A fixed holder means
		// two such opens could share a lock identity while escalating;
		// nothing else is affected.
		holder = "unknown-rng-failure"
	}
	c := f.shareContent(key)
	if c == nil {
		c = &content{fsys: f, key: key, etag: etag, baseSize: int64(len(data))}
		// Only if a local temp file can't be created is file nil: the
		// OpenFile then fails on first use, rather than every caller of
		// Create and Mknod having to handle an error here.
		if file, err := f.cache.newScratchFile(); err == nil {
			if len(data) > 0 {
				if _, err := file.Write(data); err == nil {
					_, _ = file.Seek(0, io.SeekStart)
				}
			}
			c.file, c.size = file, int64(len(data))
		}
		c = f.installContent(c)
	}
	o := &OpenFile{fsys: f, key: key, holder: holder, c: c}
	f.holdOpen(o)
	return o
}

// Close ends this open. Access layers call it exactly once, when the
// handle closes. The last open's Close removes the scratch file, unflushed
// writes and all: flushing is the caller's (Flush, at every close(2)).
//
// The last Close of a file unlinked while open deletes its objects (see
// openSessions).
func (o *OpenFile) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	o.mu.Unlock()
	defer o.fsys.releaseOpen(o)
	if !o.fsys.dropContent(o.c) {
		return nil
	}
	c := o.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return nil
	}
	name := c.file.Name()
	err := c.file.Close()
	_ = os.Remove(name)
	c.file = nil
	return err
}

// pending reports the content's unflushed state: its current size, when
// it last changed, and whether anything is waiting to be flushed.
func (c *content) pending() (size int64, lastWrite time.Time, dirty bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size, c.lastWrite, c.dirty
}

// Dirty reports whether the file has writes, through any open, that Flush
// hasn't stored.
func (o *OpenFile) Dirty() bool {
	_, _, dirty := o.c.pending()
	return dirty
}

// Size returns the current (post-edit, pre-flush) content length.
func (o *OpenFile) Size() int64 {
	size, _, _ := o.c.pending()
	return size
}

// ReadAt returns up to length bytes at off: every open's writes included,
// before Flush makes them visible to anyone else.
func (o *OpenFile) ReadAt(off, length int64) []byte { return o.c.ReadAt(off, length) }

// WriteAt writes data at off, visible at once to every open of the file.
// A negative offset is ErrInvalidArgument, and an end past the max file
// size ErrFileTooLarge.
func (o *OpenFile) WriteAt(data []byte, off int64) error { return o.c.WriteAt(data, off) }

// Truncate sets the file's length, as ftruncate(2) does, for every open.
func (o *OpenFile) Truncate(size int64) error { return o.c.Truncate(size) }

// Allocate is fallocate(2); see content.Allocate.
func (o *OpenFile) Allocate(off, length int64, mode uint32) error {
	return o.c.Allocate(off, length, mode)
}

// Flush stores every open's writes; see content.Flush.
func (o *OpenFile) Flush(ctx context.Context) (Attr, error) { return o.c.Flush(ctx, o.holder) }

// SyncAfterLock brings the content up to date for a lock just taken, as
// NFS revalidates a file's cache when a lock is acquired, so that what
// the last holder wrote reads here even through a descriptor opened
// before it wrote. Unflushed writes are stored, rebased onto a newer
// stored version if there is one; with none, a newer stored version
// replaces the content. changed reports that what this file reads may now
// differ, so the caller drops the kernel's pages and attributes for it.
func (o *OpenFile) SyncAfterLock(ctx context.Context) (changed bool, err error) {
	return o.c.syncAfterLock(ctx, o.holder)
}

// TouchAtime records an access, at most once per session. The kernel splits
// one read(2) into many FUSE reads (about 128 KiB each), and one metadata
// write per chunk would turn a large read into thousands of synchronous
// round trips. Strict atime means an access is recorded at once, not that
// every chunk of the same read is.
func (o *OpenFile) TouchAtime(ctx context.Context) error {
	o.mu.Lock()
	if o.atimeTouched {
		o.mu.Unlock()
		return nil
	}
	o.atimeTouched = true
	o.mu.Unlock()
	return o.fsys.TouchAtime(ctx, o.key, TypeFile)
}

func (c *content) ReadAt(off, length int64) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil || off >= c.size || length <= 0 {
		return nil
	}
	end := off + length
	if end > c.size {
		end = c.size
	}
	out := make([]byte, end-off)
	n, err := c.file.ReadAt(out, off)
	if err != nil && err != io.EOF {
		return nil
	}
	return out[:n]
}

// WriteAt writes data at off into the scratch file, and adds it to the
// touched ranges Flush re-applies after a lost race.
func (c *content) WriteAt(data []byte, off int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.applyLocked(data, off); err != nil {
		return err
	}
	c.touched.add(off, off+int64(len(data)))
	c.dirty = true
	c.lastWrite = time.Now()
	return nil
}

// errSessionClosed is Flush on content whose last open has closed.
var errSessionClosed = errors.New("icbfs: file closed")

// Truncate sets the content's length, as ftruncate(2) does: a shrink
// drops the tail, a grow extends with zeros (sparsely, on the local scratch
// file). Like WriteAt, it's flushed by Flush.
func (c *content) Truncate(size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if size < 0 || c.file == nil {
		return ErrInvalidArgument
	}
	if size > c.fsys.maxFileSize {
		return ErrFileTooLarge
	}
	if size == c.size {
		return nil
	}
	return c.resizeLocked(size)
}

// fallocate(2)'s modes. FUSE passes on only these; the kernel refuses
// collapsing and inserting ranges itself.
const (
	FallocKeepSize  = 0x01
	FallocPunchHole = 0x02
	FallocZeroRange = 0x10
)

// Allocate is fallocate(2) on the content, flushed as a write is. Content is stored whole, zeros included, so there's no space to
// reserve: a plain allocation only grows the size (unless FallocKeepSize),
// and punching a hole and zeroing a range write zeros. Any other mode is
// ErrNotSupported.
func (c *content) Allocate(off, length int64, mode uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if off < 0 || length <= 0 || c.file == nil {
		return ErrInvalidArgument
	}
	end := off + length
	if end < off {
		return ErrFileTooLarge
	}
	keep := mode&FallocKeepSize != 0
	grow := func(size int64) error {
		if size > c.fsys.maxFileSize {
			return ErrFileTooLarge
		}
		if keep || size <= c.size {
			return nil
		}
		return c.resizeLocked(size)
	}
	switch mode &^ FallocKeepSize {
	case 0:
		return grow(end)
	case FallocPunchHole:
		if !keep {
			return ErrInvalidArgument // as Linux: a hole never changes the size
		}
		return c.zeroLocked(off, min(end, c.size))
	case FallocZeroRange:
		if err := grow(end); err != nil {
			return err
		}
		return c.zeroLocked(off, min(end, c.size))
	default:
		return ErrNotSupported
	}
}

// resizeLocked sets the scratch file's size, as Truncate does.
func (c *content) resizeLocked(size int64) error {
	if err := c.file.Truncate(size); err != nil {
		return err
	}
	c.touchLocked(min(size, c.size), max(size, c.size))
	c.size = size
	c.truncated = true
	return nil
}

// zeroLocked writes zeros over [lo, hi).
func (c *content) zeroLocked(lo, hi int64) error {
	if lo >= hi {
		return nil
	}
	zeros := make([]byte, min(hi-lo, 1<<20))
	for at := lo; at < hi; {
		n := min(hi-at, int64(len(zeros)))
		if _, err := c.file.WriteAt(zeros[:n], at); err != nil {
			return err
		}
		at += n
	}
	c.touchLocked(lo, hi)
	return nil
}

// touchLocked adds [lo, hi) to the touched ranges and marks the content
// dirty, as WriteAt does.
func (c *content) touchLocked(lo, hi int64) {
	c.touched.add(lo, hi)
	c.dirty = true
	c.lastWrite = time.Now()
}

// applyLocked extends the local scratch file to cover off+len(data) if
// the write goes past the current end, then writes data in at off.
//
// A negative offset is invalid; an end past the filesystem's max file
// size is ErrFileTooLarge (see DefaultMaxFileSize), checked before the
// scratch file is extended.
func (c *content) applyLocked(data []byte, off int64) error {
	if off < 0 {
		return ErrInvalidArgument
	}
	end := off + int64(len(data))
	if end > c.fsys.maxFileSize {
		return ErrFileTooLarge
	}
	if c.file == nil {
		return ErrInvalidArgument
	}
	if end > c.size {
		if err := c.file.Truncate(end); err != nil {
			return err
		}
		c.size = end
	}
	if len(data) > 0 {
		if _, err := c.file.WriteAt(data, off); err != nil {
			return err
		}
	}
	return nil
}

// touchedRangeLocked returns the span of everything written since the
// last flush: one range over all of touched, for the escalation lock,
// which only has to cover them. It's called only when dirty, which
// WriteAt never sets without adding a range.
func (c *content) touchedRangeLocked() (start, end int64) {
	return c.touched.span()
}

// syncAfterLock is OpenFile.SyncAfterLock.
func (c *content) syncAfterLock(ctx context.Context, holder string) (bool, error) {
	c.mu.Lock()
	if c.dirty {
		c.mu.Unlock()
		// A flush that lost a race rebases onto what the other writer
		// stored, so the content may change.
		_, err := c.Flush(ctx, holder)
		return true, err
	}
	defer c.mu.Unlock()
	if c.file == nil {
		return false, errSessionClosed
	}
	head, err := c.fsys.store.Head(ctx, c.key)
	if objstore.IsNotFound(err) {
		return false, nil // removed elsewhere: nothing newer to read
	}
	if err != nil {
		return false, err
	}
	if head.ETag == c.etag {
		return false, nil
	}
	return true, c.rebaseLocked(ctx)
}

// rebaseLocked fetches key's current content into a fresh scratch file
// (through the cache, as Open does), copies the touched ranges onto it
// from the old scratch file, and updates c.etag. With nothing touched, it
// brings the content up to the stored version.
func (c *content) rebaseLocked(ctx context.Context) error {
	type edit struct {
		off  int64
		data []byte
	}
	var ownEdits []edit
	ownSize := c.size
	if c.file != nil {
		for _, r := range c.touched.ranges() {
			// Only what's still inside the content: past its end is a
			// truncated-away tail, which the size below takes care of.
			if r[0] >= ownSize {
				break
			}
			data := make([]byte, min(r[1], ownSize)-r[0])
			if _, err := c.file.ReadAt(data, r[0]); err != nil && err != io.EOF {
				return err
			}
			ownEdits = append(ownEdits, edit{r[0], data})
		}
	}

	head, err := c.fsys.store.Head(ctx, c.key)
	if err != nil {
		return mapNotFound(err)
	}
	fetch := func(ctx context.Context) (io.ReadCloser, error) {
		body, _, err := c.fsys.store.Get(ctx, c.key)
		if err != nil {
			return nil, mapNotFound(err)
		}
		return body, nil
	}
	fresh, size, err := c.fsys.cache.seedScratch(ctx, c.key, head.ETag, fetch)
	if err != nil {
		return err
	}
	oldName := ""
	if c.file != nil {
		oldName = c.file.Name()
		c.file.Close()
	}
	c.file = fresh
	c.size = size
	for _, e := range ownEdits {
		if err := c.applyLocked(e.data, e.off); err != nil {
			if oldName != "" {
				_ = os.Remove(oldName)
			}
			return err
		}
	}
	if c.truncated && c.size != ownSize {
		if err := c.file.Truncate(ownSize); err != nil {
			if oldName != "" {
				_ = os.Remove(oldName)
			}
			return err
		}
		c.size = ownSize
	}
	if oldName != "" {
		_ = os.Remove(oldName)
	}
	c.etag = head.ETag
	c.baseVersion, c.baseSize = head.VersionID, size
	return nil
}

// Flush writes the content back as one whole-object write, conditional on
// the ETag the scratch file is based on: every open's writes, as a close on
// a local disk stores them, whichever descriptor made them. holder is the
// flushing open's identity, for the lock check and escalation.
//
// On a lost race the scratch file can't just be resent: it's built on
// content that's no longer current, and would erase the other writer's
// change. Flush instead rebases: fetches the current content, copies its
// own touched ranges onto it, and tries again. Two writers editing
// different ranges both land, as on a local disk, interleaved or not.
//
// Retrying is bounded by contentWriteRetryBudget. After that, with locking
// on, Flush escalates: it takes a lock over its touched ranges, makes one more
// conditional write while holding it, and releases it. Escalating writers
// take turns on the lock instead of burning retries against each other, so
// each one gets its write in.
func (c *content) Flush(ctx context.Context, holder string) (Attr, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return c.fsys.statFile(ctx, c.key)
	}
	if c.file == nil {
		return Attr{}, errSessionClosed
	}
	if c.fsys.isOrphan(c.key) {
		// Unlinked while open: nothing can see this content any more,
		// and its objects go at the last Close. Drop the writes.
		c.dirty = false
		c.truncated = false
		c.touched.clear()
		return c.fsys.statFile(ctx, c.key)
	}

	// Checked before every write: refuse at once if the touched range
	// overlaps another holder's application lock (fcntl, LockFileEx). A
	// lost race alone couldn't tell that apart from ordinary contention,
	// which retry and escalation handle.
	if err := c.fsys.checkRangesLockConflict(ctx, c.key, c.touched.ranges(), holder); err != nil {
		return Attr{}, err
	}
	start, end := c.touchedRangeLocked()

	deadline := time.Now().Add(contentWriteRetryBudget)
	for {
		attr, err := c.uploadLocked(ctx)
		if err == nil {
			return attr, nil
		}
		if !objstore.IsPreconditionFailed(err) {
			return Attr{}, err
		}
		if time.Now().After(deadline) {
			break
		}
		if err := c.rebaseLocked(ctx); err != nil {
			return Attr{}, err
		}
	}

	if !c.fsys.lockingEnabled {
		// Escalation needs locking, so without it running out of retries is
		// final.
		return Attr{}, ErrWriteContention
	}
	if err := c.fsys.acquireEscalationLockRange(ctx, c.key, start, end, holder, lockEscalationLeaseTTL); err != nil {
		return Attr{}, ErrWriteContention
	}
	defer func() { _ = c.fsys.ReleaseLockRange(context.WithoutCancel(ctx), c.key, start, end, holder) }()

	if err := c.rebaseLocked(ctx); err != nil {
		return Attr{}, err
	}
	return c.uploadLocked(ctx)
}

// uploadLocked writes the scratch file back to the store, conditioned on
// the ETag it's based on: one Put for a file that fits in one part, and
// otherwise a multipart write (partsLocked) that only uploads the parts
// that changed. On success the content is clean and based on the version
// just written.
func (c *content) uploadLocked(ctx context.Context) (Attr, error) {
	if err := c.fsys.checkWritable(); err != nil {
		return Attr{}, err
	}
	var obj *objstore.Object
	var err error
	if partSize := c.fsys.partSize(); c.size > partSize {
		obj, err = c.fsys.store.PutParts(ctx, c.key, c.partsLocked(partSize), nil, c.etag)
	} else {
		obj, err = c.fsys.store.Put(ctx, c.key, io.NewSectionReader(c.file, 0, c.size), nil, c.etag)
	}
	if err != nil {
		return Attr{}, err
	}
	c.etag = obj.ETag
	c.baseVersion, c.baseSize = obj.VersionID, c.size
	c.touched.clear()
	c.dirty = false
	c.truncated = false
	c.updateSharedCacheLocked(obj.ETag)
	return c.fsys.statFile(ctx, c.key)
}

// partsLocked splits the scratch file into parts of partSize for a
// multipart write: a part wholly inside the base version and outside the
// touched ranges is copied on the server from that version, and any other
// part is uploaded. So a few changed bytes of a large file upload one part.
// The copies are pinned to the base version; whether it's still current is
// checked when the parts are assembled (If-Match), and a lost race rebases
// like any other.
func (c *content) partsLocked(partSize int64) []objstore.Part {
	var parts []objstore.Part
	for off := int64(0); off < c.size; off += partSize {
		n := min(partSize, c.size-off)
		end := off + n
		untouched := !c.touched.overlaps(off, end)
		if c.baseVersion != "" && end <= c.baseSize && untouched {
			parts = append(parts, objstore.Part{Size: n, CopyVersion: c.baseVersion, CopyOffset: off})
		} else {
			parts = append(parts, objstore.Part{Size: n, Body: io.NewSectionReader(c.file, off, n)})
		}
	}
	return parts
}

// updateSharedCacheLocked puts what was just written in the shared cache,
// so the next Open of this key needn't fetch it. Best effort: a failure
// only costs that Open a fetch.
func (c *content) updateSharedCacheLocked(newETag string) {
	if c.file == nil {
		return
	}
	_ = c.fsys.cache.updateSharedFrom(c.key, newETag, c.file, c.size)
}

// TryAcquireLockRange, TryAcquireSharedLockRange, AcquireLockRange,
// AcquireSharedLockRange, ReleaseLockRange, UnlockRange and
// FindConflictingLockRange are
// the Filesystem methods of the same names, on this file's key, for FUSE's
// flock and fcntl, which work per open file.
func (o *OpenFile) TryAcquireLockRange(ctx context.Context, start, end int64, holder string, ttl time.Duration) error {
	return o.fsys.TryAcquireLockRange(ctx, o.key, start, end, holder, ttl)
}

func (o *OpenFile) TryAcquireSharedLockRange(ctx context.Context, start, end int64, holder string, ttl time.Duration) error {
	return o.fsys.TryAcquireSharedLockRange(ctx, o.key, start, end, holder, ttl)
}

func (o *OpenFile) AcquireLockRange(ctx context.Context, start, end int64, holder string, ttl time.Duration) error {
	return o.fsys.AcquireLockRange(ctx, o.key, start, end, holder, ttl)
}

func (o *OpenFile) AcquireSharedLockRange(ctx context.Context, start, end int64, holder string, ttl time.Duration) error {
	return o.fsys.AcquireSharedLockRange(ctx, o.key, start, end, holder, ttl)
}

func (o *OpenFile) ReleaseLockRange(ctx context.Context, start, end int64, holder string) error {
	return o.fsys.ReleaseLockRange(ctx, o.key, start, end, holder)
}

// LockHolder is Filesystem.LockHolder, for the access layers' lock owners.
func (o *OpenFile) LockHolder(local string) string { return o.fsys.LockHolder(local) }

func (o *OpenFile) UnlockRange(ctx context.Context, start, end int64, holder string) error {
	return o.fsys.UnlockRange(ctx, o.key, start, end, holder)
}

func (o *OpenFile) RenewLockRange(ctx context.Context, start, end int64, holder string, ttl time.Duration) error {
	return o.fsys.RenewLockRange(ctx, o.key, start, end, holder, ttl)
}

func (o *OpenFile) FindConflictingLockRange(ctx context.Context, start, end int64, selfHolder string, querySharedType bool) (LockRangeInfo, bool, error) {
	return o.fsys.FindConflictingLockRange(ctx, o.key, start, end, selfHolder, querySharedType)
}

// LockRangesHeldBy is Filesystem.LockRangesHeldBy on this file's key, for
// lease renewal: what the holder holds now, after any merges and splits.
func (o *OpenFile) LockRangesHeldBy(ctx context.Context, holder string) ([]LockRangeInfo, error) {
	return o.fsys.LockRangesHeldBy(ctx, o.key, holder)
}
