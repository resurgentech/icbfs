package fuseserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// Extended attributes and POSIX ACLs (icbfs's xattr.go): the kernel checks
// who may get and set them (default_permissions, and with EnableAcl it
// enforces the ACLs itself); these only read and store them.

var (
	_ fs.NodeGetxattrer    = (*Node)(nil)
	_ fs.NodeSetxattrer    = (*Node)(nil)
	_ fs.NodeRemovexattrer = (*Node)(nil)
	_ fs.NodeListxattrer   = (*Node)(nil)
)

// capabilityXattr is the file capabilities' xattr. The kernel asks for it
// before every write(2), to strip it when an unprivileged process changes
// the file, and FUSE stops the kernel remembering that a file has none.
const capabilityXattr = "security.capability"

// Getxattr returns attr's value, or with a too-small dest, its size and
// ERANGE (dest is empty when the caller only asks the size).
//
// security.capability is answered from the attributes this node last
// reported, which this mount's own changes keep current, rather than with
// a stat per write (about 1 ms each). A capability set from another
// machine is seen at the next stat. Stripping is local anyway: no other
// machine's kernel strips capabilities when this mount writes.
func (n *Node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	ctx = detach(ctx)
	var a icbfs.Attr
	if last := n.last.Load(); attr == capabilityXattr && last != nil {
		a = *last
	} else {
		var err error
		if a, err = n.fsys.Stat(ctx, n.key, n.typ); err != nil {
			return 0, errnoFromErr(err)
		}
		n.last.Store(&a)
	}
	v, err := icbfs.XattrValue(a, attr)
	if err != nil {
		return 0, xattrErrno(err)
	}
	if len(dest) < len(v) {
		return uint32(len(v)), syscall.ERANGE
	}
	return uint32(copy(dest, v)), 0
}

// Setxattr stores attr; flags are setxattr(2)'s XATTR_CREATE and
// XATTR_REPLACE.
//
// Setting an access ACL clears the setgid bit if the caller is neither in
// the item's group nor root, as Linux does (the kernel leaves it to FUSE
// filesystems).
func (n *Node) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	caller, hasCaller := fuse.FromContext(ctx)
	ctx = detach(ctx)
	how := icbfs.XattrAny
	switch {
	case flags&1 != 0: // XATTR_CREATE
		how = icbfs.XattrCreate
	case flags&2 != 0: // XATTR_REPLACE
		how = icbfs.XattrReplace
	}
	a, err := n.fsys.SetXattr(ctx, n.key, n.typ, attr, data, how)
	if err == nil && attr == "system.posix_acl_access" && a.Mode&0o2000 != 0 &&
		hasCaller && caller.Uid != 0 && !inGroup(caller, a.Gid) {
		mode := a.Mode &^ 0o2000 & 0o7777
		a, err = n.fsys.SetAttr(ctx, n.key, n.typ, &mode, nil, nil, nil, nil, nil, nil)
	}
	if err == nil {
		n.last.Store(&a)
	}
	return xattrErrno(err)
}

// inGroup reports whether the calling process is in group gid: its group,
// or one of its supplementary groups, which FUSE doesn't pass and
// /proc/PID/status lists.
func inGroup(caller *fuse.Caller, gid uint32) bool {
	if caller.Gid == gid {
		return true
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", caller.Pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(status), "\n") {
		if groups, ok := strings.CutPrefix(line, "Groups:"); ok {
			for _, g := range strings.Fields(groups) {
				if g == strconv.FormatUint(uint64(gid), 10) {
					return true
				}
			}
		}
	}
	return false
}

// Removexattr removes attr.
func (n *Node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	ctx = detach(ctx)
	a, err := n.fsys.RemoveXattr(ctx, n.key, n.typ, attr)
	if err == nil {
		n.last.Store(&a)
	}
	return xattrErrno(err)
}

// Listxattr writes the names, each ending in a NUL, or with a too-small
// dest, their size and ERANGE.
func (n *Node) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	ctx = detach(ctx)
	a, err := n.fsys.Stat(ctx, n.key, n.typ)
	if err != nil {
		return 0, errnoFromErr(err)
	}
	var buf []byte
	for _, name := range icbfs.XattrNames(a) {
		buf = append(append(buf, name...), 0)
	}
	if len(dest) < len(buf) {
		return uint32(len(buf)), syscall.ERANGE
	}
	return uint32(copy(dest, buf)), 0
}

func xattrErrno(err error) syscall.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, icbfs.ErrNoAttr):
		return syscall.ENODATA
	case errors.Is(err, icbfs.ErrNotSupported):
		return syscall.EOPNOTSUPP
	case errors.Is(err, icbfs.ErrNameTooLong):
		return syscall.ERANGE // what setxattr(2) answers for a name too long
	case errors.Is(err, icbfs.ErrXattrTooBig):
		return syscall.E2BIG
	case errors.Is(err, icbfs.ErrXattrNoSpace):
		return syscall.ENOSPC
	case errors.Is(err, icbfs.ErrACLNotDir):
		return syscall.EACCES
	}
	return errnoFromErr(err)
}
