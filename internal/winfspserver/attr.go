//go:build windows

package winfspserver

import (
	"os"
	"time"

	winfsp "github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// fillFileInfo translates an icbfs.Attr into the FSP_FSCTL_FILE_INFO
// WinFsp expects back from Open/Create/GetFileInfo/Overwrite.
//
// Known simplification, same one internal/fuseserver's fillAttr
// accepts for the same underlying reason (ARCHITECTURE.md's metadata
// model derives mtime from a version's LastModified, with no separate
// ctime/atime tracked at all): CreationTime/LastAccessTime/
// LastWriteTime/ChangeTime all report Mtime (falling back to Btime,
// then now, if Mtime is zero).
func fillFileInfo(info *winfsp.FSP_FSCTL_FILE_INFO, typ icbfs.EntryType, a icbfs.Attr) {
	info.FileAttributes = attributesFromAttr(typ, a)
	if typ == icbfs.TypeSymlink {
		info.ReparseTag = windows.IO_REPARSE_TAG_SYMLINK
	} else {
		info.ReparseTag = 0
	}
	info.AllocationSize = uint64(a.Size)
	info.FileSize = uint64(a.Size)

	mtime := a.Mtime
	if mtime.IsZero() {
		mtime = a.Btime
	}
	if mtime.IsZero() {
		mtime = time.Now()
	}
	ft := filetimeFromTime(mtime)
	info.CreationTime = ft
	info.LastAccessTime = ft
	info.LastWriteTime = ft
	info.ChangeTime = ft

	info.IndexNumber = 0 // set by the caller via icbfs.Ino where relevant; F3 doesn't yet report WinFsp file IDs (ROADMAP.md F3's Ino reuse note)
	info.HardLinks = 0   // unimplemented, per FSP_FSCTL_FILE_INFO's own doc comment
	info.EaSize = 0
}

// filetimeFromTime converts a Go time.Time into a Windows FILETIME
// (100ns intervals since 1601-01-01), the same representation
// FSP_FSCTL_FILE_INFO's time fields use.
func filetimeFromTime(t time.Time) uint64 {
	ft := windows.NsecToFiletime(t.UnixNano())
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// attributesFromAttr computes the Windows FILE_ATTRIBUTE_* bits to
// report for an entry. Hidden/System/Archive bits (task F7) aren't
// wired yet — this task (F3) only distinguishes directory vs. file
// and a basic read-only approximation from the POSIX owner-write bit,
// matching the same approximation ARCHITECTURE.md already accepts for
// the reverse direction.
func attributesFromAttr(typ icbfs.EntryType, a icbfs.Attr) uint32 {
	switch typ {
	case icbfs.TypeDir:
		return windows.FILE_ATTRIBUTE_DIRECTORY
	case icbfs.TypeSymlink:
		// Windows has no native concept of a symlink's own
		// "read-only"-ness distinct from being a reparse point at
		// all — this attribute is what tells the kernel/clients to
		// resolve it via GetReparsePoint instead of treating it as
		// regular file content.
		return windows.FILE_ATTRIBUTE_REPARSE_POINT
	default:
		if a.Mode&0o200 == 0 {
			return windows.FILE_ATTRIBUTE_READONLY
		}
		return windows.FILE_ATTRIBUTE_NORMAL
	}
}

// modeFromAttributes is Create's inverse: approximates a POSIX mode
// for a new file from the Windows FILE_ATTRIBUTE_READONLY bit a
// CreateFile call supplied. Directories created through Create always
// get a fixed 0755, handled by the caller — this is files only.
func modeFromAttributes(fileAttributes uint32) uint32 {
	if fileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		return 0o444
	}
	return 0o644
}

func (d *driver) GetFileInfo(fs *winfsp.FileSystemRef, file uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return toWinError(err)
	}
	fillFileInfo(info, h.typ, attr)
	return nil
}

