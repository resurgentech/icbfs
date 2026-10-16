// Package fuseserver is the FUSE access layer: icbfs's filesystem on
// go-fuse's node tree. Linux only; internal/winfspserver is the Windows
// one.
package fuseserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/fslog"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/notify"
)

// Node is one entry: the root, or the item at key. Its type is fixed for
// the inode's lifetime, as an item's identity never changes.
//
// watches is nil without change notifications, and every use of it is a nil
// check.
type Node struct {
	fs.Inode
	fsys    *icbfs.Filesystem
	key     string
	typ     icbfs.EntryType
	watches *watchRegistry
	logger  Logger // nil unless the mount logs; see WithLogger
	// under is the filesystem path of the mount's root ("" when it is
	// the filesystem's root; see WithRoot), so a trash entry records
	// where a deleted item was in the filesystem, not just in the mount.
	under string

	// fcntlOwners are the lock owners that have taken fcntl locks on
	// this file through any handle, so a close by one of them releases
	// them (FileHandle.Flush) and a close by anyone else costs nothing.
	ownersMu    sync.Mutex
	fcntlOwners map[uint64]bool

	// last is the attributes last reported for the item, which Getattr
	// reports, with no links, once the item is gone: an open directory
	// that was removed, or replaced by a rename, as Linux does.
	last atomic.Pointer[icbfs.Attr]
}

var (
	_ fs.InodeEmbedder   = (*Node)(nil)
	_ fs.NodeLookuper    = (*Node)(nil)
	_ fs.NodeGetattrer   = (*Node)(nil)
	_ fs.NodeSetattrer   = (*Node)(nil)
	_ fs.NodeReaddirer   = (*Node)(nil)
	_ fs.NodeMkdirer     = (*Node)(nil)
	_ fs.NodeMknoder     = (*Node)(nil)
	_ fs.NodeCreater     = (*Node)(nil)
	_ fs.NodeOpener      = (*Node)(nil)
	_ fs.NodeUnlinker    = (*Node)(nil)
	_ fs.NodeRmdirer     = (*Node)(nil)
	_ fs.NodeSymlinker   = (*Node)(nil)
	_ fs.NodeReadlinker  = (*Node)(nil)
	_ fs.NodeLinker      = (*Node)(nil)
	_ fs.NodeRenamer     = (*Node)(nil)
	_ fs.NodeStatfser    = (*Node)(nil)
	_ fs.NodeOnForgetter = (*Node)(nil)
	_ fs.NodeFsyncer     = (*Node)(nil)
)

// statfsBlockSize is the unit statfs reports sizes in. There are no real
// blocks; this is the conventional unit.
const statfsBlockSize = 4096

// Root builds the root node of fsys, for fs.Mount. signals is this mount's
// change-notification subscription, or nil for none.
//
// It's a channel, not a notify.Source, because locking's acquire
// accelerator reads the same stream, and a Source's channel can be read by
// only one consumer. The caller fans the Source out with
// notify.NewBroadcaster and gives each consumer its own Subscribe channel.
func Root(fsys *icbfs.Filesystem, signals <-chan notify.Signal, opts ...Option) *Node {
	var watches *watchRegistry
	if signals != nil {
		watches = newWatchRegistry()
		startNotifyDispatch(signals, watches)
	}
	n := &Node{fsys: fsys, key: fsys.RootKey(), typ: icbfs.TypeDir, watches: watches}
	for _, opt := range opts {
		opt(n)
	}
	return n
}

func typeToFuseMode(t icbfs.EntryType) uint32 {
	switch t {
	case icbfs.TypeDir:
		return syscall.S_IFDIR
	case icbfs.TypeSymlink:
		return syscall.S_IFLNK
	case icbfs.TypeFifo:
		return syscall.S_IFIFO
	case icbfs.TypeCharDevice:
		return syscall.S_IFCHR
	case icbfs.TypeBlockDevice:
		return syscall.S_IFBLK
	case icbfs.TypeSocket:
		return syscall.S_IFSOCK
	default:
		return syscall.S_IFREG
	}
}

