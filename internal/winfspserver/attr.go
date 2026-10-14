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

// fillFileInfo translates an icbfs.Attr into the FSP_FSCTL_FILE_INFO
// WinFsp expects back from Open/Create/GetFileInfo/Overwrite.
//
// LastWriteTime and ChangeTime both report Mtime (falling back to Btime,
// then now, if Mtime is zero), although icbfs tracks a real status-change
// time (Attr.Ctime) that FUSE reports. CreationTime reports Btime.
//
// LastAccessTime is real, not a copy of
// Mtime: reports a.Atime when it's been explicitly tracked (by a real
// read — this package's own Read/ReadDirectory call TouchAtime, never
// this function), falling back to the same Mtime/Btime/now chain
// otherwise, same honest "as good as we've got" default as before.
//
// A method on *driver (not a free function) so it can consult
// d.fsys.PrimaryWindows() when deciding which attribute representation
// is authoritative — see attributesFromAttr.
func (d *driver) fillFileInfo(info *winfsp.FSP_FSCTL_FILE_INFO, key string, typ icbfs.EntryType, a icbfs.Attr) {
	info.FileAttributes = d.attributesFromAttr(typ, a)
	switch {
	case typ == icbfs.TypeSymlink:
		info.ReparseTag = windows.IO_REPARSE_TAG_SYMLINK
	default:
		// A generic (non-symlink) reparse point — the tag is the raw stored buffer's own leading
		// 4 bytes, same layout every FSCTL_SET_REPARSE_POINT caller
		// already sends. FILE_ATTRIBUTE_REPARSE_POINT must be ORed
		// in alongside whatever attributesFromAttr already reported
		// (ReadOnly, Hidden, ...) — real NTFS reparse points combine
		// freely with other attribute bits, this isn't an either/or.
		if raw, ok := icbfs.GenericReparseFromXattrs(a.Xattrs); ok && len(raw) >= 4 {
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

	atime := a.Atime
	if atime.IsZero() {
		atime = mtime
	}
	info.LastAccessTime = filetimeFromTime(atime)

	// A real, stable, distinct-per-object file ID. It matters a great
	// deal: with every object reporting IndexNumber 0, a real WinFsp
	// conformance run
	// (winfsp-tests --external's own verification tooling)
	// found that a path deleted and immediately recreated under the
	// same name could be confused with its predecessor by the
	// kernel's own FCB/attribute caching, which (like NTFS) uses the
	// file ID as part of its identity/cache-coherency logic — a brand
	// new UUID (and thus a different icbfs.Ino hash) per Create call
	// is exactly what breaks that false "same file" identification.
	// Same reuse of icbfs.Ino the FUSE driver already relies on for
	// its own inode numbers.
	info.IndexNumber = icbfs.Ino(key)
	info.HardLinks = 0 // unimplemented, per FSP_FSCTL_FILE_INFO's own doc comment
	info.EaSize = 0
}

// unixEpochAsFiletime is 1970-01-01 in FILETIME units (100ns intervals
// since 1601-01-01).
const unixEpochAsFiletime = 116444736000000000

// filetimeFromTime converts a Go time.Time into a Windows FILETIME
// (100ns intervals since 1601-01-01), the representation
// FSP_FSCTL_FILE_INFO's time fields use. Computed from whole seconds,
// not UnixNano, which overflows for dates before 1678 — and SetFileTime
// accepts any FILETIME (winfsp-tests' stream_setfileinfo_test sets one
// in 1601). Times before 1601 clamp to 0.
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

// attributesFromAttr computes the Windows FILE_ATTRIBUTE_* bits to
// report for an entry.
//
// The primary-mode rule (docs/architecture.md's "Windows compatibility:
// primary mode"): on a primary-Windows filesystem, natively-stored
// Hidden/System/ReadOnly/Archive bits (icbfs.WindowsAttributesFromXattrs)
// are authoritative and reported with full fidelity, including
// Hidden/System/Archive, which have no POSIX analog at all. On a
// primary-POSIX filesystem (or a primary-Windows file nothing has ever
// called SetWindowsAttributes on), fall back to the original
// approximation: ReadOnly derived from the POSIX owner-write bit,
// Hidden/System/Archive always clear.
func (d *driver) attributesFromAttr(typ icbfs.EntryType, a icbfs.Attr) uint32 {
	switch typ {
	case icbfs.TypeDir:
		attrs := uint32(windows.FILE_ATTRIBUTE_DIRECTORY)
		if d.fsys.PrimaryWindows() {
			if bits, ok := icbfs.WindowsAttributesFromXattrs(a.Xattrs); ok {
				attrs |= bits
			}
		}
		return attrs
	case icbfs.TypeSymlink:
		// Windows has no native concept of a symlink's own
		// "read-only"-ness distinct from being a reparse point at
		// all — this attribute is what tells the kernel/clients to
		// resolve it via GetReparsePoint instead of treating it as
		// regular file content.
		if _, ok := a.Xattrs[icbfs.WindowsDirSymlinkXattrKey]; ok {
			return windows.FILE_ATTRIBUTE_REPARSE_POINT | windows.FILE_ATTRIBUTE_DIRECTORY
		}
		return windows.FILE_ATTRIBUTE_REPARSE_POINT
	default:
		if d.fsys.PrimaryWindows() {
			if bits, ok := icbfs.WindowsAttributesFromXattrs(a.Xattrs); ok {
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
	attr, err := d.stat(h)
	if err != nil {
		return toWinError(err)
	}
	d.fillHandleInfo(info, h, attr)
	return nil
}

// SetBasicInfo updates mode/attribute-adjacent fields WinFsp's
// SetFileTime/SetFileAttributes callers end up driving.
//
// Always persists the real Hidden/System/ReadOnly/Archive byte via
// SetWindowsAttributes (native-fidelity storage, regardless of
// primary mode) *and*
// always derives/stores the approximated POSIX read-only mode bit the
// same way this driver always has, so a primary-POSIX filesystem's
// FUSE side keeps seeing a sensible (if partial) reflection of a
// Windows client's ReadOnly toggle. Which of the two representations
// GetFileInfo/attributesFromAttr later treats as authoritative is a
// read-time decision based on primary mode, not this method's.
func (d *driver) SetBasicInfo(fs *winfsp.FileSystemRef, file uintptr, flags winfsp.SetBasicInfoFlags, fileAttributes uint32, creationTime, lastAccessTime, lastWriteTime, changeTime uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
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
	// SetFileTime's explicit LastAccessTime/LastWriteTime set
	// — a real,
	// previously entirely-ignored call: this function used to accept
	// creationTime/lastAccessTime/lastWriteTime/changeTime as
	// parameters and silently do nothing with any of them.
	if flags&winfsp.SetBasicInfoLastAccessTime != 0 {
		a := timeFromFiletime(lastAccessTime)
		atimeP = &a
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
	if setAllocationSize {
		// Allocation has no storage analog here (no preallocation in an
		// object store); it's tracked in memory while the file is open
		// and reported back, as NTFS does. Shrinking allocation below
		// the data size truncates the data, also as on NTFS.
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

// fallbackSD is what GetSecurity/GetSecurityByName report for an entry
// with no natively-stored windows.acl xattr (every directory, always —
// see securityDescriptorFor — and any file/symlink SetSecurity has
// never touched): the mounting process's own identity, via go-winfsp's
// own procsd package — the same default gofs itself falls back to.
// (Mapping POSIX uid/gid to a SID would need a mapping there is no
// source for.)
//
// Every SD returned from here must carry an Owner and Group, not just
// a DACL (WinFsp rejects one without, STATUS_INVALID_SECURITY_DESCR) —
// procsd.Load() always does, since it's sourced from a live process
// token via GetSecurityInfo.
func fallbackSD() (*windows.SECURITY_DESCRIPTOR, error) {
	return procsd.Load()
}

// securityDescriptorFor resolves the SD to report for typ/attr: a
// file's, symlink's or directory's own stored windows.acl xattr if one
// was set at create time or by SetSecurity (full native fidelity, as
// for attribute bits), falling back to fallbackSD
// otherwise; a corrupt or
// unparseable stored SDDL string (which SetSecurity itself should
// never produce, but a direct store-level edit could) is treated the
// same as "nothing stored" rather than failing the whole call.
func (d *driver) securityDescriptorFor(typ icbfs.EntryType, attr icbfs.Attr) (*windows.SECURITY_DESCRIPTOR, error) {
	// Stored ACL is only authoritative on a primary-Windows filesystem
	// — "primary mode determines which [format] is authoritative at
	// write time; the other is derived on access" (docs/architecture.md's
	// "Windows compatibility: primary mode" section), the same gating
	// native attribute bits get vs. the mode-derived approximation. A primary-POSIX filesystem always derives
	// from mode instead (below), even if SetSecurity was once called
	// on it: that write is preserved (never discarded — ACLs are
	// explicitly "not a lossy one-way approximation" per the same
	// docs/architecture.md section) but isn't read back as
	// authoritative.
	if d.fsys.PrimaryWindows() {
		if raw, ok := icbfs.WindowsACLFromXattrs(attr.Xattrs); ok {
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

// approximateDACLFromMode is the mode-bit check for a primary-POSIX
// filesystem mounted on Windows: a primary-POSIX filesystem has
// no real per-file Windows ACL, and there is no POSIX uid/gid-to-SID
// mapping in this project to build a real owner/group-differentiated
// one from (no RFC2307/Services-for-NFS-style mapping exists or is
// planned) — so this can only approximate "is this file
// readable/writable at all," using "Everyone" (the WD well-known SID)
// as the universal stand-in principal, the same ownership-agnostic
// style as the ReadOnly-from-owner-write-bit approximation for
// attribute bits.
//
// A mode of 0 (e.g. chmod 000) produces an empty (present, protected,
// zero-ACE) DACL — not an absent one, which Windows treats as "no
// protection at all" and would grant everyone full access, the exact
// opposite of what's needed. WinFsp's own real access-check mechanism
// (automatic for Create/Open/Rename: the dispatcher does this, not
// this driver) then denies access to every caller
// against that empty DACL, including whoever mounted the filesystem —
// matching the FUSE side: a chmod 000 file cannot be read or written
// even by its own owner.
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
	r, err := resolvePath(ctx(), d.fsys, path, d.caseInsensitive)
	if err != nil {
		// A symlink mid-path: hand the whole path to WinFsp's own
		// reparse resolver (see hasIntermediateSymlink). go-winfsp
		// doesn't pass back the reparse point index on an error return,
		// so WinFsp gets 0 and starts its walk at the beginning of the
		// path — it skips non-reparse components via
		// GetReparsePointByName's STATUS_NOT_A_REPARSE_POINT, so the
		// result is the same, just one extra lookup per component.
		if hasIntermediateSymlink(ctx(), d.fsys, path, d.caseInsensitive) {
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

// mergeSecurityDescriptor applies only the components SetSecurity's
// info bitmask actually asks to change onto base, leaving the rest
// untouched — matching the real NtSetSecurityObject contract: a
// Windows caller that just changed DACL permissions via the Security
// tab passes a desc carrying only a DACL, and info with only
// DACL_SECURITY_INFORMATION set; blindly overwriting Owner/Group with
// desc's (likely zero-value/absent) fields would silently corrupt them.
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

// File-specific equivalents of the generic access rights (winnt.h's
// FILE_GENERIC_* / FILE_ALL_ACCESS) — the file object type's generic
// mapping.
const (
	fileGenericRead    = 0x00120089
	fileGenericWrite   = 0x00120116
	fileGenericExecute = 0x001200A0
	fileAllAccess      = 0x001F01FF
)

// mapGenericFileRights rewrites generic rights (GA/GR/GW/GX) in sd's
// DACL to their file-specific equivalents, in place, as NTFS does when
// security is set — so a DACL set with GA reads back as FA
// (winfsp-tests' setsecurity_test). sd must be memory this driver owns
// (e.g. a fresh ToSelfRelative copy), never the caller's buffer.
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

// SetSecurity persists a merged security descriptor as the real
// windows.acl xattr. Directories are a documented no-op
// (success, nothing written) — see securityDescriptorFor and
// SetWindowsACL's own doc comment for why: there's no .metadata object
// for a directory to store one in at all.
func (d *driver) SetSecurity(fs *winfsp.FileSystemRef, file uintptr, info windows.SECURITY_INFORMATION, desc *windows.SECURITY_DESCRIPTOR) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
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
