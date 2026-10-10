package icbfs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// contentWriteRetryBudget bounds how long OpenFile.Flush spends
// retrying a lost CAS race before giving up. A time budget, not an
// attempt count, per ROADMAP.md's task B1: a large file's retry means
// resending its whole body, so a fixed attempt count would give large
// and small files very different real retry windows — a time budget
// gives them comparable ones.
const contentWriteRetryBudget = 5 * time.Second

// ErrWriteContention is returned by OpenFile.Flush when
// contentWriteRetryBudget is exhausted without a successful write — see
// ARCHITECTURE.md's Locking section and ROADMAP.md's task B5: this is
// the trigger point B5 wires an escalation-to-locking attempt onto,
// once Locking (tasks B2-B4) exists.
var ErrWriteContention = errors.New("content write: exceeded retry budget under contention")

// writeOp is one recorded edit in an OpenFile's edit log — see Flush's
// doc comment for why the log, not just the merged buffer, has to be
// kept.
type writeOp struct {
	offset int64
	data   []byte
}

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
type OpenFile struct {
	fsys *Filesystem
	key  string

	mu    sync.Mutex
	data  []byte
	etag  string
	ops   []writeOp
	dirty bool
}

// Open returns an OpenFile seeded with key's current content and ETag,
// for a caller about to Read/Write/Flush it (a FUSE Open, for
// instance).
func (f *Filesystem) Open(ctx context.Context, key string) (*OpenFile, error) {
	data, _, etag, err := f.ReadFile(ctx, key)
	if err != nil {
		return nil, err
	}
	return &OpenFile{fsys: f, key: key, data: data, etag: etag}, nil
}

// NewOpenFile seeds an OpenFile directly from a just-created content
// object's key/ETag/data (e.g. right after Create), avoiding a
// redundant read-back of what the caller just wrote.
func (f *Filesystem) NewOpenFile(key, etag string, data []byte) *OpenFile {
	buf := make([]byte, len(data))
	copy(buf, data)
	return &OpenFile{fsys: f, key: key, data: buf, etag: etag}
}

// Size returns the current (post-edit, pre-flush) content length.
func (o *OpenFile) Size() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return int64(len(o.data))
}

// ReadAt returns up to length bytes starting at off from the current
// (post-edit, pre-flush) in-memory content — this is read-your-own-
// writes within the same open session, before Flush has made any of it
// visible to anyone else.
func (o *OpenFile) ReadAt(off, length int64) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	if off >= int64(len(o.data)) || length <= 0 {
		return nil
	}
	end := off + length
	if end > int64(len(o.data)) {
		end = int64(len(o.data))
	}
	out := make([]byte, end-off)
	copy(out, o.data[off:end])
	return out
}

// WriteAt records an edit at the given offset: applied to the current
// in-memory buffer immediately (so a subsequent ReadAt in the same
// session sees it), and recorded in the edit log so Flush can replay it
// onto a freshly fetched base if it loses a CAS race.
func (o *OpenFile) WriteAt(data []byte, off int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	o.applyLocked(cp, off)
	o.ops = append(o.ops, writeOp{offset: off, data: cp})
	o.dirty = true
}

func (o *OpenFile) applyLocked(data []byte, off int64) {
	end := off + int64(len(data))
	if end > int64(len(o.data)) {
		grown := make([]byte, end)
		copy(grown, o.data)
		o.data = grown
	}
	copy(o.data[off:end], data)
}

// Flush writes the accumulated edits back as a single whole-object CAS
// write, conditioned on the ETag the edit log is currently based on.
//
// On a lost race (the store reports objstore.IsPreconditionFailed), the
// in-memory merged buffer can't just be resent as-is — it was built on
// top of content that's no longer current, and resending it unconditio-
// nally would silently discard whatever the other writer just
// committed. Instead, Flush re-fetches the latest content, replays
// every recorded edit in the log onto that fresh base, and tries again.
// This is why the edit log (offset, data) is kept separately from the
// merged buffer, not just baked into it: replaying "write these bytes
// at this offset" onto a new base preserves both writers' edits when
// they touch different byte ranges, exactly the scenario ARCHITECTURE.md
// describes as today's lost-update bug.
//
// Bounded by contentWriteRetryBudget, not an attempt count (see its doc
// comment). Returns ErrWriteContention if the budget is exhausted
// without a successful write.
func (o *OpenFile) Flush(ctx context.Context) (Attr, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.dirty {
		return o.fsys.statFile(ctx, o.key)
	}

	deadline := time.Now().Add(contentWriteRetryBudget)
	for {
		attr, newETag, err := o.fsys.WriteFile(ctx, o.key, o.data, o.etag)
		if err == nil {
			o.etag = newETag
			o.ops = nil
			o.dirty = false
			return attr, nil
		}
		if !objstore.IsPreconditionFailed(err) {
			return Attr{}, err
		}
		if time.Now().After(deadline) {
			return Attr{}, ErrWriteContention
		}

		fresh, _, freshETag, rerr := o.fsys.ReadFile(ctx, o.key)
		if rerr != nil {
			return Attr{}, rerr
		}
		o.data = fresh
		for _, op := range o.ops {
			o.applyLocked(op.data, op.offset)
		}
		o.etag = freshETag
	}
}
