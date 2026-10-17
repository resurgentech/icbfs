//go:build windows

package winfspserver

import (
	"os"
	"sort"

	winfsp "github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// Named data streams (Windows alternate data streams), on top of
// icbfs.Filesystem's stream API (internal/icbfs/streams.go). A stream
// handle's key/typ address the stream's own object, so Read, Write,
// Flush, SetFileSize and the shared OpenFile all work on it unchanged;
// everything else — attributes, times, security, deletion and renaming —
// goes to (or through) the file or directory that owns it.

var _ winfsp.BehaviourGetStreamInfo = (*driver)(nil)

// owner is the file or directory h's metadata belongs to: h itself, or
// for a stream handle the entry the stream hangs off.
func (h *fileHandle) owner() (string, icbfs.EntryType) {
	if h.stream != "" {
		return h.ownerKey, h.ownerTyp
	}
	return h.key, h.typ
}

// fillHandleInfo is fillFileInfo for a handle: for a stream handle, the
// owner's info with the stream's own size, never a directory (a
// directory's streams are data). For a stream, attr (the stream
// object's, which carries only a size) is used only if the owner can't
// be read.
func (d *driver) fillHandleInfo(info *winfsp.FSP_FSCTL_FILE_INFO, h *fileHandle, attr icbfs.Attr) {
	if h.stream == "" {
		d.fillFileInfo(info, h.key, h.typ, attr)
		return
	}
	owner, err := d.fsys.Stat(ctx(), h.ownerKey, h.ownerTyp)
	if err != nil {
		d.fillFileInfo(info, h.key, h.typ, attr)
		return
	}
	size, err := d.streamSize(h.key)
	if err != nil {
		size = attr.Size
	}
	d.fillStreamInfo(info, h, owner, size)
}

func (d *driver) fillStreamInfo(info *winfsp.FSP_FSCTL_FILE_INFO, h *fileHandle, owner icbfs.Attr, size int64) {
	d.fillFileInfo(info, h.ownerKey, h.ownerTyp, owner)
	info.FileAttributes &^= windows.FILE_ATTRIBUTE_DIRECTORY
	if info.FileAttributes == 0 {
		info.FileAttributes = windows.FILE_ATTRIBUTE_NORMAL
	}
	info.FileSize = uint64(size)
	info.AllocationSize = d.allocFor(h.key, size)
}

// streamSize is a stream object's current size, including writes still
// buffered in its shared OpenFile.
func (d *driver) streamSize(key string) (int64, error) {
	if open := d.currentOpen(key); open != nil {
		return open.Size(), nil
	}
	a, err := d.fsys.StatStream(ctx(), key)
	return a.Size, err
}

// openStream opens stream on the already-resolved owner r.
func (d *driver) openStream(r resolved, path, stream string, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	real, id, ok := lookupStream(r.Attr.Streams, stream, d.caseInsensitive)
	if !ok {
		return 0, os.ErrNotExist
	}
	h := &fileHandle{key: icbfs.StreamKey(r.Key, id), typ: icbfs.TypeFile, path: path,
		stream: real, ownerKey: r.Key, ownerTyp: r.Type}
	size, err := d.streamSize(h.key)
	if err != nil {
		return 0, toWinError(err)
	}
	id2 := d.newHandle(h)
	d.fillStreamInfo(info, h, r.Attr, size)
	setNormalizedName(info, r.Path+":"+real)
	return id2, nil
}

// createStream creates stream on the owner at path. WinFsp's kernel
// driver opens (or creates) the main file before passing a stream
// create down, so the owner always exists by now.
func (d *driver) createStream(path, stream string, allocationSize uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	r, err := resolvePath(ctx(), d.res(), path)
	if err != nil {
		return 0, toWinError(err)
	}
	if _, _, ok := lookupStream(r.Attr.Streams, stream, d.caseInsensitive); ok {
		return 0, windows.STATUS_OBJECT_NAME_COLLISION
	}
	key, etag, err := d.fsys.CreateStream(ctx(), r.Key, r.Type, stream)
	d.names.ForgetItem(r.Key)
	if err != nil {
		return 0, toWinError(err)
	}
	h := &fileHandle{key: key, typ: icbfs.TypeFile, path: path, stream: stream,
		ownerKey: r.Key, ownerTyp: r.Type, etag: etag, seeded: []byte{}, hasSeed: true}
	id := d.newHandle(h)
	if allocationSize > 0 {
		d.setAlloc(key, allocationSize)
	}
	owner, err := d.fsys.Stat(ctx(), r.Key, r.Type)
	if err != nil {
		return 0, toWinError(err)
	}
	d.fillStreamInfo(info, h, owner, 0)
	setNormalizedName(info, r.Path+":"+stream)
	return id, nil
}

// GetStreamInfo lists the streams of the file or directory a handle is
// on: the main (unnamed) stream for a file — directories have none —
// then each named stream, in name order. Like WinFsp's memfs, a buffer
// too small for every entry returns the entries that fit.
func (d *driver) GetStreamInfo(fs *winfsp.FileSystemRef, file uintptr, buf []byte) (int, error) {
	h, ok := d.handle(file)
	if !ok {
		return 0, os.ErrNotExist
	}
	ownerKey, ownerTyp := h.owner()
	attr, err := d.fsys.Stat(ctx(), ownerKey, ownerTyp)
	if err != nil {
		return 0, toWinError(err)
	}
	n := 0
	if ownerTyp != icbfs.TypeDir {
		size := attr.Size
		if open := d.currentOpen(ownerKey); open != nil {
			size = open.Size()
		}
		w := winfsp.FileSystemAddStreamInfo("", uint64(size), d.allocFor(ownerKey, size), buf[n:])
		if w == 0 {
			return n, nil
		}
		n += w
	}
	names := make([]string, 0, len(attr.Streams))
	for name := range attr.Streams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		key := icbfs.StreamKey(ownerKey, attr.Streams[name])
		size, err := d.streamSize(key)
		if err != nil {
			return 0, toWinError(err)
		}
		w := winfsp.FileSystemAddStreamInfo(name, uint64(size), d.allocFor(key, size), buf[n:])
		if w == 0 {
			return n, nil
		}
		n += w
	}
	n += winfsp.FileSystemAddStreamInfoEnd(buf[n:])
	return n, nil
}
