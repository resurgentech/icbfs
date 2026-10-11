//go:build windows

package winfspserver

import (
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
// Known simplification, same one internal/fuseserver's fillAttr
// accepts for the same underlying reason (ARCHITECTURE.md's metadata
// model derives mtime from a version's LastModified, with no separate
// ctime/atime tracked at all): CreationTime/LastAccessTime/
// LastWriteTime/ChangeTime all report Mtime (falling back to Btime,
// then now, if Mtime is zero).
//
// A method on *driver (not a free function) so it can consult
// d.fsys.PrimaryWindows() when deciding which attribute representation
// is authoritative — see attributesFromAttr.
func (d *driver) fillFileInfo(info *winfsp.FSP_FSCTL_FILE_INFO, typ icbfs.EntryType, a icbfs.Attr) {
	info.FileAttributes = d.attributesFromAttr(typ, a)
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
// report for an entry.
//
// Task F7's primary-mode rule (ARCHITECTURE.md's "File attribute
// bits": "the schema always carries the field, populated/used per
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
		return windows.FILE_ATTRIBUTE_DIRECTORY
	case icbfs.TypeSymlink:
		// Windows has no native concept of a symlink's own
		// "read-only"-ness distinct from being a reparse point at
		// all — this attribute is what tells the kernel/clients to
		// resolve it via GetReparsePoint instead of treating it as
		// regular file content.
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
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return toWinError(err)
	}
	d.fillFileInfo(info, h.typ, attr)
	return nil
}

// SetBasicInfo updates mode/attribute-adjacent fields WinFsp's
// SetFileTime/SetFileAttributes callers end up driving.
//
// Always persists the real Hidden/System/ReadOnly/Archive byte via
// SetWindowsAttributes (task F7's native-fidelity storage — "the
// schema always carries the field," regardless of primary mode) *and*
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
	var modeP *uint32
	if flags&winfsp.SetBasicInfoAttributes != 0 && h.typ != icbfs.TypeDir {
		mode := modeFromAttributes(fileAttributes)
		modeP = &mode
	}
	attr, err := d.fsys.SetAttr(ctx(), h.key, h.typ, modeP, nil, nil, nil)
	if err != nil {
		return toWinError(err)
	}
	if flags&winfsp.SetBasicInfoAttributes != 0 && h.typ != icbfs.TypeDir {
		attr, err = d.fsys.SetWindowsAttributes(ctx(), h.key, fileAttributes)
		if err != nil {
			return toWinError(err)
		}
	}
	d.fillFileInfo(info, h.typ, attr)
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
		d.fillFileInfo(info, h.typ, attr)
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
	d.fillFileInfo(info, h.typ, attr)
	return nil
}

// fallbackSD is what GetSecurity/GetSecurityByName report for an entry
// with no natively-stored windows.acl xattr (every directory, always —
// see securityDescriptorFor — and any file/symlink SetSecurity has
// never touched): the mounting process's own identity, via go-winfsp's
// own procsd package — the same default gofs itself falls back to, one
// of the two concrete options ROADMAP.md's F8 entry named (the other,
// mapping POSIX uid/gid to a SID, has no real mapping to lean on in
// this environment; picking a fixed well-known SID instead, this
// driver's pre-F8 placeholder, was deliberately not kept as the
// long-term default once a real option existed).
//
// Every SD returned from here must carry an Owner and Group, not just
// a DACL (task F1's STATUS_INVALID_SECURITY_DESCR finding) —
// procsd.Load() always does, since it's sourced from a live process
// token via GetSecurityInfo.
func fallbackSD() (*windows.SECURITY_DESCRIPTOR, error) {
	return procsd.Load()
}

