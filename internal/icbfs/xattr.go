package icbfs

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
)

// Extended attributes, as a mount presents them: users' xattrs (user.,
// trusted., security.) in Attr.Xattrs, and the POSIX ACLs under Linux's
// names system.posix_acl_access and system.posix_acl_default, kept in
// Attr's ACL fields. icbfs's own state (Attr's Windows fields, App) is not
// reachable through any of this.

var (
	ErrNoAttr       = errors.New("no such attribute")              // ENODATA
	ErrNotSupported = errors.New("operation not supported")        // EOPNOTSUPP
	ErrXattrTooBig  = errors.New("attribute value too large")      // E2BIG
	ErrXattrNoSpace = errors.New("no room for more attributes")    // ENOSPC
	ErrACLNotDir    = errors.New("default ACL on a non-directory") // EACCES
)

// Linux's limits: XATTR_NAME_MAX and XATTR_SIZE_MAX. maxXattrTotal bounds
// all of one item's names and values together, since .metadata is read on
// every stat.
const (
	maxXattrName  = 255
	maxXattrValue = 64 << 10
	maxXattrTotal = 64 << 10
)

// XattrHow is setxattr(2)'s flags: XattrCreate fails with ErrExists if the
// attribute exists, XattrReplace with ErrNoAttr if it doesn't.
type XattrHow uint32

const (
	XattrAny     XattrHow = 0
	XattrCreate  XattrHow = 1
	XattrReplace XattrHow = 2
)

const (
	xattrACLAccess  = "system.posix_acl_access"
	xattrACLDefault = "system.posix_acl_default"
)

// userXattrName reports whether name is in a namespace kept in
// Attr.Xattrs.
func userXattrName(name string) bool {
	for _, ns := range []string{"user.", "trusted.", "security."} {
		if strings.HasPrefix(name, ns) && len(name) > len(ns) {
			return true
		}
	}
	return false
}

// checkXattrName refuses names a mount can't use: too long, or in no
// supported namespace. POSIX ACLs exist only on primary-POSIX
// filesystems; on primary-Windows ones the security descriptor is the
// permission.
func (f *Filesystem) checkXattrName(name string) error {
	if len(name) == 0 || len(name) > maxXattrName {
		return ErrNameTooLong
	}
	if name == xattrACLAccess || name == xattrACLDefault {
		if f.primaryWindows {
			return ErrNotSupported
		}
		return nil
	}
	if !userXattrName(name) {
		return ErrNotSupported
	}
	return nil
}

// XattrNames lists an item's extended attributes, as listxattr(2) does:
// users' xattrs, sorted, and the POSIX ACLs it has.
func XattrNames(a Attr) []string {
	names := make([]string, 0, len(a.Xattrs)+2)
	for n := range a.Xattrs {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(a.PosixACLAccess) > 0 {
		names = append(names, xattrACLAccess)
	}
	if len(a.PosixACLDefault) > 0 {
		names = append(names, xattrACLDefault)
	}
	return names
}

// XattrValue returns one of an item's extended attributes, or ErrNoAttr.
func XattrValue(a Attr, name string) ([]byte, error) {
	var v []byte
	var ok bool
	switch name {
	case xattrACLAccess:
		v, ok = a.PosixACLAccess, len(a.PosixACLAccess) > 0
	case xattrACLDefault:
		v, ok = a.PosixACLDefault, len(a.PosixACLDefault) > 0
	default:
		if !userXattrName(name) {
			return nil, ErrNotSupported
		}
		v, ok = a.Xattrs[name]
	}
	if !ok {
		return nil, ErrNoAttr
	}
	return v, nil
}

// SetXattr sets one of an item's extended attributes, as setxattr(2)
// does. Setting a POSIX ACL keeps the mode in step with it: an access ACL
// sets the mode's permission bits (its mask entry, if it has one, as the
// group bits), and one with nothing beyond what the mode says is stored as
// just the mode, as Linux does. A default ACL is only for a directory.
func (f *Filesystem) SetXattr(ctx context.Context, key string, typ EntryType, name string, value []byte, how XattrHow) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	if err := f.checkXattrName(name); err != nil {
		return Attr{}, err
	}
	if len(value) > maxXattrValue {
		return Attr{}, ErrXattrTooBig
	}
	var acl posixACL
	if name == xattrACLAccess || name == xattrACLDefault {
		if name == xattrACLDefault && typ != TypeDir {
			return Attr{}, ErrACLNotDir
		}
		var err error
		if acl, err = parsePosixACL(value); err != nil {
			return Attr{}, err
		}
	}
	var failed error
	attr, err := f.updateMetadata(ctx, key, typ, func(a *Attr) {
		failed = nil
		_, getErr := XattrValue(*a, name)
		exists := getErr == nil
		switch {
		case how == XattrCreate && exists:
			failed = ErrExists
			return
		case how == XattrReplace && !exists:
			failed = ErrNoAttr
			return
		}
		switch name {
		case xattrACLAccess:
			if acl == nil {
				a.PosixACLAccess = nil
				return
			}
			a.Mode = a.Mode&^0o777 | acl.mode()
			if acl.extended() {
				a.PosixACLAccess = acl.encode()
			} else {
				a.PosixACLAccess = nil
			}
		case xattrACLDefault:
			if acl == nil {
				a.PosixACLDefault = nil
			} else {
				a.PosixACLDefault = acl.encode()
			}
		default:
			total := len(name) + len(value)
			for n, v := range a.Xattrs {
				if n != name {
					total += len(n) + len(v)
				}
			}
			if total > maxXattrTotal {
				failed = ErrXattrNoSpace
				return
			}
			if a.Xattrs == nil {
				a.Xattrs = make(map[string][]byte)
			}
			a.Xattrs[name] = append([]byte{}, value...)
		}
	})
	if err != nil {
		return Attr{}, err
	}
	return attr, failed
}