func setTime(sec *uint64, nsec *uint32, t time.Time) {
	*sec = uint64(t.Unix())
	*nsec = uint32(t.Nanosecond())
}

// fillAttr translates an icbfs.Attr into the fuse.Attr the kernel expects.
// ctime is icbfs's status-change time, or mtime if unset. atime is the
// recorded access time, or for an item never read its creation time (as
// a new item's atime is elsewhere), which unlike mtime doesn't move.
func fillAttr(out *fuse.Attr, key string, typ icbfs.EntryType, a icbfs.Attr) {
	out.Ino = icbfs.Ino(key)
	out.Mode = typeToFuseMode(typ) | (a.Mode & 0o7777)
	out.Size = uint64(a.Size)
	out.Nlink = a.Nlink
	if out.Nlink == 0 && typ == icbfs.TypeDir {
		out.Nlink = 1 // a directory from before nlink was stored; a file at 0 is unlinked but still open
	}
	out.Owner = fuse.Owner{Uid: a.Uid, Gid: a.Gid}
	out.Rdev = a.Rdev // meaningful (and only ever non-zero) for TypeCharDevice/TypeBlockDevice

	mtime := a.Mtime
	if mtime.IsZero() {
		mtime = time.Now()
	}
	setTime(&out.Mtime, &out.Mtimensec, mtime)
	ctime := a.Ctime
	if ctime.IsZero() {
		ctime = mtime
	}
	setTime(&out.Ctime, &out.Ctimensec, ctime)

	atime := a.Atime
	if atime.IsZero() {
		atime = a.Btime
	}
	if atime.IsZero() {
		atime = icbfs.Btime(key)
	}
	if atime.IsZero() {
		atime = mtime
	}
	setTime(&out.Atime, &out.Atimensec, atime)
}

func callerOwner(ctx context.Context) (uid, gid uint32) {
	if caller, ok := fuse.FromContext(ctx); ok {
		return caller.Uid, caller.Gid
	}
	return 0, 0
}

func errnoFromErr(err error) syscall.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, icbfs.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, icbfs.ErrExists):
		return syscall.EEXIST
	case errors.Is(err, icbfs.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, icbfs.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, icbfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, icbfs.ErrReadOnly):
		return syscall.EROFS
	case errors.Is(err, icbfs.ErrArchived):
		return syscall.EROFS
	case errors.Is(err, icbfs.ErrWriteContention):
		return syscall.EAGAIN
	case errors.Is(err, icbfs.ErrLocked):
		return syscall.EAGAIN
	case errors.Is(err, icbfs.ErrLockingDisabled):
		return syscall.ENOSYS
	case errors.Is(err, icbfs.ErrInvalidName):
		// A Windows-reserved name on a primary-Windows filesystem: the core
		// refuses it whichever access layer asks.
		return syscall.EINVAL
	case errors.Is(err, icbfs.ErrNameTooLong):
		return syscall.ENAMETOOLONG
	case errors.Is(err, icbfs.ErrFileTooLarge):
		return syscall.EFBIG
	case errors.Is(err, icbfs.ErrInvalidRename):
		// POSIX rename(2)'s EINVAL: "newpath contains a path prefix
		// of oldpath".
		return syscall.EINVAL
	case errors.Is(err, icbfs.ErrInvalidArgument):
		// truncate(2)/ftruncate(2)'s own EINVAL for a negative length
		// — found as a real crash (make([]byte, negative) panics)
		// via pjdfstest, not hypothetical.
		return syscall.EINVAL
	case errors.Is(err, context.Canceled):
		// An interrupted blocking lock wait (see detach).
		return syscall.EINTR
	default:
		// The kernel only gets "I/O error"; keep the cause.
		log.Printf("icbfs: %v (reported as EIO)", err)
		return syscall.EIO
	}
}

