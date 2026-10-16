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

// noTouch marks touchedStart as "nothing written yet this session" —
// touchedEnd alone can't carry that meaning, since 0 is also a
// perfectly real end for a write at offset 0.
const noTouch = -1

// OpenFile is one open session of a file: its edits, kept in a private
// local scratch file, and flushed back as one conditional whole-object
// write that, on a lost race, is re-applied onto the current content
// (docs/architecture.md's Concurrency section). It lives here, not in an
// access layer, so FUSE and WinFsp share the retry and escalation logic.
//
// The scratch file belongs to this session alone: sharing one between
// sessions would let one's unflushed edits leak into another's, breaking
// the re-apply. Below it, a shared LRU cache of fetched versions
// (diskCache) saves re-downloading unchanged content; sessions never modify
// it, and a successful Flush replaces its entry.
type OpenFile struct {
	fsys   *Filesystem
	key    string
	holder string // this session's unique identity for escalation locks

	mu    sync.Mutex
	file  *os.File // private local scratch file — see type doc comment
	size  int64
	etag  string
	dirty bool

	// baseVersion and baseSize identify the stored version the scratch file
	// was seeded from (etag is its ETag). Outside the touched range, the
	// scratch file still holds exactly those bytes, so a multipart flush
	// can copy those ranges on the server (partsLocked). Empty when
	// unknown, as right after Create; then nothing is copied.
	baseVersion string
	baseSize    int64

	// touchedStart and touchedEnd span every byte this session wrote. The
	// writes themselves are already merged in the scratch file, in order,
	// so a rebase needs only this range read back from it, not a log of
	// writes.
	touchedStart, touchedEnd int64

	// atimeTouched guards TouchAtime's at-most-once-per-session
	// behavior — see its own doc comment.
	atimeTouched bool

	// truncated is set when Truncate has fixed this session's size since
	// its last flush, so a rebase onto newer content keeps that size
	// instead of the base's.
	truncated bool

	// lastWrite is when WriteAt last changed this session's content —
	// the mtime Stat reports for it until it's flushed (see
	// Filesystem.localOverlay).
	lastWrite time.Time
}

// newOpenFileHolder mints a unique identity for this session's escalation
// locks.
func newOpenFileHolder() (string, error) {
	return NewUUIDv7()
}

// Open returns an OpenFile seeded with key's current content and ETag. A
// Head learns the current ETag; only a cache miss on it fetches the
// content.
func (f *Filesystem) Open(ctx context.Context, key string) (*OpenFile, error) {
	head, err := f.store.Head(ctx, key)
	if err != nil {
		return nil, mapNotFound(err)
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
	holder, err := newOpenFileHolder()
	if err != nil {
		file.Close()
		return nil, err
	}
	o := &OpenFile{fsys: f, key: key, holder: holder, file: file, size: size, etag: head.ETag,
		baseVersion: head.VersionID, baseSize: size, touchedStart: noTouch}
	f.holdOpen(o)
	return o, nil
}

// NewOpenFile seeds an OpenFile from content the caller just wrote (its
// key, ETag and data; right after Create, nothing), with no read back.
func (f *Filesystem) NewOpenFile(key, etag string, data []byte) *OpenFile {
	holder, err := newOpenFileHolder()
	if err != nil {
		// Only if the system's random source fails. A fixed holder means
		// two such sessions could share a lock identity while escalating;
		// nothing else is affected.
		holder = "unknown-rng-failure"
	}
	file, err := f.cache.newScratchFile()
	if err != nil {
		// Only if a local temp file can't be created. The OpenFile then
		// fails on first use, rather than every caller of Create and Mknod
		// having to handle an error here.
		o := &OpenFile{fsys: f, key: key, holder: holder, etag: etag, touchedStart: noTouch}
		f.holdOpen(o)
		return o
	}
	if len(data) > 0 {
		if _, err := file.Write(data); err == nil {
			_, _ = file.Seek(0, io.SeekStart)
		}
	}
	o := &OpenFile{fsys: f, key: key, holder: holder, file: file, size: int64(len(data)), etag: etag,
		baseSize: int64(len(data)), touchedStart: noTouch}
	f.holdOpen(o)
	return o
}

// Close releases this session's scratch file. Access layers call it exactly
// once, when the handle closes; it's safe on an OpenFile whose scratch file
// couldn't be created.
//
// The last Close of a file unlinked while open deletes its objects (see
// openSessions).
func (o *OpenFile) Close() error {
	defer o.fsys.releaseOpen(o)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil {
		return nil
	}
	name := o.file.Name()
	err := o.file.Close()
	_ = os.Remove(name)
	o.file = nil
	return err
}

// pending reports this session's unflushed state: its current size, when
// it last wrote, and whether anything is waiting to be flushed.
func (o *OpenFile) pending() (size int64, lastWrite time.Time, dirty bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.size, o.lastWrite, o.dirty
}

// Size returns the current (post-edit, pre-flush) content length.
func (o *OpenFile) Size() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.size
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

// ReadAt returns up to length bytes at off from the scratch file: this
// session's own edits included, before Flush makes them visible to anyone
// else.
func (o *OpenFile) ReadAt(off, length int64) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil || off >= o.size || length <= 0 {
		return nil
	}
	end := off + length
	if end > o.size {
		end = o.size
	}
	out := make([]byte, end-off)
	n, err := o.file.ReadAt(out, off)
	if err != nil && err != io.EOF {
		return nil
	}
	return out[:n]
}

