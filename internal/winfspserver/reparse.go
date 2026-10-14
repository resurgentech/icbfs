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

// GetReparsePoint serves an already-open handle's reparse data back —
// a symlink's target, encoded as a REPARSE_DATA_BUFFER_SYMBOLIC_LINK,
// or (ROADMAP.md's Part G, task G4) a generic, non-symlink reparse
// point's raw stored buffer, returned byte-exact.
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

// GetReparsePointByName is the by-path counterpart, used by WinFsp's
// own reparse-point resolution machinery (FspFileSystemResolveReparsePointsInternal,
// in WinFsp's own fsop.c) while walking a path — this is what makes a
// symlink transparently followed by default (e.g. a plain `cd`/
// CreateFile without FILE_FLAG_OPEN_REPARSE_POINT).
//
// Returning STATUS_NOT_A_REPARSE_POINT for "this component isn't a
// reparse point this driver can resolve transparently" is load-
// bearing, not a style choice: confirmed by reading WinFsp's own
// resolver directly after a real failure traced straight to this
// line — it specifically checks for that exact NTSTATUS to mean
// "keep walking the path," and treats anything else non-success
// (including the os.ErrInvalid this returned at first, which
// convertNTStatus has no mapping for and falls back to a generic
// internal error) as "abort the whole resolution," surfacing as
// Windows' own "Could not find a part of the path" to the caller. A
// generic (non-symlink) reparse point is deliberately treated the
// same way here — this driver has no transparent-follow behavior for
// an opaque, unknown tag, only explicit FSCTL_GET_REPARSE_POINT
// access (via GetReparsePoint above, through a handle opened with
// FILE_FLAG_OPEN_REPARSE_POINT).
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

// copyReparseBuffer copies a generic reparse point's raw stored bytes
// back to the caller's buffer, same insufficient-buffer handling as
// fillReparseBuffer's symlink-specific path.
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

// SetReparsePoint handles two genuinely different cases, dispatched
// on the buffer's own leading ReparseTag:
//
//   - IO_REPARSE_TAG_SYMLINK: CreateSymbolicLinkW's second half. By
//     the time this fires, Create already made an ordinary icbfs file
//     under this same name (it has no other way to know a symlink was
//     intended — Windows' own CreateFile call that preceded this
//     carries no such signal). Replace that placeholder with a real
//     icbfs symlink entry pointing at the parsed target, and repoint
//     this handle at it.
//   - Any other tag (ROADMAP.md's Part G, task G4): a generic reparse
//     point — store the raw buffer byte-exact against the existing
//     file, no type change at all (unlike the symlink case, this
//     stays an ordinary icbfs file with extra stored data).
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
	// The kernel hands this callback an uppercased name under
	// task F4's -case-insensitive=true default — the exact same
	// case-folding gotcha task F9 already found and fixed for
	// Rename/Cleanup, recurring here because this is a third,
	// separately-written call site that was never updated to match
	// (ROADMAP.md's Part G, task G5; confirmed via a real debug trace,
	// not assumed: CreateSymbolicLinkW's placeholder Create arrives as
	// "\\link1.txt", but this callback's own SetReparsePoint then
	// arrives as "\\LINK1.TXT" for the identical operation).
	// icbfs.Filesystem's own lookups are exact-match only, so an
	// uppercased base here made Unlink fail with ErrNotFound on every
	// single symlink creation — which BestEffortCreateSymbolicLinkW's
	// own fallback-on-ERROR_INVALID_PARAMETER logic doesn't catch,
	// surfacing as a plain, unconditional failure regardless of
	// SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE.
	base = realStoredName(ctx(), d.fsys, parent.Key, base, d.caseInsensitive)

	// A *directory* symlink (SYMBOLIC_LINK_FLAG_DIRECTORY) arrives here
	// as a real icbfs directory, not a file — Create (driver.go) has no
	// reliable way to tell a directory-symlink placeholder apart from
	// an ordinary mkdir at create time (confirmed empirically: both
	// produce identical createOptions for this exact call), so it
	// always makes a real directory, and resolving the ambiguity is
	// this call's job instead: Rmdir, not Unlink, for that case —
	// Unlink on a directory fails outright (icbfs.ErrIsDir), which is
	// exactly what broke this before (surfaced to the caller as
	// STATUS_FILE_IS_A_DIRECTORY). The placeholder is always freshly
	// created and still empty, so Rmdir's "non-empty" rejection never
	// applies here.
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
		// Windows treats a directory symlink as a directory from here
		// on (e.g. RemoveDirectory removes it — winfsp-tests'
		// reparse_symlink_relative_test); see attributesFromAttr.
		if _, err := d.fsys.MarkWindowsDirSymlink(ctx(), newUUID); err != nil {
			return toWinError(err)
		}
	}
	h.key = newUUID
	h.typ = icbfs.TypeSymlink
	return nil
}

// DeleteReparsePoint handles FSCTL_DELETE_REPARSE_POINT — go-winfsp's
// own separate BehaviourDeleteReparsePoint hook (ROADMAP.md's Part G,
// task G4; confirmed not implemented at all before this task, for
// symlinks or anything else, while scoping this work). Only the
// generic reparse case is meaningfully reachable here in practice:
// winfsp-tests' own symlink tests never call this, and this driver
// has no "revert a symlink back to an ordinary file" operation to
// call even if a client did — deleting a generic reparse point's
// stored data is the real, tested behavior this task needs.
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
