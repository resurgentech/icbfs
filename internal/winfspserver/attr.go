//go:build windows

package winfspserver

import (
	"encoding/binary"
	"errors"
	"os"
	"time"

	winfsp "github.com/winfsp/go-winfsp"
	"github.com/winfsp/go-winfsp/procsd"
	"golang.org/x/sys/windows"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// fillFileInfo translates an icbfs.Attr into the FSP_FSCTL_FILE_INFO WinFsp
// expects from Open, Create, GetFileInfo and Overwrite.
//
// LastWriteTime and ChangeTime report Mtime (or Btime, or now, if it's
// zero); CreationTime reports Btime; LastAccessTime reports the recorded
// access time, with the same fallbacks. Which attribute bits it reports
// depends on the primary mode (attributesFromAttr).
func (d *driver) fillFileInfo(info *winfsp.FSP_FSCTL_FILE_INFO, key string, typ icbfs.EntryType, a icbfs.Attr) {
	info.FileAttributes = d.attributesFromAttr(typ, a)
	switch {
	case typ == icbfs.TypeSymlink:
		info.ReparseTag = windows.IO_REPARSE_TAG_SYMLINK
	default:
		// A non-symlink reparse point: its tag is the stored buffer's first
		// 4 bytes. FILE_ATTRIBUTE_REPARSE_POINT is added to the other
		// attribute bits, as on NTFS.
		if raw, ok := a.GenericReparse(); ok && len(raw) >= 4 {
			info.ReparseTag = binary.LittleEndian.Uint32(raw[0:4])
			info.FileAttributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
		} else {
			info.ReparseTag = 0
		}
	}
	info.AllocationSize = d.allocFor(key, a.Size)
	info.FileSize = uint64(a.Size)

	mtime := a.Mtime
	if mtime.IsZero() {
		mtime = a.Btime
	}
	if mtime.IsZero() {
		mtime = time.Now()
	}
	ft := filetimeFromTime(mtime)
	info.LastWriteTime = ft
	info.ChangeTime = ft
	// Birth time, not mtime — an explicit SetFileTime on LastWriteTime
	// must leave CreationTime alone (winfsp-tests' setfileinfo_test).
	btime := a.Btime
	if btime.IsZero() {
		btime = mtime
	}
	info.CreationTime = filetimeFromTime(btime)

	// Never read: the later of its creation and its last write. A new
	// file's is its write time (bounded by its creation's millisecond,
	// winfsp-tests' getfileinfo_test); a copy's is when it was made, as on
	// NTFS, not the source's modified time the copy set.
	atime := a.Atime
	if atime.IsZero() {
		atime = mtime
		if btime.After(atime) {
			atime = btime
		}
	}
	info.LastAccessTime = filetimeFromTime(atime)

	// A stable file ID per object. With every object reporting 0, the
	// kernel's caching confused a file deleted and recreated under the same
	// name with its predecessor (winfsp-tests); a new item has a new key
	// and so a new ID. The FUSE driver uses the same icbfs.Ino for inode
	// numbers.
	info.IndexNumber = icbfs.Ino(key)
	info.HardLinks = 0 // unimplemented, per FSP_FSCTL_FILE_INFO's own doc comment
	info.EaSize = 0
}

// unixEpochAsFiletime is 1970-01-01 in FILETIME units (100ns intervals
// since 1601-01-01).
const unixEpochAsFiletime = 116444736000000000

// filetimeFromTime converts a time.Time into a FILETIME (100 ns intervals
// since 1601-01-01). It computes from whole seconds, since UnixNano
// overflows before 1678 and SetFileTime accepts any FILETIME (winfsp-tests'
// stream_setfileinfo_test sets one in 1601). Times before 1601 become 0.
func filetimeFromTime(t time.Time) uint64 {
	ticks := t.Unix()*10_000_000 + int64(t.Nanosecond()/100) + unixEpochAsFiletime
	if ticks < 0 {
		return 0
	}
	return uint64(ticks)
}

// timeFromFiletime is filetimeFromTime's inverse — needed by
// SetBasicInfo to turn a caller's explicit SetFileTime-style request
// into a Go time.Time before handing it to icbfs.Filesystem.SetAttr.
func timeFromFiletime(v uint64) time.Time {
	ticks := int64(v) - unixEpochAsFiletime
	sec, rem := ticks/10_000_000, ticks%10_000_000
	if rem < 0 {
		sec, rem = sec-1, rem+10_000_000
	}
	return time.Unix(sec, rem*100).UTC()
}

// attributesFromAttr computes the FILE_ATTRIBUTE_* bits to report. On a
// primary-Windows filesystem the stored ReadOnly, Hidden, System and
// Archive bits are reported as stored. Otherwise (or for a file that has
// none stored) ReadOnly comes from the owner-write mode bit, and the others
// are clear.
func (d *driver) attributesFromAttr(typ icbfs.EntryType, a icbfs.Attr) uint32 {
	switch typ {
	case icbfs.TypeDir:
		attrs := uint32(windows.FILE_ATTRIBUTE_DIRECTORY)
		if d.fsys.PrimaryWindows() {
			if bits, ok := a.WindowsAttributes(); ok {
				attrs |= bits
			}
		}
		return attrs
	case icbfs.TypeSymlink:
		// The reparse point attribute tells clients to resolve it through
		// GetReparsePoint; a directory symlink is a directory too. Its own
		// bits (Archive, Hidden) as a file's are.
		attrs := uint32(windows.FILE_ATTRIBUTE_REPARSE_POINT)
		if a.WindowsDirSymlink {
			attrs |= windows.FILE_ATTRIBUTE_DIRECTORY
		}
		if d.fsys.PrimaryWindows() {
			if bits, ok := a.WindowsAttributes(); ok {
				attrs |= bits &^ windows.FILE_ATTRIBUTE_DIRECTORY
			}
		}
		return attrs
	default:
		if d.fsys.PrimaryWindows() {
			if bits, ok := a.WindowsAttributes(); ok {
				if bits == 0 {
					return windows.FILE_ATTRIBUTE_NORMAL
				}
				return bits
			}
		}
		if a.Mode&0o200 == 0 {
			return windows.FILE_ATTRIBUTE_READONLY
		}
		return windows.FILE_ATTRIBUTE_NORMAL
	}
}

// modeFromAttributes is the POSIX mode for a new file, from the
// FILE_ATTRIBUTE_READONLY bit its create asked for. Directories get 0755
// from the caller.
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
	attr, err := d.stat(h)
	if err != nil {
		return toWinError(err)
	}
	d.fillHandleInfo(info, h, attr)
	return nil
}

