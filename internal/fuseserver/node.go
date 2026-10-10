// Package fuseserver is the FUSE access layer: it adapts icbfs's core
// filesystem logic to go-fuse's node-tree API. Linux/POSIX only — the
// Windows access layer (WinFsp) is a separate, parallel package per
// ARCHITECTURE.md.
package fuseserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/notify"
)

// Node is one filesystem entry: either the root, or identified by its
// blob's UUID (key). Its type (file/dir/symlink) is fixed for the inode's
// lifetime, matching ARCHITECTURE.md's model where identity never changes.
//
// watches is nil when the Change notifications feature (ROADMAP.md
// Part E) isn't enabled for this mount — task E2's explicit
// requirement is that nil here makes every operation behave exactly
// as if this field didn't exist at all, so every access to it is a
// plain nil check, never a required dependency.
type Node struct {
	fs.Inode
	fsys    *icbfs.Filesystem
	key     string
	typ     icbfs.EntryType
	watches *watchRegistry
}

var (
	_ fs.InodeEmbedder  = (*Node)(nil)
	_ fs.NodeLookuper   = (*Node)(nil)
	_ fs.NodeGetattrer  = (*Node)(nil)
	_ fs.NodeSetattrer  = (*Node)(nil)
	_ fs.NodeReaddirer  = (*Node)(nil)
	_ fs.NodeMkdirer    = (*Node)(nil)
	_ fs.NodeCreater    = (*Node)(nil)
	_ fs.NodeOpener     = (*Node)(nil)
	_ fs.NodeUnlinker   = (*Node)(nil)
	_ fs.NodeRmdirer    = (*Node)(nil)
	_ fs.NodeSymlinker  = (*Node)(nil)
	_ fs.NodeReadlinker = (*Node)(nil)
	_ fs.NodeLinker     = (*Node)(nil)
	_ fs.NodeStatfser   = (*Node)(nil)
)

// statfsBlockSize is the block size StatfsOut reports, matching this
// filesystem's reality exactly as closely as "virtual blocks on top of
// an object store" allows: it's an arbitrary but conventional unit for
// df to report sizes in, not a real on-disk block size (there is no
// disk) — see ARCHITECTURE.md's du/df design.
const statfsBlockSize = 4096

// Root builds the node representing fsys's root directory, for use
// with fs.Mount. signals is this mount's subscription to the Change
// notifications backend (ROADMAP.md Part E, task E2) — nil disables
// the feature entirely for this mount, with every operation behaving
// exactly as it did before this feature existed.
//
// signals is a plain channel, not a notify.Source, deliberately: task
// B10 needs the exact same underlying signal stream independently
// observed by both this dispatch and Locking's blocking-acquisition
// accelerator, and notify.Source.Signals() is a single-consumption
// channel — one value read by one consumer is gone for any other. The
// caller is expected to fan one real Source out via
// notify.NewBroadcaster and pass each consumer, including this one,
// its own Subscribe() channel; this package has no need to know that
// Broadcaster exists at all, since all it ever does with signals is
// range over it.
func Root(fsys *icbfs.Filesystem, signals <-chan notify.Signal) *Node {
	var watches *watchRegistry
	if signals != nil {
		watches = newWatchRegistry()
		startNotifyDispatch(signals, watches)
	}
	return &Node{fsys: fsys, key: fsys.RootKey(), typ: icbfs.TypeDir, watches: watches}
}

func typeToFuseMode(t icbfs.EntryType) uint32 {
	switch t {
	case icbfs.TypeDir:
		return syscall.S_IFDIR
	case icbfs.TypeSymlink:
		return syscall.S_IFLNK
	default:
		return syscall.S_IFREG
	}
}

func setTime(sec *uint64, nsec *uint32, t time.Time) {
	*sec = uint64(t.Unix())
	*nsec = uint32(t.Nanosecond())
}

// fillAttr translates an icbfs.Attr into the fuse.Attr the kernel expects.
//
// Known simplification: ctime and atime are both reported as mtime.
// ARCHITECTURE.md derives mtime from the current version's LastModified,
// which also bumps on a metadata-only change — so a true ctime/mtime split
// would need an explicit extra field, and atime isn't tracked at all (see
// ARCHITECTURE.md's metadata model for why).
func fillAttr(out *fuse.Attr, key string, typ icbfs.EntryType, a icbfs.Attr) {
	out.Ino = icbfs.Ino(key)
	out.Mode = typeToFuseMode(typ) | (a.Mode & 0o7777)
	out.Size = uint64(a.Size)
	out.Nlink = a.Nlink
	if out.Nlink == 0 {
		out.Nlink = 1
	}
	out.Owner = fuse.Owner{Uid: a.Uid, Gid: a.Gid}

	mtime := a.Mtime
	if mtime.IsZero() {
		mtime = time.Now()
	}
	setTime(&out.Mtime, &out.Mtimensec, mtime)
	setTime(&out.Ctime, &out.Ctimensec, mtime)
	setTime(&out.Atime, &out.Atimensec, mtime)
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
	case errors.Is(err, icbfs.ErrArchived):
		return syscall.EROFS
	case errors.Is(err, icbfs.ErrWriteContention):
		return syscall.EAGAIN
	case errors.Is(err, icbfs.ErrLocked):
		return syscall.EAGAIN
	case errors.Is(err, icbfs.ErrLockingDisabled):
		return syscall.ENOSYS
	default:
		return syscall.EIO
	}
}

