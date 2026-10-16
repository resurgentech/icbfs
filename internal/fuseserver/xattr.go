package fuseserver

import (
	"context"
	"errors"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"

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

// Getxattr returns attr's value, or with a too-small dest, its size and
// ERANGE (dest is empty when the caller only asks the size).
func (n *Node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	ctx = detach(ctx)
	a, err := n.fsys.Stat(ctx, n.key, n.typ)
	if err != nil {
		return 0, errnoFromErr(err)
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
func (n *Node) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	ctx = detach(ctx)
	how := icbfs.XattrAny
	switch {
	case flags&1 != 0: // XATTR_CREATE
		how = icbfs.XattrCreate
	case flags&2 != 0: // XATTR_REPLACE
		how = icbfs.XattrReplace
	}
	_, err := n.fsys.SetXattr(ctx, n.key, n.typ, attr, data, how)
	return xattrErrno(err)
}

// Removexattr removes attr.
func (n *Node) Removexattr(ctx context.Context, attr string) syscall.Errno {
	ctx = detach(ctx)
	_, err := n.fsys.RemoveXattr(ctx, n.key, n.typ, attr)
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
