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
// retrying a lost CAS race before escalating to locking (task B5). A
// time budget, not an attempt count, per ROADMAP.md's task B1: a large
// file's retry means resending its whole body, so a fixed attempt
// count would give large and small files very different real retry
// windows — a time budget gives them comparable ones.
//
// A var, not a const: internal/icbfs's own tests shrink it to force
// deterministic, fast escalation under simulated contention rather
// than relying on timing-sensitive luck within the real 5s budget.
var contentWriteRetryBudget = 5 * time.Second

// lockEscalationLeaseTTL is the lease length Flush's escalation path
// (task B5) requests when it grabs a lock to guarantee its final write
// attempt. Long enough to comfortably cover one read-modify-write round
// trip even under load; per ARCHITECTURE.md's Locking section ("30+
// seconds"), not tuned down just because this usage is normally brief —
// Flush always releases promptly on its own once done, so a generous
// TTL only matters if a holder crashes mid-escalation.
const lockEscalationLeaseTTL = 30 * time.Second

// ErrWriteContention is returned by OpenFile.Flush when even task B5's
// lock-escalation attempt fails to land a final write (e.g. the ctx
// passed to Flush is done before the escalation lock could be
// acquired). Plain CAS-retry-budget exhaustion alone no longer
// surfaces this directly — it now triggers escalation instead, per
// ARCHITECTURE.md's Locking section and ROADMAP.md's task B5.
var ErrWriteContention = errors.New("content write: exceeded retry budget and lock escalation under contention")

// noTouch marks touchedStart as "nothing written yet this session" —
// touchedEnd alone can't carry that meaning, since 0 is also a
// perfectly real end for a write at offset 0.
const noTouch = -1

// OpenFile buffers one open file's in-progress edits and flushes them
// back as a single whole-object CAS write, retried-and-reapplied
// against the store's current content on a lost race (ARCHITECTURE.md's
// Locking section, ROADMAP.md's task B1).
//
// This type lives in internal/icbfs rather than in the FUSE access
// layer deliberately: ROADMAP.md's task B5 requires the eventual
// content-write-retry-then-lock-escalation logic to be implemented once
// and shared by every access layer (FUSE today, WinFsp eventually), not
// copy-pasted per driver. Putting the buffering/retry machinery itself
// here, not just the future escalation step, means a WinFsp driver gets
// both for free just by using OpenFile the same way FUSE's FileHandle
// does.
//
// Content lives on local disk (a private scratch file, ROADMAP.md's
// Part G, task G3), not as an in-memory []byte — the prior design's
// Open() always materialized a file's entire content as one Go slice,
// which was the root cause of three real crash/OOM bugs this project
// found and fixed the hard way running pjdfstest against a real,
// memory-constrained mount (see maxSetAttrSize's own doc comment).
// This scratch file is private to this one OpenFile session, never
// shared with any other concurrent session of the same key — sharing
// one physical file across sessions would let one session's
// uncommitted edits leak into another's before Flush, breaking the
// isolation task B1's CAS-retry-and-reapply correctness depends on.
// A shared, LRU-bounded cache of validated snapshots (diskCache) sits
// below this, purely to avoid redundant network fetches across
// separate opens of the same unchanged content — never mutated by an
// in-progress session, only ever replaced wholesale after a
// successful Flush.
type OpenFile struct {
	fsys   *Filesystem
	key    string
	holder string // this session's unique identity for escalation locks (task B5)

	mu    sync.Mutex
	file  *os.File // private local scratch file — see type doc comment
	size  int64
	etag  string
	dirty bool

	// touchedStart/touchedEnd track the single contiguous span this
	// session's writes have touched, in O(1) space — not a log of
	// every individual write's own bytes (ROADMAP.md's Part G, task
	// G3's own real finding: an earlier version of this kept every
	// write's data in a separate in-memory slice "for replay on a
	// rebase," which defeated this entire task's point for exactly
	// the case it was built for — one large sequential write ended up
	// holding the whole thing in RAM a second time, in the log,
	// even though the scratch file itself never did). applyLocked
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
}

// newOpenFileHolder mints a unique per-session identity for Flush's
// lock-escalation path (task B5) — distinct from the UUIDs newKey
// mints for store objects, but drawing from the same generator since
// both just need a unique opaque string.
func newOpenFileHolder() (string, error) {
	return NewUUIDv7()
}

// Open returns an OpenFile seeded with key's current content and ETag,
// for a caller about to Read/Write/Flush it (a FUSE Open, for
// instance). The scratch file is seeded via the shared disk cache
// (ROADMAP.md's Part G, task G3): a cheap Head call learns the
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
	return &OpenFile{fsys: f, key: key, holder: holder, file: file, size: size, etag: head.ETag, touchedStart: noTouch}, nil
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
		return &OpenFile{fsys: f, key: key, holder: holder, etag: etag, touchedStart: noTouch}
	}
	if len(data) > 0 {
		if _, err := file.Write(data); err == nil {
			_, _ = file.Seek(0, io.SeekStart)
		}
	}
	return &OpenFile{fsys: f, key: key, holder: holder, file: file, size: int64(len(data)), etag: etag, touchedStart: noTouch}
}

// Close releases this session's private local scratch file — callers
// (fuseserver's FileHandle.Release, winfspserver's equivalent
// handle-close path) must call this exactly once when the open
// session genuinely ends, or the scratch file leaks on local disk for
// the lifetime of the process. Safe to call on an OpenFile whose
// scratch file failed to create (NewOpenFile's degraded path above).
func (o *OpenFile) Close() error {
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

// Size returns the current (post-edit, pre-flush) content length.
func (o *OpenFile) Size() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.size
}