func (n *Node) newChild(ctx context.Context, key string, typ icbfs.EntryType, attr icbfs.Attr) *fs.Inode {
	ctx = detach(ctx)
	child := &Node{fsys: n.fsys, key: key, typ: typ, watches: n.watches, logger: n.logger, under: n.under}
	child.last.Store(&attr)
	stable := fs.StableAttr{Ino: icbfs.Ino(key), Mode: typeToFuseMode(typ)}
	// StableAttr.Ino dedups against any already-known inode with the same
	// number — this is exactly how hardlinks (Link, below) end up sharing
	// one Inode across multiple directory entries.
	inode := n.NewInode(ctx, child, stable)
	if n.watches != nil {
		// A FUSE watch is established by a path lookup, which
		// resolves to a UUID at that moment — this is that moment, for every path that produces a node (Lookup,
		// Create, Mkdir, Symlink all call newChild).
		n.watches.register(key, inode)
	}
	return inode
}

// withUmask is detach for a create: the context also carries the
// request's umask for icbfs to apply (see Mount).
func withUmask(ctx context.Context) context.Context {
	umask, ok := createUmask(ctx)
	ctx = detach(ctx)
	if ok {
		ctx = icbfs.WithUmask(ctx, umask)
	}
	return ctx
}

// confirmWatch records n's inode as its key's (see watchRegistry).
func (n *Node) confirmWatch() {
	if n.watches != nil && n.key != "" {
		n.watches.confirm(n.key, n.EmbeddedInode())
	}
}

// OnForget: the kernel has dropped n's inode.
func (n *Node) OnForget() {
	if n.watches != nil {
		n.watches.forget(n.key, n.EmbeddedInode())
	}
}

func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	ctx = detach(ctx)
	entry, attr, err := n.fsys.Lookup(ctx, n.key, name)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, entry.UUID, entry.Type, attr)
	return n.newChild(ctx, entry.UUID, entry.Type, attr), 0
}

func (n *Node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	ctx = detach(ctx)
	n.confirmWatch()
	attr, err := n.fsys.Stat(ctx, n.key, n.typ)
	if errors.Is(err, icbfs.ErrNotFound) {
		if last := n.last.Load(); last != nil {
			fillAttr(&out.Attr, n.key, n.typ, *last)
			out.Nlink = 0
			return 0
		}
	}
	if err != nil {
		return errnoFromErr(err)
	}
	n.last.Store(&attr)
	fillAttr(&out.Attr, n.key, n.typ, attr)
	return 0
}

// Statfs reports declared capacity and usage (icbfs.Filesystem.StatFS) in
// statfsBlockSize units. Bavail equals Bfree: nothing is reserved for root.
func (n *Node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	ctx = detach(ctx)
	total, used, err := n.fsys.StatFS(ctx)
	if err != nil {
		return errnoFromErr(err)
	}
	out.Bsize = statfsBlockSize
	out.Frsize = statfsBlockSize
	out.Blocks = total / statfsBlockSize
	var free uint64
	if used < total {
		free = (total - used) / statfsBlockSize
	}
	out.Bfree = free
	out.Bavail = free
	// Unset, pathconf(_PC_NAME_MAX) reads 0, which sends tools that build
	// long names (pjdfstest's dirgen_max) into an endless loop. 255 is the
	// limit icbfs enforces (checkName): bytes on primary-POSIX, UTF-16 code
	// units on primary-Windows, as Linux's NTFS and exFAT drivers report.
	out.NameLen = 255
	return 0
}

func (n *Node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	ctx = detach(ctx)
	var modeP, uidP, gidP *uint32
	var sizeP *int64
	var atimeP *time.Time
	if m, ok := in.GetMode(); ok {
		mm := m & 0o7777
		modeP = &mm
	}
	if u, ok := in.GetUID(); ok {
		uidP = &u
	}
	if g, ok := in.GetGID(); ok {
		gidP = &g
	}
	if s, ok := in.GetSize(); ok {
		ss := int64(s)
		sizeP = &ss
	}
	// utimensat(2)'s atime; go-fuse already resolves UTIME_NOW.
	if a, ok := in.GetATime(); ok {
		atimeP = &a
	}
	// utimensat(2)'s explicit mtime set, same UTIME_NOW handling.
	var mtimeP *time.Time
	if m, ok := in.GetMTime(); ok {
		mtimeP = &m
	}

	// ftruncate(2), or open(2) with O_TRUNC, names the handle: truncate
	// its session, or its copy keeps the old bytes and its next flush
	// re-applies its earlier writes over the truncation.
	if h, ok := f.(*FileHandle); ok && h != nil && sizeP != nil {
		if err := h.open.Truncate(*sizeP); err != nil {
			return errnoFromErr(err)
		}
		if _, err := h.open.Flush(ctx); err != nil {
			return errnoFromErr(err)
		}
		sizeP = nil
	}

	attr, err := n.fsys.SetAttr(ctx, n.key, n.typ, modeP, uidP, gidP, sizeP, atimeP, mtimeP, nil)
	if err != nil {
		return errnoFromErr(err)
	}
	fillAttr(&out.Attr, n.key, n.typ, attr)
	return 0
}

