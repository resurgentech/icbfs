//go:build windows

package winfspserver

import (
	"io"
	"os"

	winfsp "github.com/winfsp/go-winfsp"
)

func (d *driver) Read(fs *winfsp.FileSystemRef, file uintptr, buf []byte, offset uint64) (int, error) {
	h, ok := d.handle(file)
	if !ok {
		return 0, os.ErrNotExist
	}
	open, err := h.ensureOpen(d.fsys)
	if err != nil {
		return 0, toWinError(err)
	}
	data := open.ReadAt(int64(offset), int64(len(buf)))
	if len(data) == 0 {
		if int64(offset) >= open.Size() {
			return 0, io.EOF
		}
		return 0, nil
	}
	return copy(buf, data), nil
}

// Write handles both of WinFsp's special write modes, not just the
// plain offset case: writeToEndOfFile (append-mode opens, where the
// real write position is "wherever the file currently ends," not the
// offset argument) and constrainedIo (memory-mapped writes, which must
// never grow the file — a write landing partly or fully past the
// current end is clipped, not rejected or allowed to extend it).
func (d *driver) Write(fs *winfsp.FileSystemRef, file uintptr, buf []byte, offset uint64, writeToEndOfFile, constrainedIo bool, info *winfsp.FSP_FSCTL_FILE_INFO) (int, error) {
	h, ok := d.handle(file)
	if !ok {
		return 0, os.ErrNotExist
	}
	open, err := h.ensureOpen(d.fsys)
	if err != nil {
		return 0, toWinError(err)
	}

	off := int64(offset)
	if writeToEndOfFile {
		off = open.Size()
	}
	if constrainedIo {
		size := open.Size()
		if off >= size {
			attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
			if err != nil {
				return 0, toWinError(err)
			}
			fillFileInfo(info, h.typ, attr)
			return 0, nil
		}
		if off+int64(len(buf)) > size {
			buf = buf[:size-off]
		}
	}

	open.WriteAt(buf, off)

	// The underlying object's own Stat still reflects whatever was
	// last Flushed, not this write — override Size with the current
	// in-memory (post-edit, pre-flush) length so the response reflects
	// what the caller just wrote, same read-your-own-writes guarantee
	// icbfs.OpenFile.ReadAt already gives within one open session.
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return 0, toWinError(err)
	}
	attr.Size = open.Size()
	fillFileInfo(info, h.typ, attr)
	return len(buf), nil
}

// Flush commits an open file's buffered writes — see icbfs.OpenFile.
// Flush's own doc comment for the CAS-retry/escalation machinery this
// delegates to; this driver doesn't reimplement any of it.
func (d *driver) Flush(fs *winfsp.FileSystemRef, file uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	h.mu.Lock()
	open := h.open
	h.mu.Unlock()
	if open == nil {
		attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
		if err != nil {
			return toWinError(err)
		}
		fillFileInfo(info, h.typ, attr)
		return nil
	}
	attr, err := open.Flush(ctx())
	if err != nil {
		return toWinError(err)
	}
	fillFileInfo(info, h.typ, attr)
	return nil
}
