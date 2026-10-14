//go:build windows

// Symlinks on Windows are reparse points. Creating one is a Create (an
// ordinary file or directory) followed by FSCTL_SET_REPARSE_POINT, which
// WinFsp delivers as SetReparsePoint below. Reading one is GetReparsePoint
// (by handle) or GetReparsePointByName (by path, used by WinFsp's own
// resolver, which is what makes a plain open follow a symlink).
//
// There are no hard links here: WinFsp has no callback to create one.
package winfspserver

import (
	"encoding/binary"
	"os"
	"strings"
	"syscall"

	winfsp "github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// reparseHeaderSize is REPARSE_DATA_BUFFER_SYMBOLIC_LINK's fixed part:
// ReparseTag(4), ReparseDataLength(2), Reserved(2),
// SubstituteNameOffset(2), SubstituteNameLength(2), PrintNameOffset(2),
// PrintNameLength(2) and Flags(4), 20 bytes, before the PathBuffer.
const reparseHeaderSize = 20

// buildSymlinkReparseBuffer encodes target as a
// REPARSE_DATA_BUFFER_SYMBOLIC_LINK, the layout CreateSymbolicLinkW makes
// (and golang.org/x/sys/windows's Readlink parses).
//
// SYMLINK_FLAG_RELATIVE is set only for a relative target: set for an
// absolute one, the kernel resolves it against the link's directory and
// looks up "\??" as a name. The print name drops the NT prefix of an
// absolute target, as dir and Explorer show it; the substitute name keeps
// it, as NTFS does.
func buildSymlinkReparseBuffer(target string) ([]byte, error) {
	flags := uint32(winfsp.SYMLINK_FLAG_RELATIVE)
	printTarget := target
	if strings.HasPrefix(target, ntPathPrefix) {
		flags = 0
		printTarget = strings.TrimPrefix(target, ntPathPrefix)
	}

	substBytes, err := utf16LEBytes(target)
	if err != nil {
		return nil, err
	}
	printBytes, err := utf16LEBytes(printTarget)
	if err != nil {
		return nil, err
	}
	pathBuffer := append(append([]byte{}, substBytes...), printBytes...)
	substLen := uint16(len(substBytes))
	printLen := uint16(len(printBytes))

	dataLen := 12 + len(pathBuffer) // SubstituteNameOffset/Length + PrintNameOffset/Length (2*4=8) + Flags (4) = 12
	buf := make([]byte, 8+dataLen)
	binary.LittleEndian.PutUint32(buf[0:4], windows.IO_REPARSE_TAG_SYMLINK)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(dataLen))
	binary.LittleEndian.PutUint16(buf[8:10], 0)         // SubstituteNameOffset
	binary.LittleEndian.PutUint16(buf[10:12], substLen) // SubstituteNameLength
	binary.LittleEndian.PutUint16(buf[12:14], substLen) // PrintNameOffset (right after substitute name)
	binary.LittleEndian.PutUint16(buf[14:16], printLen) // PrintNameLength
	binary.LittleEndian.PutUint32(buf[16:20], flags)
	copy(buf[20:], pathBuffer)
	return buf, nil
}