func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	ctx = detach(ctx)
	entries, err := n.fsys.ReadDir(ctx, n.key)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	// Best effort: a lost race recording the access must not fail a listing
	// that has already succeeded.
	_ = n.fsys.TouchAtime(ctx, n.key, icbfs.TypeDir)
	n.logEvent(ctx, fslog.EventList, n.path(), 0)
	list := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, fuse.DirEntry{
			Name: e.Name,
			Ino:  icbfs.Ino(e.UUID),
			Mode: typeToFuseMode(e.Type),
		})
	}
	return fs.NewListDirStream(list), 0
}

func (n *Node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	ctx = withUmask(ctx)
	uid, gid := callerOwner(ctx)
	newUUID, attr, err := n.fsys.Mkdir(ctx, n.key, name, mode&0o7777, uid, gid)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, icbfs.TypeDir, attr)
	return n.newChild(ctx, newUUID, icbfs.TypeDir, attr), 0
}

// Mknod creates a FIFO, a Unix socket (bind(2) makes its entry through
// mknod too), or a character or block device. The kernel serves their I/O
// once the type is reported, so this only dispatches on the S_IFMT bits and
// carries dev through for devices.
func (n *Node) Mknod(ctx context.Context, name string, mode, dev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	ctx = withUmask(ctx)
	var typ icbfs.EntryType
	switch mode & syscall.S_IFMT {
	case syscall.S_IFIFO:
		typ = icbfs.TypeFifo
	case syscall.S_IFSOCK:
		typ = icbfs.TypeSocket
	case syscall.S_IFCHR:
		typ = icbfs.TypeCharDevice
	case syscall.S_IFBLK:
		typ = icbfs.TypeBlockDevice
	default:
		// Anything else (a regular file is Create's) isn't a valid mknod
		// target.
		return nil, syscall.EINVAL
	}
	uid, gid := callerOwner(ctx)
	newUUID, attr, err := n.fsys.Mknod(ctx, n.key, name, mode&0o7777, typ, dev, uid, gid)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, typ, attr)
	return n.newChild(ctx, newUUID, typ, attr), 0
}

func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	ctx = withUmask(ctx)
	uid, gid := callerOwner(ctx)
	newUUID, attr, etag, err := n.fsys.Create(ctx, n.key, name, mode&0o7777, uid, gid)
	if err != nil {
		return nil, nil, 0, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, icbfs.TypeFile, attr)
	inode := n.newChild(ctx, newUUID, icbfs.TypeFile, attr)
	fh := newFileHandle(n.fsys.NewOpenFile(newUUID, etag, nil))
	fh.node = inode.Operations().(*Node)
	return inode, fh, 0, 0
}

func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	ctx = detach(ctx)
	n.confirmWatch()
	open, err := n.fsys.Open(ctx, n.key)
	if err != nil {
		return nil, 0, errnoFromErr(err)
	}
	if flags&syscall.O_ACCMODE != syscall.O_WRONLY {
		n.logEvent(ctx, fslog.EventRead, n.path(), 0)
	}
	fh := newFileHandle(open)
	fh.node = n
	return fh, 0, 0
}