// TouchAtime bumps this file's recorded last-access time to now
// (ROADMAP.md's Part G, task G1) — a thin forward to Filesystem's own
// TouchAtime, keeping o.fsys/o.key unexported from access-layer
// callers (fuseserver, winfspserver) the same way every other
// OpenFile method already does.
//
// At most once per session, not once per call: the kernel's own
// max_read-style chunking splits one logical application read(2) into
// many smaller FUSE Read callback invocations (confirmed via a real
// hang found testing task G3 — a single 500 MB read turned into
// thousands of synchronous atime-metadata network PUTs, one per ~128
// KB chunk, each blocking the read that triggered it). "Strict, not
// relatime" (task G1's own explicit decision) means every real access
// updates atime immediately rather than deferring/approximating it —
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
	return nil
}

// applyLocked extends the local scratch file to cover off+len(data) if
// the write goes past the current end, then writes data in at off.
//
// A negative offset, or an end that exceeds maxSetAttrSize, is
// rejected before ever reaching Truncate — maxSetAttrSize's own bound
// predates this task (it was sized against the old make([]byte, end)
// panic/OOM this design no longer has), but it's kept as the same
// real ceiling regardless: extending a *sparse* local file past this
// bound would still make Flush's eventual whole-object Put (task G3
// deliberately keeps the backend protocol whole-object, not
// range/multipart) try to upload an enormous object, which is exactly
// as real a problem as the original in-memory allocation was.
func (o *OpenFile) applyLocked(data []byte, off int64) error {
	if off < 0 {
		return ErrInvalidArgument
	}
	end := off + int64(len(data))
	if end > maxSetAttrSize {
		return ErrInvalidArgument
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
// into one span is accepted here, same reasoning ARCHITECTURE.md
// gives for task B6's diff-based enforcement: a wider lock-escalation
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
	if o.touchedStart != noTouch && o.file != nil {
		ownEdits = make([]byte, o.touchedEnd-o.touchedStart)
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
	if oldName != "" {
		_ = os.Remove(oldName)
	}
	o.etag = head.ETag
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
// the scenario ARCHITECTURE.md describes as today's lost-update bug —
// the same property an explicit per-write edit log would have given,
// without that log's own unbounded-RAM cost for a large sequential
// write (see the OpenFile type doc comment).
//
// Bounded by contentWriteRetryBudget, not an attempt count (see its doc
// comment). Once that budget is exhausted, Flush escalates (task B5,
// connecting task B1 to tasks B2-B4): it blocks (task B3) on a lock
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

	// Task B6: checked before every write, not just on retry
	// exhaustion — refuse outright if the touched range overlaps a
	// real, external application lock (fcntl/LockFileEx), rather than
	// attempting the write and only finding out indirectly via a CAS
	// conflict (which wouldn't distinguish "someone else's cooperating
	// lock" from ordinary same-codebase contention B1/B5 already
	// handle on their own).
	start, end := o.touchedRangeLocked()
	if err := o.fsys.CheckRangeLockConflict(ctx, o.key, start, end, o.holder); err != nil {
		return Attr{}, err
	}

	deadline := time.Now().Add(contentWriteRetryBudget)
	for {
		attr, newETag, err := o.fsys.writeFileFrom(ctx, o.key, io.NewSectionReader(o.file, 0, o.size), o.etag)
		if err == nil {
			o.etag = newETag
			o.touchedStart = noTouch
			o.touchedEnd = 0
			o.dirty = false
			o.updateSharedCacheLocked(newETag)
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
		// Task B7: escalation itself uses the Locking feature (the
		// same .lock object, task B5), so a mount that opted out of
		// Locking's costs doesn't get it either — plain CAS-retry
		// exhaustion is simply terminal, exactly as it was before
		// task B5 existed.
		return Attr{}, ErrWriteContention
	}
	if err := o.fsys.acquireEscalationLockRange(ctx, o.key, start, end, o.holder, lockEscalationLeaseTTL); err != nil {
		return Attr{}, ErrWriteContention
	}
	defer func() { _ = o.fsys.ReleaseLockRange(context.WithoutCancel(ctx), o.key, start, end, o.holder) }()

	if err := o.rebaseLocked(ctx); err != nil {
		return Attr{}, err
	}
	attr, newETag, err := o.fsys.writeFileFrom(ctx, o.key, io.NewSectionReader(o.file, 0, o.size), o.etag)
	if err != nil {
		return Attr{}, err
	}
	o.etag = newETag
	o.touchedStart = noTouch
	o.touchedEnd = 0
	o.dirty = false
	o.updateSharedCacheLocked(newETag)
	return attr, nil
}

// updateSharedCacheLocked keeps the shared disk cache representing
// what was actually just written (ROADMAP.md's Part G, task G3), so
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
// Filesystem methods, fixed to this OpenFile's own key — task B8's
// FUSE flock/fcntl wiring (and any future WinFsp equivalent) operates
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
// lease renewal (task B8's fix) without separately tracking what an
// earlier acquire call originally requested.
func (o *OpenFile) LockRangesHeldBy(ctx context.Context, holder string) ([]LockRangeInfo, error) {
	return o.fsys.LockRangesHeldBy(ctx, o.key, holder)
}
