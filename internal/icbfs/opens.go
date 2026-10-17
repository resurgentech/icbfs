package icbfs

import (
	"context"
	"os"
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
	// contents is each open file's content, shared by its opens.
	contents map[string]*content
	// orphans holds the streams of each key whose last link went while
	// it was open here, so its last Close can delete everything.
	orphans map[string]map[string]uint64
}

// shareContent returns key's open content with one more reference, or nil
// if the file isn't open here.
func (f *Filesystem) shareContent(key string) *content {
	f.openMu.Lock()
	defer f.openMu.Unlock()
	c := f.opens.contents[key]
	if c != nil {
		c.refs++
	}
	return c
}

// installContent makes c key's open content, with one reference, unless
// another open installed one first: then that one is shared, and c's
// scratch file removed.
func (f *Filesystem) installContent(c *content) *content {
	f.openMu.Lock()
	if have := f.opens.contents[c.key]; have != nil {
		have.refs++
		f.openMu.Unlock()
		if c.file != nil {
			name := c.file.Name()
			c.file.Close()
			_ = os.Remove(name)
		}
		return have
	}
	if f.opens.contents == nil {
		f.opens.contents = make(map[string]*content)
	}
	c.refs = 1
	f.opens.contents[c.key] = c
	f.openMu.Unlock()
	return c
}

// dropContent drops one reference to c, and reports whether it was the
// last, which leaves c to its caller to close.
func (f *Filesystem) dropContent(c *content) bool {
	f.openMu.Lock()
	defer f.openMu.Unlock()
	c.refs--
	if c.refs > 0 {
		return false
	}
	if f.opens.contents[c.key] == c {
		delete(f.opens.contents, c.key)
	}
	return true
}

// openContent is key's open content, or nil.
func (f *Filesystem) openContent(key string) *content {
	f.openMu.Lock()
	defer f.openMu.Unlock()
	return f.opens.contents[key]
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
	_ = f.store.Delete(ctx, metadataObjectKey(key))
	_ = f.store.Delete(ctx, key)
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
// stored attributes: the open content's size and mtime, if it has changed
// since its last flush.
func (f *Filesystem) localOverlay(key string, a Attr) Attr {
	c := f.openContent(key)
	if c == nil {
		return a
	}
	size, wrote, dirty := c.pending()
	if dirty {
		a.Size = size
		a.Mtime = wrote
		if wrote.After(a.Ctime) {
			a.Ctime = wrote
		}
	}
	return a
}