func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	ctx = n.deleting(detach(ctx), n.childPath(name))
	if err := n.fsys.Unlink(ctx, n.key, name); err != nil {
		return errnoFromErr(err)
	}
	n.logEvent(ctx, fslog.EventDelete, n.childPath(name), 0)
	return 0
}

func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	ctx = n.deleting(detach(ctx), n.childPath(name))
	if err := n.fsys.Rmdir(ctx, n.key, name); err != nil {
		return errnoFromErr(err)
	}
	n.logEvent(ctx, fslog.EventDelete, n.childPath(name), 0)
	return 0
}

func (n *Node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	ctx = detach(ctx)
	uid, gid := callerOwner(ctx)
	newUUID, attr, err := n.fsys.Symlink(ctx, n.key, name, target, uid, gid)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, icbfs.TypeSymlink, attr)
	return n.newChild(ctx, newUUID, icbfs.TypeSymlink, attr), 0
}

func (n *Node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	ctx = detach(ctx)
	target, err := n.fsys.Readlink(ctx, n.key)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	return []byte(target), 0
}

func (n *Node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	ctx = detach(ctx)
	tn, ok := target.(*Node)
	if !ok {
		return nil, syscall.EINVAL
	}
	attr, err := n.fsys.Link(ctx, n.key, name, tn.key, tn.typ)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, tn.key, tn.typ, attr)
	return n.newChild(ctx, tn.key, tn.typ, attr), 0
}

// renameExchange is renameat2(2)'s RENAME_EXCHANGE, an atomic swap of two
// paths. It isn't supported: it would need its own core operation swapping
// two entries under one condition, and the temp-file-then-rename pattern
// rsync, git and sqlite use doesn't need it.
const renameExchange = 0x2

// Rename implements fs.NodeRenamer: n is the old parent, name the entry's
// name in it. flags carries renameat2(2)'s bits; go-fuse names only
// RENAME_EXCHANGE, so RENAME_NOREPLACE's value (0x1, as in
// golang.org/x/sys/unix) is written out here.
func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	ctx = detach(ctx)
	if flags&renameExchange != 0 {
		return syscall.ENOSYS
	}
	np, ok := newParent.(*Node)
	if !ok {
		return syscall.EINVAL
	}
	const renameNoReplace = 0x1
	replace := flags&renameNoReplace == 0
	ctx = n.deleting(ctx, np.childPath(newName)) // for what the rename replaces
	return errnoFromErr(n.fsys.Rename(ctx, n.key, name, np.key, newName, replace))
}

// FileHandle is an icbfs.OpenFile as go-fuse's file handle.
//
// renewers holds one lease-renewal goroutine per lock owner that holds a
// lock through this handle (ensureRenewal, runRenewal); nil until the first
// lock.
type FileHandle struct {
	open *icbfs.OpenFile
	node *Node // the file, for logging; nil in tests that build handles directly

	// wrote is set by a write and cleared by the close (Flush) that
	// logs it, so a file closed after writing is logged once.
	wrote atomic.Bool

	mu       sync.Mutex
	renewers map[uint64]context.CancelFunc
	wg       sync.WaitGroup // one Add per spawned runRenewal goroutine; Release waits on this
}

func newFileHandle(open *icbfs.OpenFile) *FileHandle {
	return &FileHandle{open: open}
}

var (
	_ fs.FileHandle    = (*FileHandle)(nil)
	_ fs.FileReader    = (*FileHandle)(nil)
	_ fs.FileWriter    = (*FileHandle)(nil)
	_ fs.FileFlusher   = (*FileHandle)(nil)
	_ fs.FileReleaser  = (*FileHandle)(nil)
	_ fs.FileGetlker   = (*FileHandle)(nil)
	_ fs.FileSetlker   = (*FileHandle)(nil)
	_ fs.FileSetlkwer  = (*FileHandle)(nil)
	_ fs.FileAllocater = (*FileHandle)(nil)
)

func (h *FileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	ctx = detach(ctx)
	data := h.open.ReadAt(off, int64(len(dest)))
	// Best-effort, deliberately not checked — same reasoning as
	// Node.Readdir's own TouchAtime call: a lost race on this bump
	// must never fail the read itself.
	_ = h.open.TouchAtime(ctx)
	return fuse.ReadResultData(data), 0
}