// RemoveXattr removes one of an item's extended attributes, or answers
// ErrNoAttr. Removing the access ACL leaves the mode as it is.
func (f *Filesystem) RemoveXattr(ctx context.Context, key string, typ EntryType, name string) (Attr, error) {
	if err := f.checkWritable(); err != nil {
		return Attr{}, err
	}
	if err := f.checkXattrName(name); err != nil {
		return Attr{}, err
	}
	var failed error
	attr, err := f.updateMetadata(ctx, key, typ, func(a *Attr) {
		failed = nil
		if _, err := XattrValue(*a, name); err != nil {
			failed = err
			return
		}
		switch name {
		case xattrACLAccess:
			a.PosixACLAccess = nil
		case xattrACLDefault:
			a.PosixACLDefault = nil
		default:
			delete(a.Xattrs, name)
			if len(a.Xattrs) == 0 {
				a.Xattrs = nil
			}
		}
	})
	if err != nil {
		return Attr{}, err
	}
	return attr, failed
}

// EnablePosixACLs makes new items inherit their directory's default ACL
// (on a primary-POSIX filesystem), as Linux does. It costs a read of the
// directory's .metadata per create, so it's for mounts that present ACLs.
func (f *Filesystem) EnablePosixACLs(enabled bool) { f.posixACLs = enabled }

// inheritACLs applies dirKey's default ACL, if it has one, to a new item
// being created in it with attr: the new item's access ACL is the default
// with each class's permissions limited by the requested mode, and the
// mode follows the result; a new directory also gets the default ACL as
// its own.
func (f *Filesystem) inheritACLs(ctx context.Context, dirKey string, attr *Attr, isDir bool) error {
	if !f.posixACLs || f.primaryWindows {
		return nil
	}
	parent, err := f.Stat(ctx, dirKey, TypeDir)
	if err != nil {
		return err
	}
	if len(parent.PosixACLDefault) == 0 {
		return nil
	}
	def, err := parsePosixACL(parent.PosixACLDefault)
	if err != nil || def == nil {
		return nil // an unreadable default is ignored, as if absent
	}
	acl := def.limitedBy(attr.Mode & 0o777)
	attr.Mode = attr.Mode&^0o777 | acl.mode()
	if acl.extended() {
		attr.PosixACLAccess = acl.encode()
	}
	if isDir {
		attr.PosixACLDefault = append([]byte{}, parent.PosixACLDefault...)
	}
	return nil
}

// chmodACL updates an item's access ACL for a new mode, as chmod(2) does
// with ACLs: the owner, mask (or group, without a mask) and other entries
// take the new mode's bits.
func chmodACL(a *Attr, mode uint32) {
	if len(a.PosixACLAccess) == 0 {
		return
	}
	acl, err := parsePosixACL(a.PosixACLAccess)
	if err != nil || acl == nil {
		return
	}
	hasMask := acl.has(aclMask)
	for i := range acl {
		switch acl[i].tag {
		case aclUserObj:
			acl[i].perm = uint16(mode>>6) & 7
		case aclMask:
			acl[i].perm = uint16(mode>>3) & 7
		case aclGroupObj:
			if !hasMask {
				acl[i].perm = uint16(mode>>3) & 7
			}
		case aclOther:
			acl[i].perm = uint16(mode) & 7
		}
	}
	a.PosixACLAccess = acl.encode()
}

