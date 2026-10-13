package icbfs

import (
	"context"
	"time"
)

// openSessions is this process's own record of which files it has open
// (OpenFile sessions, from Open/NewOpenFile until Close) and which of
// those lost their last link while open. It's what gives two POSIX
// guarantees a single mount can keep on its own:
//
//   - A write is visible as soon as write() returns, before any flush:
//     Stat reports an open session's unflushed size and mtime
//     (localOverlay). Other mounts still see it only after the flush —
//     close-to-open consistency, as with NFS.
//   - An unlinked file lives until its last close: Unlink still removes
//     the name and tombstones .metadata (nlink 0) at once, so every
//     mount sees the file gone, but if this process has it open the
//     objects themselves are deleted on the last Close instead
//     (deleteUnlessOpen). Until then the open sessions keep working —
//     stat, read and write — and their writes are discarded at close,
//     never uploaded. A crash in between leaves the tombstoned objects
//     behind; nothing references them.
//
// Opens on other machines aren't tracked: a file deleted elsewhere
// disappears under this process's sessions as before.
type openSessions struct {
	sessions map[string]map[*OpenFile]struct{}
	// orphans holds the streams of each key whose last link went while
	// it was open here, so its last Close can delete everything.
	orphans map[string]map[string]uint64
}

// holdOpen registers o as open on its key.
func (f *Filesystem) holdOpen(o *OpenFile) {
	f.openMu.Lock()
	defer f.openMu.Unlock()
	if f.opens.sessions == nil {
		f.opens.sessions = make(map[string]map[*OpenFile]struct{})
	}
	set := f.opens.sessions[o.key]
	if set == nil {
		set = make(map[*OpenFile]struct{})
		f.opens.sessions[o.key] = set
	}
	set[o] = struct{}{}
}

// releaseOpen unregisters o, and deletes its file's objects if o was the
// last session holding an unlinked file.
func (f *Filesystem) releaseOpen(o *OpenFile) {
	f.openMu.Lock()
	set := f.opens.sessions[o.key]
	if _, ok := set[o]; !ok {
		f.openMu.Unlock()
		return
	}
	delete(set, o)
	last := len(set) == 0
	if last {
		delete(f.opens.sessions, o.key)
	}
	streams, orphaned := f.opens.orphans[o.key]
	if last && orphaned {
		delete(f.opens.orphans, o.key)
	}
	f.openMu.Unlock()
	if last && orphaned {
		// The close that ends the file's life; the caller's request
		// context may already be gone.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		f.deleteFileObjects(ctx, o.key, streams)
	}
}

// deleteUnlessOpen physically deletes a file whose last link is gone
// (its .metadata already tombstoned), or, if this process has it open,
// leaves that to the last Close.
func (f *Filesystem) deleteUnlessOpen(ctx context.Context, key string, streams map[string]uint64) {
	f.openMu.Lock()
	if len(f.opens.sessions[key]) > 0 {
		if f.opens.orphans == nil {
			f.opens.orphans = make(map[string]map[string]uint64)
		}
		f.opens.orphans[key] = streams
		f.openMu.Unlock()
		return
	}
	f.openMu.Unlock()
	f.deleteFileObjects(ctx, key, streams)
}

// deleteFileObjects removes a file's .metadata, content and streams.
func (f *Filesystem) deleteFileObjects(ctx context.Context, key string, streams map[string]uint64) {
	_ = f.store.Delete(ctx, metadataObjectKey(key), "")
	_ = f.store.Delete(ctx, key, "")
	f.deleteStreamObjects(ctx, key, streams)
}

// isOrphan reports whether key lost its last link while open here.
func (f *Filesystem) isOrphan(key string) bool {
	f.openMu.Lock()
	defer f.openMu.Unlock()
	_, ok := f.opens.orphans[key]
	return ok
}

// localOverlay applies this process's unflushed writes to a, key's
// stored attributes: the size and mtime of the open session that wrote
// most recently, if any has written since its last flush.
func (f *Filesystem) localOverlay(key string, a Attr) Attr {
	f.openMu.Lock()
	set := f.opens.sessions[key]
	sessions := make([]*OpenFile, 0, len(set))
	for o := range set {
		sessions = append(sessions, o)
	}
	f.openMu.Unlock()

	var latest time.Time
	for _, o := range sessions {
		size, wrote, dirty := o.pending()
		if dirty && wrote.After(latest) {
			latest = wrote
			a.Size = size
			a.Mtime = wrote
			if wrote.After(a.Ctime) {
				a.Ctime = wrote
			}
		}
	}
	return a
}