func (h *FileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	ctx = detach(ctx)
	if err := h.open.WriteAt(data, off); err != nil {
		return 0, errnoFromErr(err)
	}
	h.wrote.Store(true)
	return uint32(len(data)), 0
}

// Allocate is fallocate(2); see icbfs.OpenFile.Allocate.
func (h *FileHandle) Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno {
	if off > math.MaxInt64 || size > math.MaxInt64 {
		return syscall.EFBIG
	}
	err := h.open.Allocate(int64(off), int64(size), mode)
	if errors.Is(err, icbfs.ErrNotSupported) {
		return syscall.EOPNOTSUPP
	}
	if err != nil {
		return errnoFromErr(err)
	}
	h.wrote.Store(true)
	return 0
}

// Flush runs at every close(2) of the file, synchronously — which is
// why a write is logged here, after the content is stored: a
// synchronously logged close returns only once the server has the event.
//
// A close also releases the closing process's fcntl locks on the file,
// whichever descriptor took them, as POSIX says.
func (h *FileHandle) Flush(ctx context.Context) syscall.Errno {
	if owner, ok := flushOwner(ctx); ok && h.node != nil && h.node.dropFcntlOwner(owner) {
		defer h.releaseAllHeldBy(detach(ctx), owner)
	}
	ctx = detach(ctx)
	attr, err := h.open.Flush(ctx)
	if err != nil {
		return errnoFromErr(err)
	}
	if h.node != nil && h.wrote.Swap(false) {
		h.node.logEvent(ctx, fslog.EventWrite, h.node.path(), attr.Size)
	}
	return 0
}

// Fsync is fsync(2) and fdatasync(2), and msync(2)'s MS_SYNC: a file's
// writes are stored, as a close stores them. A directory's changes are
// stored before each operation returns, so it has nothing to do.
func (n *Node) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	h, ok := f.(*FileHandle)
	if !ok || h == nil {
		return 0
	}
	_, err := h.open.Flush(detach(ctx))
	return errnoFromErr(err)
}

// fcntlLockEOF is the lock end the kernel sends for "to end of file":
// go-fuse's FileLock.FromFlockT maps len 0 to math.MaxInt64.
const fcntlLockEOF = math.MaxInt64

// lockLeaseTTL is the lease of every flock and fcntl claim made through
// FUSE, renewed (ensureRenewal, runRenewal) for as long as the owner holds
// the file open. A holder that crashes or is cut off loses its locks within
// one lease. A var so tests can shrink it.
var lockLeaseTTL = 30 * time.Second

// lockRenewInterval is how often runRenewal renews: a third of the lease,
// so one or two slow or failed renewals don't lose the lock. A var for
// tests, as lockLeaseTTL is.
var lockRenewInterval = lockLeaseTTL / 3

// lockHolderFromOwner turns a FUSE lock owner token (the kernel's, stable
// for one process's or open file's locks) into a lock holder string.
func lockHolderFromOwner(owner uint64) string {
	return fmt.Sprintf("fuse-owner-%d", owner)
}

// lockRangeFromFileLock turns a fuse.FileLock into a [start, end) range. An
// flock(2) request (FUSE_LK_FLOCK) is always the whole file, as in
// go-fuse's LoopbackFile. For fcntl(2), lk.End is inclusive, and
// fcntlLockEOF ("to end of file") maps to the whole-file end rather than
// overflowing as lk.End+1 would.
func lockRangeFromFileLock(lk *fuse.FileLock, flags uint32) (start, end int64) {
	if flags&fuse.FUSE_LK_FLOCK != 0 {
		return 0, fcntlLockEOF
	}
	start = int64(lk.Start)
	if lk.End == fcntlLockEOF {
		return start, fcntlLockEOF
	}
	return start, int64(lk.End) + 1
}