func (n *Node) newChild(ctx context.Context, key string, typ icbfs.EntryType) *fs.Inode {
	child := &Node{fsys: n.fsys, key: key, typ: typ, watches: n.watches}
	stable := fs.StableAttr{Ino: icbfs.Ino(key), Mode: typeToFuseMode(typ)}
	// StableAttr.Ino dedups against any already-known inode with the same
	// number — this is exactly how hardlinks (Link, below) end up sharing
	// one Inode across multiple directory entries.
	inode := n.NewInode(ctx, child, stable)
	if n.watches != nil {
		// Task E3: "a FUSE watch gets established via a path lookup,
		// which already resolves to a UUID at that moment" — this is
		// that moment, for every path that produces a node (Lookup,
		// Create, Mkdir, Symlink all call newChild).
		n.watches.register(key, inode)
	}
	return inode
}

func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	entry, attr, err := n.fsys.Lookup(ctx, n.key, name)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, entry.UUID, entry.Type, attr)
	return n.newChild(ctx, entry.UUID, entry.Type), 0
}

func (n *Node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	attr, err := n.fsys.Stat(ctx, n.key, n.typ)
	if err != nil {
		return errnoFromErr(err)
	}
	fillAttr(&out.Attr, n.key, n.typ, attr)
	return 0
}

// Statfs reports this filesystem's declared capacity and current usage
// (ARCHITECTURE.md's du/df design) via icbfs.Filesystem.StatFS, scaled
// into statfsBlockSize units since that's the unit StatfsOut's
// Blocks/Bfree/Bavail fields are defined in terms of. Bavail is reported
// equal to Bfree (no distinct "reserved for root" reservation exists
// here, unlike a real block device).
func (n *Node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
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
	return 0
}

func (n *Node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	var modeP, uidP, gidP *uint32
	var sizeP *int64
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

	attr, err := n.fsys.SetAttr(ctx, n.key, n.typ, modeP, uidP, gidP, sizeP)
	if err != nil {
		return errnoFromErr(err)
	}
	fillAttr(&out.Attr, n.key, n.typ, attr)
	return 0
}

func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.fsys.ReadDir(ctx, n.key)
	if err != nil {
		return nil, errnoFromErr(err)
	}
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
	uid, gid := callerOwner(ctx)
	newUUID, attr, err := n.fsys.Mkdir(ctx, n.key, name, mode&0o7777, uid, gid)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, icbfs.TypeDir, attr)
	return n.newChild(ctx, newUUID, icbfs.TypeDir), 0
}

func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	uid, gid := callerOwner(ctx)
	newUUID, attr, etag, err := n.fsys.Create(ctx, n.key, name, mode&0o7777, uid, gid)
	if err != nil {
		return nil, nil, 0, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, icbfs.TypeFile, attr)
	fh := &FileHandle{open: n.fsys.NewOpenFile(newUUID, etag, nil)}
	return n.newChild(ctx, newUUID, icbfs.TypeFile), fh, 0, 0
}

func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	open, err := n.fsys.Open(ctx, n.key)
	if err != nil {
		return nil, 0, errnoFromErr(err)
	}
	return &FileHandle{open: open}, 0, 0
}

func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	return errnoFromErr(n.fsys.Unlink(ctx, n.key, name))
}

func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errnoFromErr(n.fsys.Rmdir(ctx, n.key, name))
}

func (n *Node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	uid, gid := callerOwner(ctx)
	newUUID, attr, err := n.fsys.Symlink(ctx, n.key, name, target, uid, gid)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, icbfs.TypeSymlink, attr)
	return n.newChild(ctx, newUUID, icbfs.TypeSymlink), 0
}

func (n *Node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.fsys.Readlink(ctx, n.key)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	return []byte(target), 0
}

func (n *Node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	tn, ok := target.(*Node)
	if !ok {
		return nil, syscall.EINVAL
	}
	attr, err := n.fsys.Link(ctx, n.key, name, tn.key, tn.typ)
	if err != nil {
		return nil, errnoFromErr(err)
	}
	fillAttr(&out.Attr, tn.key, tn.typ, attr)
	return n.newChild(ctx, tn.key, tn.typ), 0
}

// FileHandle adapts icbfs.OpenFile (the shared buffering/CAS-retry
// logic — see its doc comment) to go-fuse's FileHandle interfaces.
type FileHandle struct {
	open *icbfs.OpenFile
}