// utf16LEBytes encodes s as little-endian UTF-16 bytes with no NUL
// terminator — the raw form REPARSE_DATA_BUFFER's PathBuffer needs.
func utf16LEBytes(s string) ([]byte, error) {
	utf16, err := windows.UTF16FromString(s)
	if err != nil {
		return nil, err
	}
	utf16 = utf16[:len(utf16)-1] // drop the NUL UTF16FromString appends
	b := make([]byte, len(utf16)*2)
	for i, c := range utf16 {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b, nil
}

// parseSymlinkReparseBuffer is buildSymlinkReparseBuffer's inverse, for a
// buffer a client's CreateSymbolicLinkW sends (SetReparsePoint).
func parseSymlinkReparseBuffer(buf []byte) (string, error) {
	if len(buf) < reparseHeaderSize {
		return "", os.ErrInvalid
	}
	if binary.LittleEndian.Uint32(buf[0:4]) != windows.IO_REPARSE_TAG_SYMLINK {
		return "", os.ErrInvalid
	}
	substOff := binary.LittleEndian.Uint16(buf[8:10])
	substLen := binary.LittleEndian.Uint16(buf[10:12])
	pathBuffer := buf[reparseHeaderSize:]
	if int(substOff)+int(substLen) > len(pathBuffer) {
		return "", os.ErrInvalid
	}
	raw := pathBuffer[substOff : substOff+substLen]
	utf16 := make([]uint16, len(raw)/2)
	for i := range utf16 {
		utf16[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	return windows.UTF16ToString(utf16), nil
}

// GetReparsePoint returns an open handle's reparse data: a symlink's target
// as a REPARSE_DATA_BUFFER_SYMBOLIC_LINK, or a non-symlink reparse point's
// stored buffer as is.
func (d *driver) GetReparsePoint(fs *winfsp.FileSystemRef, file uintptr, name string, buffer []byte) (int, error) {
	h, ok := d.handle(file)
	if !ok {
		return 0, os.ErrNotExist
	}
	if h.typ == icbfs.TypeSymlink {
		target, err := d.fsys.Readlink(ctx(), h.key)
		if err != nil {
			return 0, toWinError(err)
		}
		return fillReparseBuffer(buffer, target)
	}
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return 0, toWinError(err)
	}
	if raw, ok := icbfs.GenericReparseFromXattrs(attr.Xattrs); ok {
		return copyReparseBuffer(buffer, raw)
	}
	return 0, windows.STATUS_NOT_A_REPARSE_POINT
}

// GetReparsePointByName is GetReparsePoint by path, for WinFsp's resolver
// (FspFileSystemResolveReparsePointsInternal) as it walks a path.
//
// For a component that isn't a symlink it must answer
// STATUS_NOT_A_REPARSE_POINT: that's what tells WinFsp to keep walking. Any
// other error aborts the resolution, and the caller sees "Could not find a
// part of the path". A non-symlink reparse point answers the same: its
// opaque data is reachable only through GetReparsePoint, on a handle opened
// with FILE_FLAG_OPEN_REPARSE_POINT.
func (d *driver) GetReparsePointByName(fs *winfsp.FileSystemRef, name string, isDirectory bool, buffer []byte) (int, error) {
	r, err := resolvePath(ctx(), d.fsys, name, d.caseInsensitive)
	if err != nil {
		return 0, toWinError(err)
	}
	if r.Type != icbfs.TypeSymlink {
		return 0, windows.STATUS_NOT_A_REPARSE_POINT
	}
	target, err := d.fsys.Readlink(ctx(), r.Key)
	if err != nil {
		return 0, toWinError(err)
	}
	return fillReparseBuffer(buffer, target)
}

// copyReparseBuffer copies a non-symlink reparse point's stored bytes to
// the caller's buffer, failing as fillReparseBuffer does when it's too
// small.
func copyReparseBuffer(buffer, raw []byte) (int, error) {
	if len(raw) > len(buffer) {
		return 0, syscall.ERROR_INSUFFICIENT_BUFFER
	}
	return copy(buffer, raw), nil
}

func fillReparseBuffer(buffer []byte, target string) (int, error) {
	buf, err := buildSymlinkReparseBuffer(target)
	if err != nil {
		return 0, err
	}
	if len(buf) > len(buffer) {
		return 0, syscall.ERROR_INSUFFICIENT_BUFFER
	}
	return copy(buffer, buf), nil
}

// SetReparsePoint handles two cases, by the buffer's ReparseTag:
//
//   - IO_REPARSE_TAG_SYMLINK: the second half of CreateSymbolicLinkW.
//     Create has already made an ordinary entry under the name (nothing
//     in the create says a symlink is coming); replace it with an icbfs
//     symlink to the parsed target, and point this handle at it.
//   - Any other tag: a generic reparse point. Store the buffer as is on
//     the file, which stays an ordinary file.
func (d *driver) SetReparsePoint(fs *winfsp.FileSystemRef, file uintptr, name string, buffer []byte) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	if len(buffer) >= 4 && binary.LittleEndian.Uint32(buffer[0:4]) != windows.IO_REPARSE_TAG_SYMLINK {
		_, err := d.fsys.SetGenericReparseData(ctx(), h.key, buffer)
		return toWinError(err)
	}

	target, err := parseSymlinkReparseBuffer(buffer)
	if err != nil {
		return err
	}
	dir, base := splitParent(name)
	parent, err := resolvePath(ctx(), d.fsys, dir, d.caseInsensitive)
	if err != nil {
		return toWinError(err)
	}
	// The kernel passes the name uppercased under -case-insensitive (see
	// realStoredName); with the exact-match Unlink, every symlink creation
	// failed.
	base = realStoredName(ctx(), d.fsys, parent.Key, base, d.caseInsensitive)

	// A directory symlink (SYMBOLIC_LINK_FLAG_DIRECTORY) arrives as a
	// directory, because Create can't tell it from a mkdir (see Create). So
	// remove the placeholder with Rmdir, not Unlink (which fails on a
	// directory with ErrIsDir); it's new and empty.
	wasDir := h.typ == icbfs.TypeDir
	// Replacing a placeholder isn't a delete anyone made: no trash entry.
	pctx := icbfs.WithDeleteInfo(ctx(), icbfs.DeleteInfo{NoTrash: true})
	if wasDir {
		if err := d.fsys.Rmdir(pctx, parent.Key, base); err != nil {
			return toWinError(err)
		}
	} else if h.typ != icbfs.TypeSymlink {
		if err := d.fsys.Unlink(pctx, parent.Key, base); err != nil {
			return toWinError(err)
		}
	}
	newUUID, _, err := d.fsys.Symlink(ctx(), parent.Key, base, target, 0, 0)
	if err != nil {
		return toWinError(err)
	}
	if wasDir {
		// Windows treats a directory symlink as a directory from now on
		// (RemoveDirectory removes it: winfsp-tests'
		// reparse_symlink_relative_test); see attributesFromAttr.
		if _, err := d.fsys.MarkWindowsDirSymlink(ctx(), newUUID); err != nil {
			return toWinError(err)
		}
	}
	h.key = newUUID
	h.typ = icbfs.TypeSymlink
	return nil
}

// DeleteReparsePoint handles FSCTL_DELETE_REPARSE_POINT for a generic
// reparse point, removing its stored data. A symlink can't be turned back
// into a file.
func (d *driver) DeleteReparsePoint(fs *winfsp.FileSystemRef, file uintptr, name string, buffer []byte) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	if h.typ == icbfs.TypeSymlink {
		return windows.STATUS_NOT_A_REPARSE_POINT
	}
	_, err := d.fsys.DeleteGenericReparseData(ctx(), h.key)
	return toWinError(err)
}
