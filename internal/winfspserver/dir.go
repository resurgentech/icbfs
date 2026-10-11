//go:build windows

package winfspserver

import (
	"os"

	winfsp "github.com/winfsp/go-winfsp"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// GetOrNewDirBuffer returns h's own directory buffer, not a fresh one
// per call — WinFsp requires the buffer handed back here to persist
// across calls for the same open directory (marker-based continuation
// reads back out of it); returning a new zero buffer every time leaks
// the native allocation and breaks continuation (found the hard way in
// task F1's smoke test — see ASSUMPTIONS.md/ROADMAP.md).
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

// ReadDirectory fills every entry in the directory, ignoring pattern —
// matching gofs's own (tested, reference) behavior: WinFsp's own
// dispatcher re-filters fill's results against the query pattern
// itself unless a filesystem opts into FspFSAttributePassQueryDirectoryPattern
// (this one doesn't), so filtering here would be redundant, not
// additionally correct.
func (d *driver) ReadDirectory(fs *winfsp.FileSystemRef, file uintptr, pattern string, fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error)) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	entries, err := d.fsys.ReadDir(ctx(), h.key)
	if err != nil {
		return toWinError(err)
	}
	for _, e := range entries {
		attr, err := d.fsys.Stat(ctx(), e.UUID, e.Type)
		if err != nil {
			continue // best-effort: an entry that vanished between ReadDir and Stat is simply omitted, same as a concurrent unlink racing a real directory read on any filesystem
		}
		var info winfsp.FSP_FSCTL_FILE_INFO
		fillFileInfo(&info, e.Type, attr)
		if ok, err := fill(e.Name, &info); err != nil || !ok {
			return err
		}
	}
	return nil
}