// SetBasicInfo applies SetFileAttributes and SetFileTime.
//
// It stores the ReadOnly, Hidden, System and Archive bits as given,
// whatever the primary mode, and also sets the POSIX owner-write bit from
// ReadOnly, so a Linux mount of a primary-POSIX filesystem sees a Windows
// client's ReadOnly. Which one is authoritative is decided when read, by
// primary mode.
func (d *driver) SetBasicInfo(fs *winfsp.FileSystemRef, file uintptr, flags winfsp.SetBasicInfoFlags, fileAttributes uint32, creationTime, lastAccessTime, lastWriteTime, changeTime uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	defer d.changedItem(h)
	// A stream's attributes and times are its owner's.
	key, typ := h.owner()
	var modeP *uint32
	if flags&winfsp.SetBasicInfoAttributes != 0 && typ != icbfs.TypeDir {
		mode := modeFromAttributes(fileAttributes)
		modeP = &mode
	}
	var atimeP, mtimeP, btimeP *time.Time
	if flags&winfsp.SetBasicInfoCreationTime != 0 {
		b := timeFromFiletime(creationTime)
		btimeP = &b
	}
	// SetFileTime's explicit times.
	if flags&winfsp.SetBasicInfoLastAccessTime != 0 {
		a := timeFromFiletime(lastAccessTime)
		atimeP = &a
		h.keepAtime.Store(true)
	}
	if flags&winfsp.SetBasicInfoLastWriteTime != 0 {
		m := timeFromFiletime(lastWriteTime)
		mtimeP = &m
	}
	attr, err := d.fsys.SetAttr(ctx(), key, typ, modeP, nil, nil, nil, atimeP, mtimeP, btimeP)
	if err != nil {
		return toWinError(err)
	}
	if flags&winfsp.SetBasicInfoAttributes != 0 {
		// Directories too, e.g. a read-only directory (winfsp-tests'
		// create_readonlydir_test), which Windows then refuses to
		// remove until the bit is cleared.
		attr, err = d.fsys.SetWindowsAttributes(ctx(), key, typ, fileAttributes)
		if err != nil {
			return toWinError(err)
		}
	}
	d.fillHandleInfo(info, h, attr)
	return nil
}