// --- POSIX ACLs in Linux's xattr encoding: a little-endian version
// (2), then entries of tag (u16), permissions (u16) and id (u32). ---

const posixACLVersion = 2

const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
)

type posixACLEntry struct {
	tag, perm uint16
	id        uint32
}

type posixACL []posixACLEntry

// parsePosixACL decodes and checks an ACL as Linux's posix_acl_valid
// does: one owner, group and other entry, a mask whenever there are named
// entries, no duplicate names, permissions within rwx. An empty value (a
// version header alone) is no ACL, nil.
func parsePosixACL(b []byte) (posixACL, error) {
	if len(b) < 4 || (len(b)-4)%8 != 0 || binary.LittleEndian.Uint32(b) != posixACLVersion {
		return nil, ErrInvalidArgument
	}
	var acl posixACL
	seen := map[[2]uint32]bool{}
	counts := map[uint16]int{}
	named := false
	for off := 4; off < len(b); off += 8 {
		e := posixACLEntry{
			tag:  binary.LittleEndian.Uint16(b[off:]),
			perm: binary.LittleEndian.Uint16(b[off+2:]),
			id:   binary.LittleEndian.Uint32(b[off+4:]),
		}
		if e.perm&^7 != 0 {
			return nil, ErrInvalidArgument
		}
		switch e.tag {
		case aclUserObj, aclGroupObj, aclMask, aclOther:
			counts[e.tag]++
			if counts[e.tag] > 1 {
				return nil, ErrInvalidArgument
			}
			e.id = 0xFFFFFFFF
		case aclUser, aclGroup:
			k := [2]uint32{uint32(e.tag), e.id}
			if seen[k] {
				return nil, ErrInvalidArgument
			}
			seen[k] = true
			named = true
		default:
			return nil, ErrInvalidArgument
		}
		acl = append(acl, e)
	}
	if len(acl) == 0 {
		return nil, nil
	}
	if counts[aclUserObj] != 1 || counts[aclGroupObj] != 1 || counts[aclOther] != 1 || (named && counts[aclMask] != 1) {
		return nil, ErrInvalidArgument
	}
	sort.SliceStable(acl, func(i, j int) bool {
		if acl[i].tag != acl[j].tag {
			return acl[i].tag < acl[j].tag
		}
		return acl[i].id < acl[j].id
	})
	return acl, nil
}

func (acl posixACL) encode() []byte {
	b := make([]byte, 4, 4+8*len(acl))
	binary.LittleEndian.PutUint32(b, posixACLVersion)
	for _, e := range acl {
		b = binary.LittleEndian.AppendUint16(b, e.tag)
		b = binary.LittleEndian.AppendUint16(b, e.perm)
		b = binary.LittleEndian.AppendUint32(b, e.id)
	}
	return b
}

func (acl posixACL) has(tag uint16) bool {
	for _, e := range acl {
		if e.tag == tag {
			return true
		}
	}
	return false
}

func (acl posixACL) perm(tag uint16) uint32 {
	for _, e := range acl {
		if e.tag == tag {
			return uint32(e.perm)
		}
	}
	return 0
}

// mode is the permission bits the ACL implies: owner, then the mask if
// there is one (else the owning group), then other.
func (acl posixACL) mode() uint32 {
	group := acl.perm(aclGroupObj)
	if acl.has(aclMask) {
		group = acl.perm(aclMask)
	}
	return acl.perm(aclUserObj)<<6 | group<<3 | acl.perm(aclOther)
}

// extended reports whether the ACL says more than a mode can: named
// entries or a mask.
func (acl posixACL) extended() bool {
	for _, e := range acl {
		if e.tag == aclUser || e.tag == aclGroup || e.tag == aclMask {
			return true
		}
	}
	return false
}

// limitedBy is the access ACL a new item gets from a default ACL, as
// Linux's posix_acl_create: the owner, other, and mask (or owning group,
// without a mask) entries keep only the permissions mode allows.
func (acl posixACL) limitedBy(mode uint32) posixACL {
	out := append(posixACL{}, acl...)
	hasMask := out.has(aclMask)
	for i := range out {
		switch out[i].tag {
		case aclUserObj:
			out[i].perm &= uint16(mode>>6) & 7
		case aclMask:
			out[i].perm &= uint16(mode>>3) & 7
		case aclGroupObj:
			if !hasMask {
				out[i].perm &= uint16(mode>>3) & 7
			}
		case aclOther:
			out[i].perm &= uint16(mode) & 7
		}
	}
	return out
}