// Getlk implements fcntl(F_GETLK): reports a lock that would conflict
// with the requested one, or L_UNLCK if none would. See
// fs.NodeGetlker's doc comment.
func (h *FileHandle) Getlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32, out *fuse.FileLock) syscall.Errno {
	ctx = detach(ctx)
	if lk.Typ == syscall.F_UNLCK {
		out.Typ = syscall.F_UNLCK
		return 0
	}
	start, end := lockRangeFromFileLock(lk, flags)
	// F_GETLK asks whether a request of this type would succeed, so a read
	// query doesn't report another holder's shared claim.
	info, conflict, err := h.open.FindConflictingLockRange(ctx, start, end, lockHolderFromOwner(owner), lk.Typ == syscall.F_RDLCK)
	if err != nil {
		return errnoFromErr(err)
	}
	if !conflict {
		out.Typ = syscall.F_UNLCK
		return 0
	}
	out.Start = uint64(info.Start)
	if info.End >= fcntlLockEOF {
		out.End = fcntlLockEOF
	} else {
		out.End = uint64(info.End - 1)
	}
	// Reports the conflicting claim's *actual* type, not just
	// F_WRLCK — this codebase's lock model does distinguish
	// shared/exclusive now (see setlk's doc comment).
	if info.Shared {
		out.Typ = syscall.F_RDLCK
	} else {
		out.Typ = syscall.F_WRLCK
	}
	return 0
}

// Setlk implements fcntl(F_SETLK)/flock(LOCK_*, non-blocking). See
// fs.NodeSetlker's doc comment.
func (h *FileHandle) Setlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno {
	return h.setlk(ctx, owner, lk, flags, false)
}

// Setlkw implements fcntl(F_SETLKW)/flock(LOCK_*, blocking). See
// fs.NodeSetlkwer's doc comment.
func (h *FileHandle) Setlkw(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32) syscall.Errno {
	return h.setlk(ctx, owner, lk, flags, true)
}

// setlk is Setlk's and Setlkw's implementation: F_RDLCK takes a shared
// claim and F_WRLCK an exclusive one.
func (h *FileHandle) setlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32, blocking bool) syscall.Errno {
	if !blocking {
		ctx = detach(ctx) // only a blocking wait honors an interrupt
	}
	start, end := lockRangeFromFileLock(lk, flags)
	holder := lockHolderFromOwner(owner)
	if flags&fuse.FUSE_LK_FLOCK == 0 && lk.Typ != syscall.F_UNLCK && h.node != nil {
		h.node.addFcntlOwner(owner)
	}
	switch lk.Typ {
	case syscall.F_UNLCK:
		return errnoFromErr(h.open.ReleaseLockRange(ctx, start, end, holder))
	case syscall.F_RDLCK:
		if blocking {
			if err := h.open.AcquireSharedLockRange(ctx, start, end, holder, lockLeaseTTL); err != nil {
				return errnoFromErr(err)
			}
			h.ensureRenewal(owner)
			return 0
		}
		err := h.open.TryAcquireSharedLockRange(ctx, start, end, holder, lockLeaseTTL)
		if errors.Is(err, icbfs.ErrLocked) {
			return syscall.EAGAIN
		}
		if err != nil {
			return errnoFromErr(err)
		}
		h.ensureRenewal(owner)
		return 0
	case syscall.F_WRLCK:
		if blocking {
			if err := h.open.AcquireLockRange(ctx, start, end, holder, lockLeaseTTL); err != nil {
				return errnoFromErr(err)
			}
			h.ensureRenewal(owner)
			return 0
		}
		err := h.open.TryAcquireLockRange(ctx, start, end, holder, lockLeaseTTL)
		if errors.Is(err, icbfs.ErrLocked) {
			return syscall.EAGAIN
		}
		if err != nil {
			return errnoFromErr(err)
		}
		h.ensureRenewal(owner)
		return 0
	default:
		return syscall.EINVAL
	}
}

func (n *Node) addFcntlOwner(owner uint64) {
	n.ownersMu.Lock()
	defer n.ownersMu.Unlock()
	if n.fcntlOwners == nil {
		n.fcntlOwners = make(map[uint64]bool)
	}
	n.fcntlOwners[owner] = true
}