func (d *driver) SetFileSize(fs *winfsp.FileSystemRef, file uintptr, newSize uint64, setAllocationSize bool, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	defer d.changedItem(h)
	if setAllocationSize {
		// Allocation has no counterpart in an object store, so it's kept in
		// memory while the file is open and reported, as NTFS does.
		// Shrinking it below the data size truncates the data, also as on
		// NTFS.
		attr, err := d.stat(h)
		if err != nil {
			return toWinError(err)
		}
		if int64(newSize) < attr.Size {
			if err := d.flushShared(h.key); err != nil {
				return toWinError(err)
			}
			size := int64(newSize)
			if attr, err = d.fsys.SetAttr(ctx(), h.key, h.typ, nil, nil, nil, &size, nil, nil, nil); err != nil {
				return toWinError(err)
			}
			d.invalidateShared(h.key)
		}
		d.setAlloc(h.key, newSize)
		d.fillHandleInfo(info, h, attr)
		return nil
	}
	// Commit buffered writes first, so the resize applies on top of
	// them rather than them being lost (or later flushed over it).
	if err := d.flushShared(h.key); err != nil {
		return toWinError(err)
	}
	size := int64(newSize)
	attr, err := d.fsys.SetAttr(ctx(), h.key, h.typ, nil, nil, nil, &size, nil, nil, nil)
	if err != nil {
		return toWinError(err)
	}
	d.invalidateShared(h.key) // next I/O reopens against the resized content
	d.fillHandleInfo(info, h, attr)
	return nil
}

// stat is fsys.Stat, but with Size reflecting writes still buffered in
// the file's shared OpenFile — the store only sees them at Flush.
func (d *driver) stat(h *fileHandle) (icbfs.Attr, error) {
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return attr, err
	}
	if open := d.currentOpen(h.key); open != nil {
		attr.Size = open.Size()
	}
	return attr, nil
}

// fallbackSD is the security descriptor for an item on a primary-Windows
// filesystem with none stored: the mounting process's identity, from
// go-winfsp's procsd, as gofs does. (There's no uid-to-SID mapping to
// derive one from.) It has an Owner and a Group, as WinFsp requires
// (STATUS_INVALID_SECURITY_DESCR otherwise).
func fallbackSD() (*windows.SECURITY_DESCRIPTOR, error) {
	return procsd.Load()
}

// securityDescriptorFor resolves the descriptor to report for an item. A
// stored descriptor that doesn't parse is treated as none.
func (d *driver) securityDescriptorFor(typ icbfs.EntryType, attr icbfs.Attr) (*windows.SECURITY_DESCRIPTOR, error) {
	// On a primary-Windows filesystem the stored descriptor is
	// authoritative, else fallbackSD. On a primary-POSIX one the descriptor
	// is always derived from the mode; a descriptor set there is kept but
	// not read back.
	if d.fsys.PrimaryWindows() {
		if raw, ok := attr.WindowsACL(); ok {
			if sd, err := windows.SecurityDescriptorFromString(string(raw)); err == nil {
				return sd, nil
			}
		}
	}
	if !d.fsys.PrimaryWindows() {
		return approximateDACLFromMode(attr.Mode)
	}
	return fallbackSD()
}

// approximateDACLFromMode derives a DACL from a primary-POSIX item's mode.
// There's no uid-to-SID mapping, so it can only say whether the item can be
// read and written at all, granted to Everyone (WD).
//
// Mode 0 gives an empty DACL, not an absent one: Windows reads an absent
// DACL as "no protection" and would grant everyone everything. WinFsp's
// access check then refuses every caller, the mounting one included, as a
// mode-0 file is unreadable to its owner on Linux.
func approximateDACLFromMode(mode uint32) (*windows.SECURITY_DESCRIPTOR, error) {
	const (
		anyRead  = 0o444
		anyWrite = 0o200 // matches attributesFromAttr's own owner-write-bit approximation
	)
	var sddl string
	switch {
	case mode&anyWrite != 0:
		sddl = "O:BAG:BAD:P(A;;FA;;;WD)"
	case mode&anyRead != 0:
		sddl = "O:BAG:BAD:P(A;;FR;;;WD)"
	default:
		sddl = "O:BAG:BAD:P"
	}
	return windows.SecurityDescriptorFromString(sddl)
}