// SetBasicInfo updates mode/attribute-adjacent fields WinFsp's
// SetFileTime/SetFileAttributes callers end up driving. This task maps
// only the read-only attribute bit through to the POSIX owner-write
// bit — real Hidden/System/Archive persistence is F7's job (storing
// them in .metadata's xattrs map, per ROADMAP.md).
func (d *driver) SetBasicInfo(fs *winfsp.FileSystemRef, file uintptr, flags winfsp.SetBasicInfoFlags, fileAttributes uint32, creationTime, lastAccessTime, lastWriteTime, changeTime uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	var modeP *uint32
	if flags&winfsp.SetBasicInfoAttributes != 0 && h.typ != icbfs.TypeDir {
		mode := modeFromAttributes(fileAttributes)
		modeP = &mode
	}
	attr, err := d.fsys.SetAttr(ctx(), h.key, h.typ, modeP, nil, nil, nil)
	if err != nil {
		return toWinError(err)
	}
	fillFileInfo(info, h.typ, attr)
	return nil
}

func (d *driver) SetFileSize(fs *winfsp.FileSystemRef, file uintptr, newSize uint64, setAllocationSize bool, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	if setAllocationSize {
		// Allocation-size-only changes (reserving space without
		// changing the reported file size) have no analog in this
		// store — a no-op, same as a sparse-file preallocation hint
		// any backend without real sparse-file support can safely
		// ignore. Report current info unchanged.
		attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
		if err != nil {
			return toWinError(err)
		}
		fillFileInfo(info, h.typ, attr)
		return nil
	}
	size := int64(newSize)
	attr, err := d.fsys.SetAttr(ctx(), h.key, h.typ, nil, nil, nil, &size)
	if err != nil {
		return toWinError(err)
	}
	h.mu.Lock()
	h.open = nil // drop any stale buffered OpenFile; next I/O reopens against the now-resized content
	h.mu.Unlock()
	fillFileInfo(info, h.typ, attr)
	return nil
}

// permissiveSD grants Everyone full control, owned by Builtin
// Administrators — not a real ACL design, F8's job (mapping POSIX
// uid/gid or a stored windows.acl xattr onto a real security
// descriptor). This is deliberately the same placeholder
// cmd/icbfs-winfsp-hello's smoke test uses: the kernel's access check
// on a file open rejects a DACL-only SD outright (confirmed in task
// F1 — see ASSUMPTIONS.md/ROADMAP.md), so Owner/Group have to be
// present for anything to open at all, even before F8 decides what
// they should really be.
func permissiveSD() (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;FA;;;WD)")
}

func (d *driver) GetSecurityByName(fs *winfsp.FileSystemRef, name string, flags winfsp.GetSecurityByNameFlags) (uint32, *windows.SECURITY_DESCRIPTOR, error) {
	r, err := resolvePath(ctx(), d.fsys, name, d.caseInsensitive)
	if err != nil {
		return 0, nil, toWinError(err)
	}
	sd, err := permissiveSD()
	if err != nil {
		return 0, nil, err
	}
	return attributesFromAttr(r.Type, r.Attr), sd, nil
}

func (d *driver) GetSecurity(fs *winfsp.FileSystemRef, file uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	if _, ok := d.handle(file); !ok {
		return nil, os.ErrNotExist
	}
	return permissiveSD()
}

// GetVolumeInfo reports StatFS's declared size/usage — the same
// numbers internal/fuseserver's Statfs reports, just through WinFsp's
// own struct shape.
func (d *driver) GetVolumeInfo(fs *winfsp.FileSystemRef, info *winfsp.FSP_FSCTL_VOLUME_INFO) error {
	total, used, err := d.fsys.StatFS(ctx())
	if err != nil {
		return toWinError(err)
	}
	info.TotalSize = total
	var free uint64
	if used < total {
		free = total - used
	}
	info.FreeSize = free
	label, err := windows.UTF16FromString("icbfs")
	if err != nil {
		return err
	}
	n := copy(info.VolumeLabel[:], label)
	info.VolumeLabelLength = uint16((n - 1) * 2) // bytes, excluding the NUL UTF16FromString appends
	return nil
}
