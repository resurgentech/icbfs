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
	open, err := d.openFile(h)
	if err != nil {
		return 0, toWinError(err)
	}
	data := open.ReadAt(int64(offset), int64(len(buf)))
	// Best-effort, deliberately not checked — a lost race on this
	// bump must never fail the read itself.
	if !h.keepAtime.Load() {
		_ = open.TouchAtime(ctx())
	}
	if len(data) == 0 {
		if int64(offset) >= open.Size() {
			return 0, io.EOF
		}
		return 0, nil
	}
	return copy(buf, data), nil
}

// Write handles WinFsp's two special modes as well as a plain offset:
// writeToEndOfFile (append opens, writing at the current end, not at
// offset) and constrainedIo (memory-mapped writes, which never grow the
// file: a write past the end is clipped).
func (d *driver) Write(fs *winfsp.FileSystemRef, file uintptr, buf []byte, offset uint64, writeToEndOfFile, constrainedIo bool, info *winfsp.FSP_FSCTL_FILE_INFO) (int, error) {
	h, ok := d.handle(file)
	if !ok {
		return 0, os.ErrNotExist
	}
	defer d.changedItem(h)
	open, err := d.openFile(h)
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
			d.fillHandleInfo(info, h, attr)
			return 0, nil
		}
		if off+int64(len(buf)) > size {
			buf = buf[:size-off]
		}
	}

	if err := open.WriteAt(buf, off); err != nil {
		return 0, toWinError(err)
	}
	h.wrote.Store(true)

	// Stat reflects the last flush, not this write; report the session's
	// current size, as ReadAt reads its own writes.
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return 0, toWinError(err)
	}
	attr.Size = open.Size()
	d.fillHandleInfo(info, h, attr)
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
	defer d.changedItem(h)
	open := d.currentOpen(h.key)
	if open == nil {
		attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
		if err != nil {
			return toWinError(err)
		}
		d.fillHandleInfo(info, h, attr)
		return nil
	}
	attr, err := open.Flush(ctx())
	if err != nil {
		return toWinError(err)
	}
	d.fillHandleInfo(info, h, attr)
	return nil
}
