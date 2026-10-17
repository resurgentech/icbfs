//go:build windows

package winfspserver

import (
	"os"
	"sync"

	winfsp "github.com/winfsp/go-winfsp"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/namecache"
)

// GetOrNewDirBuffer returns h's directory buffer, the same one on every
// call: WinFsp reads continuation markers back out of it, and a new buffer
// each time would leak and break continuation.
func (d *driver) GetOrNewDirBuffer(fs *winfsp.FileSystemRef, file uintptr) (*winfsp.DirBuffer, error) {
	h, ok := d.handle(file)
	if !ok {
		return nil, os.ErrNotExist
	}
	if h.typ != icbfs.TypeDir {
		return nil, os.ErrInvalid
	}
	return &h.dirBuf, nil
}

// ReadDirectory fills every entry, ignoring pattern, as gofs does: WinFsp
// filters the results against the pattern itself unless a filesystem sets
// FspFSAttributePassQueryDirectoryPattern, which this one doesn't.
func (d *driver) ReadDirectory(fs *winfsp.FileSystemRef, file uintptr, pattern string, fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error)) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	gen := d.names.Gen()
	entries, err := d.fsys.ReadDir(ctx(), h.key)
	if err != nil {
		return toWinError(err)
	}
	// Best-effort, deliberately not checked — same reasoning as
	// Read's own TouchAtime call.
	_ = d.fsys.TouchAtime(ctx(), h.key, icbfs.TypeDir)

	// Every non-root directory lists "." and ".." first, as on NTFS
	// (winfsp-tests' querydir_test). ".." reports the parent's info
	// when the path this handle was opened with still resolves, and
	// falls back to the directory's own info otherwise (e.g. after a
	// rename) — only its directory-ness is load-bearing for callers.
	if h.key != d.fsys.RootKey() {
		self, err := d.fsys.Stat(ctx(), h.key, icbfs.TypeDir)
		if err != nil {
			return toWinError(err)
		}
		var info winfsp.FSP_FSCTL_FILE_INFO
		d.fillFileInfo(&info, h.key, icbfs.TypeDir, self)
		if ok, err := fill(".", &info); err != nil || !ok {
			return err
		}
		parentKey, parentAttr := h.key, self
		if dir, _ := splitParent(h.path); h.path != "" {
			if p, err := resolvePath(ctx(), d.res(), dir); err == nil && p.Type == icbfs.TypeDir {
				parentKey, parentAttr = p.Key, p.Attr
			}
		}
		info = winfsp.FSP_FSCTL_FILE_INFO{}
		d.fillFileInfo(&info, parentKey, icbfs.TypeDir, parentAttr)
		if ok, err := fill("..", &info); err != nil || !ok {
			return err
		}
	}

	values := d.statEntries(h.key, entries)
	var listed []namecache.Value
	for i, e := range entries {
		v := values[i]
		if !v.Found {
			continue // best-effort: an entry that vanished between ReadDir and Stat is simply omitted, same as a concurrent unlink racing a real directory read on any filesystem
		}
		listed = append(listed, v)
		var info winfsp.FSP_FSCTL_FILE_INFO
		d.fillFileInfo(&info, e.UUID, e.Type, v.Attr)
		if ok, err := fill(e.Name, &info); err != nil || !ok {
			return err
		}
	}
	// The whole listing: opening what was just listed needs no lookup,
	// and a name not in it (Explorer's desktop.ini) is known missing.
	d.names.PutListing(h.key, listed, gen)
	return nil
}

// statConcurrency is how many of a listing's items are stat'ed at once:
// a listing of a folder of a thousand files is a thousand round trips to
// the store, overlapped.
const statConcurrency = 16

// statEntries is each entry's attributes: from the name cache when it has
// them still fresh, else from the store, statConcurrency at a time. An
// entry gone meanwhile is Found false.
func (d *driver) statEntries(dirKey string, entries []block.Entry) []namecache.Value {
	out := make([]namecache.Value, len(entries))
	sem := make(chan struct{}, statConcurrency)
	var wg sync.WaitGroup
	for i, e := range entries {
		if v, ok := d.names.Get(dirKey, e.Name); ok && v.Found && v.Entry.UUID == e.UUID {
			out[i] = v
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e block.Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			attr, err := d.fsys.Stat(ctx(), e.UUID, e.Type)
			if err == nil {
				out[i] = namecache.Value{Found: true, Entry: e, Attr: attr}
			}
		}(i, e)
	}
	wg.Wait()
	return out
}