func (d *driver) GetSecurityByName(fs *winfsp.FileSystemRef, name string, flags winfsp.GetSecurityByNameFlags) (uint32, *windows.SECURITY_DESCRIPTOR, error) {
	path, stream, ok := splitStream(name)
	if !ok {
		return 0, nil, windows.STATUS_OBJECT_NAME_INVALID
	}
	r, err := resolvePath(ctx(), d.res(), path)
	if err != nil {
		// A symlink partway along the path: let WinFsp's reparse resolver
		// walk it (see hasIntermediateSymlink). go-winfsp passes no reparse
		// index back on an error, so WinFsp starts from the beginning; it
		// skips plain components (STATUS_NOT_A_REPARSE_POINT), at one extra
		// lookup each.
		if hasIntermediateSymlink(ctx(), d.res(), path) {
			return 0, nil, windows.STATUS_REPARSE
		}
		return 0, nil, toWinError(err)
	}
	sd, err := d.securityDescriptorFor(r.Type, r.Attr)
	if err != nil {
		return 0, nil, err
	}
	attrs := d.attributesFromAttr(r.Type, r.Attr)
	if stream != "" {
		// A stream has its owner's security; it is never a directory.
		if _, _, found := lookupStream(r.Attr.Streams, stream, d.caseInsensitive); !found {
			return 0, nil, os.ErrNotExist
		}
		attrs &^= windows.FILE_ATTRIBUTE_DIRECTORY
		if attrs == 0 {
			attrs = windows.FILE_ATTRIBUTE_NORMAL
		}
	}
	return attrs, sd, nil
}

func (d *driver) GetSecurity(fs *winfsp.FileSystemRef, file uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	h, ok := d.handle(file)
	if !ok {
		return nil, os.ErrNotExist
	}
	key, typ := h.owner()
	attr, err := d.fsys.Stat(ctx(), key, typ)
	if err != nil {
		return nil, toWinError(err)
	}
	return d.securityDescriptorFor(typ, attr)
}

