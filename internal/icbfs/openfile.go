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

// contentWriteRetryBudget bounds how long OpenFile.Flush spends
// retrying a lost CAS race before escalating to locking. A
// time budget, not an attempt count: a large
// file's retry means resending its whole body, so a fixed attempt
// count would give large and small files very different real retry
// windows — a time budget gives them comparable ones.
//
// A var, not a const: internal/icbfs's own tests shrink it to force
// deterministic, fast escalation under simulated contention rather
// than relying on timing-sensitive luck within the real 5s budget.
var contentWriteRetryBudget = 5 * time.Second

// lockEscalationLeaseTTL is the lease length Flush's escalation path
// requests when it grabs a lock to guarantee its final write
// attempt. Long enough to comfortably cover one read-modify-write round
// trip even under load; per docs/architecture.md's Locking section ("30+
// seconds"), not tuned down just because this usage is normally brief —
// Flush always releases promptly on its own once done, so a generous
// TTL only matters if a holder crashes mid-escalation.
const lockEscalationLeaseTTL = 30 * time.Second

// ErrWriteContention is returned by OpenFile.Flush when even the
// lock-escalation attempt fails to land a final write (e.g. the ctx
// passed to Flush is done before the escalation lock could be
// acquired), or when the retry budget runs out on a mount without
// locking. With locking on, retry-budget exhaustion triggers
// escalation instead (docs/architecture.md's Concurrency section).
var ErrWriteContention = errors.New("content write: exceeded retry budget and lock escalation under contention")

// noTouch marks touchedStart as "nothing written yet this session" —
// touchedEnd alone can't carry that meaning, since 0 is also a
// perfectly real end for a write at offset 0.
const noTouch = -1