var (
	_ fs.FileHandle   = (*FileHandle)(nil)
	_ fs.FileReader   = (*FileHandle)(nil)
	_ fs.FileWriter   = (*FileHandle)(nil)
	_ fs.FileFlusher  = (*FileHandle)(nil)
	_ fs.FileGetlker  = (*FileHandle)(nil)
	_ fs.FileSetlker  = (*FileHandle)(nil)
	_ fs.FileSetlkwer = (*FileHandle)(nil)
)

func (h *FileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	return fuse.ReadResultData(h.open.ReadAt(off, int64(len(dest)))), 0
}

func (h *FileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.open.WriteAt(data, off)
	return uint32(len(data)), 0
}

func (h *FileHandle) Flush(ctx context.Context) syscall.Errno {
	_, err := h.open.Flush(ctx)
	return errnoFromErr(err)
}

// fcntlLockEOF is the fuse.FileLock.End sentinel the kernel sends for
// "to end of file" (len == 0 in the original flock_t) — go-fuse's own
// FileLock.FromFlockT maps that case to (1<<63)-1 (math.MaxInt64, the
// largest value an int64 lock offset can hold), confirmed by reading
// that conversion directly rather than assumed.
const fcntlLockEOF = math.MaxInt64

// lockLeaseTTL is the lease every FUSE-sourced flock/fcntl claim gets
// (task B8). Real POSIX locks have no TTL at all — held until
// explicitly unlocked or the owning fd/process goes away — but every
// claim in this codebase's Locking model requires one (see
// ARCHITECTURE.md's Locking section). This project doesn't implement
// background lease renewal for the duration an application holds a
// real fcntl/flock lock, so a lock held longer than this TTL without
// being renewed can be silently stolen by another claimant — a known,
// accepted gap (see ASSUMPTIONS.md), not a guarantee this TTL is
// trying to approximate. 24 hours is chosen to make that gap
// practically unreachable for ordinary use rather than to model any
// real semantics.
const lockLeaseTTL = 24 * time.Hour

// lockHolderFromOwner turns a FUSE lock owner token (kernel-assigned,
// stable for the lifetime of one open-file-description's/process's
// locking context, per fcntl(2)/flock(2)) into this codebase's opaque
// holder string.
func lockHolderFromOwner(owner uint64) string {
	return fmt.Sprintf("fuse-owner-%d", owner)
}

// lockRangeFromFileLock translates a fuse.FileLock into this codebase's
// [start, end) exclusive-end convention. For an flock(2)-style request
// (the FUSE_LK_FLOCK flag), the range is always the whole file,
// matching go-fuse's own reference LoopbackFile implementation, which
// ignores lk.Start/End entirely in that case. For an fcntl(2)-style
// request, lk.End is inclusive (confirmed via FileLock.ToFlockT/
// FromFlockT) with fcntlLockEOF meaning "to EOF" — mapped directly to
// lockWholeFileEnd-equivalent rather than lk.End+1, which would
// overflow.
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
	if lk.Typ == syscall.F_UNLCK {
		out.Typ = syscall.F_UNLCK
		return 0
	}
	start, end := lockRangeFromFileLock(lk, flags)
	// The query's own requested type matters here, real fcntl(F_GETLK)
	// semantics: querying as F_RDLCK should not report another
	// holder's shared claim as a conflict, only an exclusive one —
	// FindConflictingLockRange's querySharedType applies the same
	// locksConflict rule TryAcquireLockRange/TryAcquireSharedLockRange
	// use when actually acquiring.
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

// setlk is Setlk/Setlkw's shared implementation. F_RDLCK routes to the
// shared (read) lock primitives, F_WRLCK to the exclusive (write)
// ones — real POSIX fcntl/flock semantics: multiple different
// holders can hold overlapping F_RDLCK claims at once, but any
// F_WRLCK claim conflicts with every other holder's claim regardless
// of its type.
func (h *FileHandle) setlk(ctx context.Context, owner uint64, lk *fuse.FileLock, flags uint32, blocking bool) syscall.Errno {
	start, end := lockRangeFromFileLock(lk, flags)
	holder := lockHolderFromOwner(owner)
	switch lk.Typ {
	case syscall.F_UNLCK:
		return errnoFromErr(h.open.ReleaseLockRange(ctx, start, end, holder))
	case syscall.F_RDLCK:
		if blocking {
			return errnoFromErr(h.open.AcquireSharedLockRange(ctx, start, end, holder, lockLeaseTTL))
		}
		err := h.open.TryAcquireSharedLockRange(ctx, start, end, holder, lockLeaseTTL)
		if errors.Is(err, icbfs.ErrLocked) {
			return syscall.EAGAIN
		}
		return errnoFromErr(err)
	case syscall.F_WRLCK:
		if blocking {
			return errnoFromErr(h.open.AcquireLockRange(ctx, start, end, holder, lockLeaseTTL))
		}
		err := h.open.TryAcquireLockRange(ctx, start, end, holder, lockLeaseTTL)
		if errors.Is(err, icbfs.ErrLocked) {
			return syscall.EAGAIN
		}
		return errnoFromErr(err)
	default:
		return syscall.EINVAL
	}
}