// mergeSecurityDescriptor applies to base only the parts SetSecurity's info
// asks to change, as NtSetSecurityObject does: changing permissions in
// Explorer sends only a DACL, and overwriting Owner and Group with its
// absent ones would erase them.
func mergeSecurityDescriptor(base *windows.SECURITY_DESCRIPTOR, info windows.SECURITY_INFORMATION, desc *windows.SECURITY_DESCRIPTOR) (*windows.SECURITY_DESCRIPTOR, error) {
	abs, err := base.ToAbsolute()
	if err != nil {
		return nil, err
	}
	if info&windows.OWNER_SECURITY_INFORMATION != 0 {
		owner, defaulted, err := desc.Owner()
		if err != nil {
			return nil, err
		}
		if err := abs.SetOwner(owner, defaulted); err != nil {
			return nil, err
		}
	}
	if info&windows.GROUP_SECURITY_INFORMATION != 0 {
		group, defaulted, err := desc.Group()
		if err != nil {
			return nil, err
		}
		if err := abs.SetGroup(group, defaulted); err != nil {
			return nil, err
		}
	}
	if info&windows.DACL_SECURITY_INFORMATION != 0 {
		dacl, defaulted, err := desc.DACL()
		present := true
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			present, err = false, nil
		}
		if err != nil {
			return nil, err
		}
		if err := abs.SetDACL(dacl, present, defaulted); err != nil {
			return nil, err
		}
		if err := copyControl(abs, desc, info, windows.SE_DACL_PROTECTED, windows.SE_DACL_AUTO_INHERITED, windows.SE_DACL_AUTO_INHERIT_REQ,
			windows.PROTECTED_DACL_SECURITY_INFORMATION, windows.UNPROTECTED_DACL_SECURITY_INFORMATION); err != nil {
			return nil, err
		}
	}
	if info&windows.SACL_SECURITY_INFORMATION != 0 {
		sacl, defaulted, err := desc.SACL()
		present := true
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			present, err = false, nil
		}
		if err != nil {
			return nil, err
		}
		if err := abs.SetSACL(sacl, present, defaulted); err != nil {
			return nil, err
		}
		if err := copyControl(abs, desc, info, windows.SE_SACL_PROTECTED, windows.SE_SACL_AUTO_INHERITED, windows.SE_SACL_AUTO_INHERIT_REQ,
			windows.PROTECTED_SACL_SECURITY_INFORMATION, windows.UNPROTECTED_SACL_SECURITY_INFORMATION); err != nil {
			return nil, err
		}
	}
	merged, err := abs.ToSelfRelative()
	if err != nil {
		return nil, err
	}
	if err := mapGenericFileRights(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// copyControl sets abs's protected and auto-inherited flags (SDDL's P and
// AI) for a new ACL from desc, as NTFS does, measured: protection is set
// by the new ACL's flag or info's PROTECTED_ flag, taken away only by
// UNPROTECTED_, and otherwise stays as it was; the auto-inherited mark is
// set when the caller asks for inheritance to be worked out (desc's
// AUTO_INHERIT_REQ, as SetSecurityInfo and robocopy /SEC send it) and
// otherwise cleared, so a plain SetFileSecurity of an AI descriptor
// leaves it unmarked, as on NTFS.
func copyControl(abs, desc *windows.SECURITY_DESCRIPTOR, info windows.SECURITY_INFORMATION,
	protected, autoInherited, autoInheritReq windows.SECURITY_DESCRIPTOR_CONTROL, protect, unprotect windows.SECURITY_INFORMATION) error {
	ctl, _, err := desc.Control()
	if err != nil {
		return err
	}
	old, _, err := abs.Control()
	if err != nil {
		return err
	}
	set := old & protected
	switch {
	case info&unprotect != 0:
		set = 0
	case info&protect != 0 || ctl&protected != 0:
		set = protected
	}
	if ctl&autoInheritReq != 0 {
		set |= autoInherited
	}
	return abs.SetControl(protected|autoInherited, set)
}

// File-specific equivalents of the generic access rights (winnt.h's
// FILE_GENERIC_* / FILE_ALL_ACCESS) — the file object type's generic
// mapping.
const (
	fileGenericRead    = 0x00120089
	fileGenericWrite   = 0x00120116
	fileGenericExecute = 0x001200A0
	fileAllAccess      = 0x001F01FF
)

// mapGenericFileRights rewrites generic rights (GA, GR, GW, GX) in sd's
// DACL to their file equivalents, in place, as NTFS does when security is
// set, so a DACL set with GA reads back as FA (winfsp-tests'
// setsecurity_test). sd must be this driver's own memory (a fresh
// ToSelfRelative copy), never the caller's buffer.
func mapGenericFileRights(sd *windows.SECURITY_DESCRIPTOR) error {
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || dacl == nil {
		return nil
	}
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		// An inherit-only entry keeps its generic rights, as NTFS's do:
		// they're mapped where it's inherited.
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		// Every standard ACE layout (allowed, denied, audit, object
		// variants) is header-then-mask, so Mask is valid for all.
		m := uint32(ace.Mask)
		if m&windows.GENERIC_READ != 0 {
			m = m&^windows.GENERIC_READ | fileGenericRead
		}
		if m&windows.GENERIC_WRITE != 0 {
			m = m&^windows.GENERIC_WRITE | fileGenericWrite
		}
		if m&windows.GENERIC_EXECUTE != 0 {
			m = m&^windows.GENERIC_EXECUTE | fileGenericExecute
		}
		if m&windows.GENERIC_ALL != 0 {
			m = m&^windows.GENERIC_ALL | fileAllAccess
		}
		ace.Mask = windows.ACCESS_MASK(m)
	}
	return nil
}

// SetSecurity stores the merged security descriptor as the item's
// windows.acl, for files and directories alike.
func (d *driver) SetSecurity(fs *winfsp.FileSystemRef, file uintptr, info windows.SECURITY_INFORMATION, desc *windows.SECURITY_DESCRIPTOR) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	defer d.changedItem(h)
	key, typ := h.owner()
	attr, err := d.fsys.Stat(ctx(), key, typ)
	if err != nil {
		return toWinError(err)
	}
	base, err := d.securityDescriptorFor(typ, attr)
	if err != nil {
		return err
	}
	merged, err := mergeSecurityDescriptor(base, info, desc)
	if err != nil {
		return err
	}
	if _, err := d.fsys.SetWindowsACL(ctx(), key, typ, []byte(merged.String())); err != nil {
		return toWinError(err)
	}
	return nil
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