// securityDescriptorFor resolves the SD to report for typ/attr: a
// file/symlink's own stored windows.acl xattr if SetSecurity has ever
// set one (full native fidelity, same "schema always carries the
// field" principle task F7 already established for attribute bits),
// falling back to fallbackSD otherwise. Directories never get a stored
// SD at all — they have no .metadata object to store one in (see
// Attr's doc comment) — so always report fallbackSD; a corrupt or
// unparseable stored SDDL string (which SetSecurity itself should
// never produce, but a direct store-level edit could) is treated the
// same as "nothing stored" rather than failing the whole call.
func (d *driver) securityDescriptorFor(typ icbfs.EntryType, attr icbfs.Attr) (*windows.SECURITY_DESCRIPTOR, error) {
	// Stored ACL is only authoritative on a primary-Windows filesystem
	// — "primary mode determines which [format] is authoritative at
	// write time; the other is derived on access" (ARCHITECTURE.md's
	// "Windows compatibility: primary mode" section), the same gating
	// task F7 already applies to native attribute bits vs. the mode-
	// derived approximation. A primary-POSIX filesystem always derives
	// from mode instead (below), even if SetSecurity was once called
	// on it: that write is preserved (never discarded — ACLs are
	// explicitly "not a lossy one-way approximation" per the same
	// ARCHITECTURE.md section) but isn't read back as authoritative,
	// matching the fix this task (C3) found while building the
	// POSIX-primary enforcement path itself, not assumed correct from
	// F8 alone.
	if typ != icbfs.TypeDir && d.fsys.PrimaryWindows() {
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

// approximateDACLFromMode is task C3's "at minimum an approximated
// mode-bit check equivalent to [task C1]'s for a POSIX-primary
// filesystem mounted on Windows" bar: a primary-POSIX filesystem has
// no real per-file Windows ACL, and there is no POSIX uid/gid-to-SID
// mapping in this project to build a real owner/group-differentiated
// one from (no RFC2307/Services-for-NFS-style mapping exists or is
// planned) — so this can only approximate "is this file
// readable/writable at all," using "Everyone" (the WD well-known SID)
// as the universal stand-in principal, the same ownership-agnostic
// style task F7's ReadOnly-from-owner-write-bit approximation already
// established for attribute bits.
//
// A mode of 0 (e.g. chmod 000) produces an empty (present, protected,
// zero-ACE) DACL — not an absent one, which Windows treats as "no
// protection at all" and would grant everyone full access, the exact
// opposite of what's needed. WinFsp's own real access-check mechanism
// (already confirmed automatic for Create/Open/Rename — see task F9's
// share-mode finding for the same class of "the dispatcher does this,
// not our driver" confirmation) then denies access to every caller
// against that empty DACL, including whoever mounted the filesystem —
// matching task C1's own FUSE-side bar translated to Windows: "a
// chmod 000 file cannot be read or written even by its own owner."
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
	r, err := resolvePath(ctx(), d.fsys, name, d.caseInsensitive)
	if err != nil {
		return 0, nil, toWinError(err)
	}
	sd, err := d.securityDescriptorFor(r.Type, r.Attr)
	if err != nil {
		return 0, nil, err
	}
	return d.attributesFromAttr(r.Type, r.Attr), sd, nil
}

func (d *driver) GetSecurity(fs *winfsp.FileSystemRef, file uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	h, ok := d.handle(file)
	if !ok {
		return nil, os.ErrNotExist
	}
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return nil, toWinError(err)
	}
	return d.securityDescriptorFor(h.typ, attr)
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
	return abs.ToSelfRelative()
}

// SetSecurity persists a merged security descriptor as the real
// windows.acl xattr (task F8). Directories are a documented no-op
// (success, nothing written) — see securityDescriptorFor and
// SetWindowsACL's own doc comment for why: there's no .metadata object
// for a directory to store one in at all.
func (d *driver) SetSecurity(fs *winfsp.FileSystemRef, file uintptr, info windows.SECURITY_INFORMATION, desc *windows.SECURITY_DESCRIPTOR) error {
	h, ok := d.handle(file)
	if !ok {
		return os.ErrNotExist
	}
	if h.typ == icbfs.TypeDir {
		return nil
	}
	attr, err := d.fsys.Stat(ctx(), h.key, h.typ)
	if err != nil {
		return toWinError(err)
	}
	base, err := d.securityDescriptorFor(h.typ, attr)
	if err != nil {
		return err
	}
	merged, err := mergeSecurityDescriptor(base, info, desc)
	if err != nil {
		return err
	}
	if _, err := d.fsys.SetWindowsACL(ctx(), h.key, []byte(merged.String())); err != nil {
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