// WriteAt writes data at off into the scratch file, and widens the touched
// range Flush re-applies after a lost race.
//
// A negative offset is ErrInvalidArgument, and an end past the max file
// size ErrFileTooLarge (see applyLocked).
func (o *OpenFile) WriteAt(data []byte, off int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.applyLocked(data, off); err != nil {
		return err
	}
	end := off + int64(len(data))
	if o.touchedStart == noTouch || off < o.touchedStart {
		o.touchedStart = off
	}
	if end > o.touchedEnd {
		o.touchedEnd = end
	}
	o.dirty = true
	o.lastWrite = time.Now()
	return nil
}

// Truncate sets this session's content length, as ftruncate(2) does: a
// shrink drops the tail, a grow extends with zeros (sparsely, on the
// local scratch file). Like WriteAt, it's flushed by Flush.
func (o *OpenFile) Truncate(size int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if size < 0 || o.file == nil {
		return ErrInvalidArgument
	}
	if size > o.fsys.maxFileSize {
		return ErrFileTooLarge
	}
	if size == o.size {
		return nil
	}
	return o.resizeLocked(size)
}

// fallocate(2)'s modes. FUSE passes on only these; the kernel refuses
// collapsing and inserting ranges itself.
const (
	FallocKeepSize  = 0x01
	FallocPunchHole = 0x02
	FallocZeroRange = 0x10
)

// Allocate is fallocate(2) on this session's content, flushed as a write
// is. Content is stored whole, zeros included, so there's no space to
// reserve: a plain allocation only grows the size (unless FallocKeepSize),
// and punching a hole and zeroing a range write zeros. Any other mode is
// ErrNotSupported.
func (o *OpenFile) Allocate(off, length int64, mode uint32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if off < 0 || length <= 0 || o.file == nil {
		return ErrInvalidArgument
	}
	end := off + length
	if end < off {
		return ErrFileTooLarge
	}
	keep := mode&FallocKeepSize != 0
	grow := func(size int64) error {
		if size > o.fsys.maxFileSize {
			return ErrFileTooLarge
		}
		if keep || size <= o.size {
			return nil
		}
		return o.resizeLocked(size)
	}
	switch mode &^ FallocKeepSize {
	case 0:
		return grow(end)
	case FallocPunchHole:
		if !keep {
			return ErrInvalidArgument // as Linux: a hole never changes the size
		}
		return o.zeroLocked(off, min(end, o.size))
	case FallocZeroRange:
		if err := grow(end); err != nil {
			return err
		}
		return o.zeroLocked(off, min(end, o.size))
	default:
		return ErrNotSupported
	}
}

// resizeLocked sets the scratch file's size, as Truncate does.
func (o *OpenFile) resizeLocked(size int64) error {
	if err := o.file.Truncate(size); err != nil {
		return err
	}
	o.touchLocked(min(size, o.size), max(size, o.size))
	o.size = size
	o.truncated = true
	return nil
}

// zeroLocked writes zeros over [lo, hi).
func (o *OpenFile) zeroLocked(lo, hi int64) error {
	if lo >= hi {
		return nil
	}
	zeros := make([]byte, min(hi-lo, 1<<20))
	for at := lo; at < hi; {
		n := min(hi-at, int64(len(zeros)))
		if _, err := o.file.WriteAt(zeros[:n], at); err != nil {
			return err
		}
		at += n
	}
	o.touchLocked(lo, hi)
	return nil
}