// OpenFile buffers one open file's in-progress edits and flushes them
// back as a single whole-object CAS write, retried-and-reapplied
// against the store's current content on a lost race
// (docs/architecture.md's Concurrency section).
//
// This type lives in internal/icbfs rather than in an access layer
// deliberately: the content-write-retry-then-lock-escalation logic is
// implemented once and shared by every access layer (FUSE and WinFsp),
// not copy-pasted per driver.
//
// Content lives on local disk (a private scratch file), not as an
// in-memory []byte, so a file's size is bounded by local disk, not RAM.
// This scratch file is private to this one OpenFile session, never
// shared with any other concurrent session of the same key — sharing
// one physical file across sessions would let one session's
// uncommitted edits leak into another's before Flush, breaking the
// isolation CAS-retry-and-reapply's correctness depends on.
// A shared, LRU-bounded cache of validated snapshots (diskCache) sits
// below this, purely to avoid redundant network fetches across
// separate opens of the same unchanged content — never mutated by an
// in-progress session, only ever replaced wholesale after a
// successful Flush.
type OpenFile struct {
	fsys   *Filesystem
	key    string
	holder string // this session's unique identity for escalation locks

	mu    sync.Mutex
	file  *os.File // private local scratch file — see type doc comment
	size  int64
	etag  string
	dirty bool

	// baseVersion and baseSize identify the stored version the scratch
	// file was seeded from (etag is its ETag): outside the touched range
	// below, the scratch file still holds exactly its bytes, which is
	// what lets a multipart flush copy those ranges server-side instead
	// of uploading them (see partsLocked). baseVersion is empty when
	// unknown (a session seeded right after Create), and then nothing is
	// copied.
	baseVersion string
	baseSize    int64

	// touchedStart/touchedEnd track the single contiguous span this
	// session's writes have touched, in O(1) space — not a log of
	// every individual write's own bytes. applyLocked
	// already writes every edit directly onto the one shared scratch
	// file, in call order, so a later overlapping write already wins
	// over an earlier one there, exactly as a replayed log would
	// produce — rebaseLocked only ever needs this session's current,
	// already-correctly-merged bytes over this one range, read once
	// from the scratch file, not a sequence of smaller ops to replay.
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

// newOpenFileHolder mints a unique per-session identity for Flush's
// lock-escalation path — distinct from the UUIDs newKey
// mints for store objects, but drawing from the same generator since
// both just need a unique opaque string.
func newOpenFileHolder() (string, error) {
	return NewUUIDv7()
}

// Open returns an OpenFile seeded with key's current content and ETag,
// for a caller about to Read/Write/Flush it (a FUSE Open, for
// instance). The scratch file is seeded via the shared disk cache:
// a cheap Head call learns the
// current ETag, and only a cache miss against it costs a real content
// fetch.
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

// NewOpenFile seeds an OpenFile directly from a just-created content
// object's key/ETag/data (e.g. right after Create), avoiding a
// redundant read-back of what the caller just wrote. data is small
// (Create always seeds an empty object; a just-created file has no
// content yet) and already in hand, so this writes it straight into a
// fresh private scratch file rather than routing through the shared
// cache's own fetch path — there's nothing to fetch.
func (f *Filesystem) NewOpenFile(key, etag string, data []byte) *OpenFile {
	holder, err := newOpenFileHolder()
	if err != nil {
		// Astronomically unlikely (uuid.NewV7 only fails if the
		// system's crypto RNG is broken) — fall back to a fixed
		// placeholder rather than making NewOpenFile fallible for
		// every caller over this. Worst case, two such sessions in
		// the same unlikely failure mode would briefly share a lock
		// identity during escalation; the ordinary, non-escalated
		// write path is completely unaffected either way.
		holder = "unknown-rng-failure"
	}
	file, err := f.cache.newScratchFile()
	if err != nil {
		// No sensible recovery for "can't create a local temp file"
		// short of making this constructor fallible, which would
		// ripple into every caller (Create, Mknod, ...) for a
		// condition this project already treats as effectively
		// impossible elsewhere (e.g. the RNG failure above) — a
		// locally-inconsistent OpenFile that fails on its first real
		// use is an acceptable, contained consequence.
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

// Close releases this session's private local scratch file — callers
// (fuseserver's FileHandle.Release, winfspserver's equivalent
// handle-close path) must call this exactly once when the open
// session genuinely ends, or the scratch file leaks on local disk for
// the lifetime of the process. Safe to call on an OpenFile whose
// scratch file failed to create (NewOpenFile's degraded path above).
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

// TouchAtime bumps this file's recorded last-access time to now
// — a thin forward to Filesystem's own
// TouchAtime, keeping o.fsys/o.key unexported from access-layer
// callers (fuseserver, winfspserver) the same way every other
// OpenFile method already does.
//
// At most once per session, not once per call: the kernel's own
// max_read-style chunking splits one logical application read(2) into
// many smaller FUSE Read callback invocations (a single 500 MB read
// would otherwise be thousands of synchronous atime-metadata network
// PUTs, one per ~128 KB chunk, each blocking the read that triggered
// it). "Strict, not relatime" means every real access updates atime
// immediately rather than deferring/approximating it —
// it was never meant to mean every individual kernel-level chunk of
// the *same* ongoing read re-updates it again, which is a real
// performance bug, not a semantics requirement: a real filesystem's
// own strict-atime behavior doesn't do that either.
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

// ReadAt returns up to length bytes starting at off from the current
// (post-edit, pre-flush) local scratch file — this is read-your-own-
// writes within the same open session, before Flush has made any of it
// visible to anyone else.
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

// WriteAt applies an edit at the given offset directly to the local
// scratch file (so a subsequent ReadAt in the same session sees it),
// and widens this session's own tracked touched range so Flush/
// rebaseLocked know what to reapply onto a freshly fetched base if
// this session loses a CAS race — see the OpenFile type doc comment
// for why that's a byte range read back from the scratch file at
// rebase time, not a growing log of every write's own data kept
// separately in RAM.
//
// Returns ErrInvalidArgument for an out-of-range offset/length — see
// applyLocked's own doc comment for why this is a real, necessary
// check here too, found via the same pjdfstest run that found
// SetAttr's identical gap, via a genuinely different code path: a
// *write* at a huge offset (testing sparse-file/32-bit-off_t-overflow
// behavior — pjdfstest's own `open` category does this, not a
// truncate at all), not a truncate/resize request.
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
	if err := o.file.Truncate(size); err != nil {
		return err
	}
	lo, hi := min(size, o.size), max(size, o.size)
	o.size = size
	if o.touchedStart == noTouch || lo < o.touchedStart {
		o.touchedStart = lo
	}
	if hi > o.touchedEnd {
		o.touchedEnd = hi
	}
	o.dirty = true
	o.truncated = true
	o.lastWrite = time.Now()
	return nil
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

// touchedRangeLocked returns the single contiguous span this
// session's writes have touched — over-approximating disjoint edits
// into one span is accepted here, same reasoning docs/architecture.md
// gives for diff-based lock enforcement: a wider lock-escalation
// range can only make a conflict check more likely to serialize
// against something real, never silently miss one. Only ever called
// with dirty == true (Flush's own guard), which WriteAt never sets
// without also widening touchedStart/touchedEnd, so touchedStart is
// never still noTouch here.
func (o *OpenFile) touchedRangeLocked() (start, end int64) {
	return o.touchedStart, o.touchedEnd
}

// rebaseLocked re-fetches key's current content into a fresh scratch
// file (via the same shared-cache-aware path Open uses), then
// reapplies this session's own touched range — read back from the
// *old* scratch file before it's discarded, which already holds every
// one of this session's edits correctly merged (applyLocked always
// wrote each one directly onto the same file, in call order, so a
// later overlapping write already won over an earlier one there) —
// onto the fresh base, and updates o.etag in place.
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

// Flush writes the accumulated edits back as a single whole-object CAS
// write, conditioned on the ETag this session's scratch file is
// currently based on.
//
// On a lost race (the store reports objstore.IsPreconditionFailed), the
// local scratch file can't just be resent as-is — it was built on top
// of content that's no longer current, and resending it unconditio-
// nally would silently discard whatever the other writer just
// committed. Instead, Flush re-fetches the latest content into a fresh
// scratch file and reapplies this session's own touched byte range
// (read back from the old scratch file, which already holds every
// edit in that range correctly merged) onto it, and tries again.
// Reapplying "these bytes at this range" onto a new base preserves
// both writers' edits when they touch different byte ranges, exactly
// the lost-update case docs/architecture.md describes —
// the same property an explicit per-write edit log would have given,
// without that log's own unbounded-RAM cost for a large sequential
// write (see the OpenFile type doc comment).
//
// Bounded by contentWriteRetryBudget, not an attempt count (see its doc
// comment). Once that budget is exhausted, Flush escalates: it blocks
// on a lock
// covering the byte range this session actually touched, then makes
// one final, guaranteed-uncontested-among-escalating-writers CAS write
// attempt while holding it, then releases. This is what guarantees
// every writer eventually makes forward progress under sustained
// contention, rather than every contender thrashing against every
// other contender's retries indefinitely: whichever writer currently
// holds the escalation lock has exclusive access to land its write
// while every other escalating writer is blocked waiting on the lock
// (not burning retries), so each gets its turn. Returns
// ErrWriteContention only if the escalation lock itself couldn't be
// acquired (e.g. ctx was done first).
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

	// Checked before every write, not just on retry
	// exhaustion — refuse outright if the touched range overlaps a
	// real, external application lock (fcntl/LockFileEx), rather than
	// attempting the write and only finding out indirectly via a CAS
	// conflict (which wouldn't distinguish "someone else's cooperating
	// lock" from ordinary contention that retry and escalation
	// handle on their own).
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
		// Escalation itself uses the Locking feature (the same .lock
		// object), so a mount that opted out of Locking's costs
		// doesn't get it either — plain CAS-retry exhaustion is
		// simply terminal.
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
// multipart write: a part wholly inside the base version and outside
// the range this session touched is copied server-side from that
// version; any other part is uploaded from the scratch file. So editing
// a few bytes of a large file uploads one part, not the file. The copies
// are pinned to the base version; whether that's still current is
// checked when the parts are assembled (If-Match), and a lost race
// rebases and tries again like any other flush.
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

// updateSharedCacheLocked keeps the shared disk cache representing
// what was actually just written, so
// the very next Open of this key — from this session or any other —
// doesn't redundantly re-fetch content this process already has
// locally. Best-effort: a failure here never fails the Flush that
// already genuinely succeeded; it only means the next Open won't get
// a cache hit, no worse than if this cache didn't exist.
func (o *OpenFile) updateSharedCacheLocked(newETag string) {
	if o.file == nil {
		return
	}
	_ = o.fsys.cache.updateSharedFrom(o.key, newETag, o.file, o.size)
}

// TryAcquireLockRange, TryAcquireSharedLockRange, AcquireLockRange,
// AcquireSharedLockRange, ReleaseLockRange, and FindConflictingLockRange
// are OpenFile-scoped convenience wrappers over the identically-named
// Filesystem methods, fixed to this OpenFile's own key — FUSE's
// flock/fcntl wiring operates
// per open file, so it's more natural to call these on the OpenFile it
// already has in hand than to separately track fsys/key itself.
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

// LockRangesHeldBy is OpenFile's wrapper over the identically-named
// Filesystem method — see its doc comment. Used to drive background
// lease renewal without separately tracking what an
// earlier acquire call originally requested.
func (o *OpenFile) LockRangesHeldBy(ctx context.Context, holder string) ([]LockRangeInfo, error) {
	return o.fsys.LockRangesHeldBy(ctx, o.key, holder)
}