// dropFcntlOwner forgets owner, and reports whether it had taken fcntl
// locks on n.
func (n *Node) dropFcntlOwner(owner uint64) bool {
	n.ownersMu.Lock()
	defer n.ownersMu.Unlock()
	had := n.fcntlOwners[owner]
	delete(n.fcntlOwners, owner)
	return had
}

// ensureRenewal starts runRenewal for owner unless it's already running on
// this handle.
func (h *FileHandle) ensureRenewal(owner uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.renewers == nil {
		h.renewers = make(map[uint64]context.CancelFunc)
	}
	if _, running := h.renewers[owner]; running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.renewers[owner] = cancel
	h.wg.Add(1)
	go h.runRenewal(ctx, owner)
}

// runRenewal renews every lock owner holds on this file, until this handle
// is released (ctx) or owner holds nothing more.
//
// Each tick asks what owner holds now (LockRangesHeldBy) and renews that,
// since a later overlapping lock can merge or split owner's entries
// (mergeOwnRanges). So a live holder keeps its locks for as long as it has
// the file open, and one that crashes, is cut off, or closes without
// unlocking loses them within one lease.
func (h *FileHandle) runRenewal(ctx context.Context, owner uint64) {
	defer h.wg.Done()
	holder := lockHolderFromOwner(owner)
	defer h.stopRenewal(owner)
	ticker := time.NewTicker(lockRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ranges, err := h.open.LockRangesHeldBy(ctx, holder)
			if err != nil {
				continue // transient (e.g. a slow/failed store round trip) — try again next tick
			}
			if len(ranges) == 0 {
				return // nothing left of this owner's to renew
			}
			for _, r := range ranges {
				// Best effort: a range not renewed this tick is retried
				// next tick, from a fresh LockRangesHeldBy.
				_ = h.open.RenewLockRange(ctx, r.Start, r.End, holder, lockLeaseTTL)
			}
		}
	}
}

func (h *FileHandle) stopRenewal(owner uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.renewers, owner)
}

// Release implements fs.FileReleaser, when the kernel closes this handle.
// It stops this handle's renewal goroutines and releases the locks their
// owners still hold, so a close or exit without F_UNLCK frees them at once,
// as fcntl and flock require (TestMountFcntlByteRangeLocksAcrossProcesses
// checks this).
//
// A close through another descriptor releases them sooner (Flush).
func (h *FileHandle) Release(ctx context.Context) syscall.Errno {
	ctx = detach(ctx)
	h.mu.Lock()
	renewers := h.renewers
	h.renewers = nil
	h.mu.Unlock()
	for _, cancel := range renewers {
		cancel()
	}
	// Wait for the renewal goroutines to stop, so none renews a range while
	// releaseAllHeldBy releases it (and none still reads lockLeaseTTL when
	// a test restores it).
	h.wg.Wait()
	for owner := range renewers {
		h.releaseAllHeldBy(ctx, owner)
	}
	// Releases this session's private local scratch file — without this, every real close(2) would leak
	// a file on local disk for the life of the mount process.
	_ = h.open.Close()
	return 0
}

// releaseAllHeldBy releases every range owner holds on this file.
func (h *FileHandle) releaseAllHeldBy(ctx context.Context, owner uint64) {
	holder := lockHolderFromOwner(owner)
	ranges, err := h.open.LockRangesHeldBy(ctx, holder)
	if err != nil {
		return // best-effort: the lease's own TTL is still the backstop
	}
	for _, r := range ranges {
		_ = h.open.ReleaseLockRange(ctx, r.Start, r.End, holder)
	}
}

// detach returns ctx without its cancellation. go-fuse cancels a
// request's context when the kernel interrupts it, and the kernel
// interrupts whenever the calling thread gets a signal — which the Go
// runtime does constantly to preempt goroutines, in any Go program
// using the mount under load. Abandoning an operation on that would
// turn a spurious signal into an I/O error, or worse (a flush cut off
// mid-upload, a rename between its remove and its insert), so FUSE
// operations run to completion; only a blocking lock wait (Setlkw)
// still honors an interrupt, with EINTR.
func detach(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