// touchLocked widens the touched range to cover [lo, hi) and marks the
// session dirty, as WriteAt does.
func (o *OpenFile) touchLocked(lo, hi int64) {
	if o.touchedStart == noTouch || lo < o.touchedStart {
		o.touchedStart = lo
	}
	if hi > o.touchedEnd {
		o.touchedEnd = hi
	}
	o.dirty = true
	o.lastWrite = time.Now()
}

// applyLocked extends the local scratch file to cover off+len(data) if
// the write goes past the current end, then writes data in at off.
//
// A negative offset is invalid; an end past the filesystem's max file
// size is ErrFileTooLarge (see DefaultMaxFileSize), checked before the
// scratch file is extended.
func (o *OpenFile) applyLocked(data []byte, off int64) error {
	if off < 0 {
		return ErrInvalidArgument
	}
	end := off + int64(len(data))
	if end > o.fsys.maxFileSize {
		return ErrFileTooLarge
	}
	if o.file == nil {
		return ErrInvalidArgument
	}
	if end > o.size {
		if err := o.file.Truncate(end); err != nil {
			return err
		}
		o.size = end
	}
	if len(data) > 0 {
		if _, err := o.file.WriteAt(data, off); err != nil {
			return err
		}
	}
	return nil
}

// touchedRangeLocked returns the span this session's writes touched.
// Merging separate edits into one span over-approximates, which can only
// make a lock check stricter, never miss a conflict. It's called only when
// dirty, which WriteAt never sets without widening the range.
func (o *OpenFile) touchedRangeLocked() (start, end int64) {
	return o.touchedStart, o.touchedEnd
}

// rebaseLocked fetches key's current content into a fresh scratch file
// (through the cache, as Open does), copies this session's touched range
// onto it from the old scratch file, and updates o.etag.
func (o *OpenFile) rebaseLocked(ctx context.Context) error {
	var ownEdits []byte
	ownSize := o.size
	if o.touchedStart != noTouch && o.file != nil && o.touchedStart < ownSize {
		// Only what's still inside this session's content: past its end
		// is a truncated-away tail, which the size below takes care of.
		ownEdits = make([]byte, min(o.touchedEnd, ownSize)-o.touchedStart)
		if _, err := o.file.ReadAt(ownEdits, o.touchedStart); err != nil && err != io.EOF {
			return err
		}
	}

	head, err := o.fsys.store.Head(ctx, o.key)
	if err != nil {
		return mapNotFound(err)
	}
	fetch := func(ctx context.Context) (io.ReadCloser, error) {
		body, _, err := o.fsys.store.Get(ctx, o.key)
		if err != nil {
			return nil, mapNotFound(err)
		}
		return body, nil
	}
	fresh, size, err := o.fsys.cache.seedScratch(ctx, o.key, head.ETag, fetch)
	if err != nil {
		return err
	}
	oldName := ""
	if o.file != nil {
		oldName = o.file.Name()
		o.file.Close()
	}
	o.file = fresh
	o.size = size
	if ownEdits != nil {
		if err := o.applyLocked(ownEdits, o.touchedStart); err != nil {
			if oldName != "" {
				_ = os.Remove(oldName)
			}
			return err
		}
	}
	if o.truncated && o.size != ownSize {
		if err := o.file.Truncate(ownSize); err != nil {
			if oldName != "" {
				_ = os.Remove(oldName)
			}
			return err
		}
		o.size = ownSize
	}
	if oldName != "" {
		_ = os.Remove(oldName)
	}
	o.etag = head.ETag
	o.baseVersion, o.baseSize = head.VersionID, size
	return nil
}

