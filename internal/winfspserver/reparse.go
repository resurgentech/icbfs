//go:build windows

// Symlinks on this driver: Windows has no native "symlink" concept
// separate from a reparse point — creating one is Create (which this
// driver's Create callback already handles, making an ordinary icbfs
// file) immediately followed by a FSCTL_SET_REPARSE_POINT call, which
// WinFsp delivers as SetReparsePoint below; reading one is
// GetReparsePoint (by open handle) or GetReparsePointByName (by path,
// used by WinFsp's own transparent-reparse-point-resolution machinery
// — wiring this is what lets `cd`/`CreateFile` et al. follow a symlink
// automatically, without this driver doing anything extra in Open).
//
// Hardlinks have no equivalent at all: WinFsp's native
// FSP_FILE_SYSTEM_INTERFACE (confirmed directly, not assumed — see
// ROADMAP.md's F1 entry) has no hardlink-creation callback, unlike
// cgofuse's FUSE-shaped Link(). There is nothing to wire here —
// ROADMAP.md's F3 task text calls for either a working implementation
// or "a clear, intentional error," and the clear, intentional answer
// is that this operation does not exist on this access layer at all.
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

// reparseHeaderSize is REPARSE_DATA_BUFFER_SYMBOLIC_LINK's fixed
// portion: ReparseTag(4) + ReparseDataLength(2) + Reserved(2) +
// SubstituteNameOffset(2) + SubstituteNameLength(2) + PrintNameOffset(2)
// + PrintNameLength(2) + Flags(4) = 20 bytes, before the variable-
// length PathBuffer.
const reparseHeaderSize = 20

// buildSymlinkReparseBuffer encodes target as a
// REPARSE_DATA_BUFFER_SYMBOLIC_LINK — the same structure Windows'
// own CreateSymbolicLinkW produces, confirmed against
// golang.org/x/sys/windows's own Readlink (which parses exactly this
// layout) rather than guessed.
//
// Found the hard way, not assumed: SYMLINK_FLAG_RELATIVE must actually
// reflect whether target is relative — unconditionally setting it
// (this function's first version) made the kernel try to resolve an
// *absolute* target as if it were relative to the link's own
// directory, landing on the literal string "\??" as a bogus first
// path component of a lookup against this volume (confirmed via a
// GetReparsePointByName(name="\??") call this driver's own trace
// caught). Print name strips the NT prefix for an absolute target
// (what a real `dir`/Explorer displays); substitute name keeps it —
// the standard real-NTFS-symlink convention.
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

// parseSymlinkReparseBuffer is buildSymlinkReparseBuffer's inverse,
// used when a client calls CreateSymbolicLinkW (which sends us the
// buffer via SetReparsePoint, below) rather than this driver having
// produced it itself.
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

// GetReparsePoint serves an already-open symlink handle's target back
// as a REPARSE_DATA_BUFFER_SYMBOLIC_LINK, e.g. for an explicit
// FSCTL_GET_REPARSE_POINT call (what readlink(2)-equivalent client
// code issues).
func (d *driver) GetReparsePoint(fs *winfsp.FileSystemRef, file uintptr, name string, buffer []byte) (int, error) {
	h, ok := d.handle(file)
	if !ok {
		return 0, os.ErrNotExist
	}
	if h.typ != icbfs.TypeSymlink {
		return 0, windows.STATUS_NOT_A_REPARSE_POINT
	}
	target, err := d.fsys.Readlink(ctx(), h.key)
	if err != nil {
		return 0, toWinError(err)
	}
	return fillReparseBuffer(buffer, target)
}

// GetReparsePointByName is the by-path counterpart, used by WinFsp's
// own reparse-point resolution machinery (FspFileSystemResolveReparsePointsInternal,
// in WinFsp's own fsop.c) while walking a path — this is what makes a
// symlink transparently followed by default (e.g. a plain `cd`/
// CreateFile without FILE_FLAG_OPEN_REPARSE_POINT).
//
// Returning STATUS_NOT_A_REPARSE_POINT for "this component isn't a
// symlink" is load-bearing, not a style choice: confirmed by reading
// WinFsp's own resolver directly after a real failure traced straight
// to this line — it specifically checks for that exact NTSTATUS to
// mean "keep walking the path," and treats anything else non-success
// (including the os.ErrInvalid this returned at first, which
// convertNTStatus has no mapping for and falls back to a generic
// internal error) as "abort the whole resolution," surfacing as
// Windows' own "Could not find a part of the path" to the caller.
func (d *driver) GetReparsePointByName(fs *winfsp.FileSystemRef, name string, isDirectory bool, buffer []byte) (int, error) {
	r, err := resolvePath(ctx(), d.fsys, name)
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

// SetReparsePoint handles CreateSymbolicLinkW's second half: by the
// time this fires, Create already made an ordinary icbfs file under
// this same name (it has no other way to know a symlink was intended
// — Windows' own CreateFile call that preceded this carries no such
// signal). Replace that placeholder with a real icbfs symlink entry
// pointing at the parsed target, and repoint this handle at it.
func (d *driver) SetReparsePoint(fs *winfsp.FileSystemRef, file uintptr, name string, buffer []byte) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	target, err := parseSymlinkReparseBuffer(buffer)
	if err != nil {
		return err
	}
	dir, base := splitParent(name)
	parent, err := resolvePath(ctx(), d.fsys, dir)
	if err != nil {
		return toWinError(err)
	}

	if h.typ != icbfs.TypeSymlink {
		if err := d.fsys.Unlink(ctx(), parent.Key, base); err != nil {
			return toWinError(err)
		}
	}
	newUUID, _, err := d.fsys.Symlink(ctx(), parent.Key, base, target, 0, 0)
	if err != nil {
		return toWinError(err)
	}
	h.key = newUUID
	h.typ = icbfs.TypeSymlink
	return nil
}
