package fuseserver

import (
	"context"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

var _ fs.NodeStatxer = (*Node)(nil)

// Statx is Getattr with the creation time, which stat(2)'s attributes
// have no room for: statx(2), and so `stat` and `ls --time=birth`, show
// it (on Linux 6.6 and later, which asks a FUSE server for it).
func (n *Node) Statx(ctx context.Context, f fs.FileHandle, flags, mask uint32, out *fuse.StatxOut) syscall.Errno {
	var a fuse.AttrOut
	if errno := n.Getattr(ctx, f, &a); errno != 0 {
		return errno
	}
	sx := &out.Statx
	sx.Mask = unix.STATX_BASIC_STATS
	sx.Ino = a.Ino
	sx.Mode = uint16(a.Mode)
	sx.Nlink = a.Nlink
	sx.Uid, sx.Gid = a.Uid, a.Gid
	sx.Size = a.Size
	sx.RdevMajor, sx.RdevMinor = unix.Major(uint64(a.Rdev)), unix.Minor(uint64(a.Rdev))
	sx.Atime = fuse.SxTime{Sec: a.Atime, Nsec: a.Atimensec}
	sx.Mtime = fuse.SxTime{Sec: a.Mtime, Nsec: a.Mtimensec}
	sx.Ctime = fuse.SxTime{Sec: a.Ctime, Nsec: a.Ctimensec}
	btime := icbfs.Btime(n.key)
	if last := n.last.Load(); last != nil && !last.Btime.IsZero() {
		btime = last.Btime // set by Getattr, with an override if there is one
	}
	if !btime.IsZero() {
		sx.Mask |= unix.STATX_BTIME
		sx.Btime = sxTime(btime)
	}
	return 0
}

func sxTime(t time.Time) fuse.SxTime {
	return fuse.SxTime{Sec: uint64(t.Unix()), Nsec: uint32(t.Nanosecond())}
}