// Flush writes the session's edits back as one whole-object write,
// conditional on the ETag the scratch file is based on.
//
// On a lost race the scratch file can't just be resent: it's built on
// content that's no longer current, and would erase the other writer's
// change. Flush instead rebases: fetches the current content, copies its
// own touched range onto it, and tries again. Two writers editing different
// ranges both land, as on a local disk.
//
// Retrying is bounded by contentWriteRetryBudget. After that, with locking
// on, Flush escalates: it takes a lock on its touched range, makes one more
// conditional write while holding it, and releases it. Escalating writers
// take turns on the lock instead of burning retries against each other, so
// each one gets its write in.
func (o *OpenFile) Flush(ctx context.Context) (Attr, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.dirty {
		return o.fsys.statFile(ctx, o.key)
	}
	if o.fsys.isOrphan(o.key) {
		// Unlinked while open: nothing can see this content any more,
		// and its objects go at the last Close. Drop the writes.
		o.dirty = false
		o.truncated = false
		o.touchedStart, o.touchedEnd = noTouch, 0
		return o.fsys.statFile(ctx, o.key)
	}

	// Checked before every write: refuse at once if the touched range
	// overlaps another holder's application lock (fcntl, LockFileEx). A
	// lost race alone couldn't tell that apart from ordinary contention,
	// which retry and escalation handle.
	start, end := o.touchedRangeLocked()
	if err := o.fsys.CheckRangeLockConflict(ctx, o.key, start, end, o.holder); err != nil {
		return Attr{}, err
	}

	deadline := time.Now().Add(contentWriteRetryBudget)
	for {
		attr, err := o.uploadLocked(ctx)
		if err == nil {
			return attr, nil
		}
		if !objstore.IsPreconditionFailed(err) {
			return Attr{}, err
		}
		if time.Now().After(deadline) {
			break
		}
		if err := o.rebaseLocked(ctx); err != nil {
			return Attr{}, err
		}
	}

	if !o.fsys.lockingEnabled {
		// Escalation needs locking, so without it running out of retries is
		// final.
		return Attr{}, ErrWriteContention
	}
	if err := o.fsys.acquireEscalationLockRange(ctx, o.key, start, end, o.holder, lockEscalationLeaseTTL); err != nil {
		return Attr{}, ErrWriteContention
	}
	defer func() { _ = o.fsys.ReleaseLockRange(context.WithoutCancel(ctx), o.key, start, end, o.holder) }()

	if err := o.rebaseLocked(ctx); err != nil {
		return Attr{}, err
	}
	return o.uploadLocked(ctx)
}

// uploadLocked writes the scratch file back to the store, conditioned on
// the ETag it's based on: one Put for a file that fits in one part, and
// otherwise a multipart write (partsLocked) that only uploads the parts
// this session changed. On success the session is clean and based on
// the version just written.
func (o *OpenFile) uploadLocked(ctx context.Context) (Attr, error) {
	if err := o.fsys.checkWritable(); err != nil {
		return Attr{}, err
	}
	var obj *objstore.Object
	var err error
	if partSize := o.fsys.partSize(); o.size > partSize {
		obj, err = o.fsys.store.PutParts(ctx, o.key, o.partsLocked(partSize), nil, o.etag)
	} else {
		obj, err = o.fsys.store.Put(ctx, o.key, io.NewSectionReader(o.file, 0, o.size), nil, o.etag)
	}
	if err != nil {
		return Attr{}, err
	}
	o.etag = obj.ETag
	o.baseVersion, o.baseSize = obj.VersionID, o.size
	o.touchedStart = noTouch
	o.touchedEnd = 0
	o.dirty = false
	o.truncated = false
	o.updateSharedCacheLocked(obj.ETag)
	return o.fsys.statFile(ctx, o.key)
}

// partsLocked splits the scratch file into parts of partSize for a
// multipart write: a part wholly inside the base version and outside the
// touched range is copied on the server from that version, and any other
// part is uploaded. So a few changed bytes of a large file upload one part.
// The copies are pinned to the base version; whether it's still current is
// checked when the parts are assembled (If-Match), and a lost race rebases
// like any other.
func (o *OpenFile) partsLocked(partSize int64) []objstore.Part {
	var parts []objstore.Part
	for off := int64(0); off < o.size; off += partSize {
		n := min(partSize, o.size-off)
		end := off + n
		untouched := o.touchedStart == noTouch || end <= o.touchedStart || off >= o.touchedEnd
		if o.baseVersion != "" && end <= o.baseSize && untouched {
			parts = append(parts, objstore.Part{Size: n, CopyVersion: o.baseVersion, CopyOffset: off})
		} else {
			parts = append(parts, objstore.Part{Size: n, Body: io.NewSectionReader(o.file, off, n)})
		}
	}
	return parts
}

// updateSharedCacheLocked puts what was just written in the shared cache,
// so the next Open of this key needn't fetch it. Best effort: a failure
// only costs that Open a fetch.
func (o *OpenFile) updateSharedCacheLocked(newETag string) {
	if o.file == nil {
		return
	}
	_ = o.fsys.cache.updateSharedFrom(o.key, newETag, o.file, o.size)
}

// TryAcquireLockRange, TryAcquireSharedLockRange, AcquireLockRange,
// AcquireSharedLockRange, ReleaseLockRange and FindConflictingLockRange are
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
