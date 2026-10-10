// Package fuseserver is the FUSE access layer: it adapts icbfs's core
// filesystem logic to go-fuse's node-tree API. Linux/POSIX only — the
// Windows access layer (WinFsp) is a separate, parallel package per
// ARCHITECTURE.md.
package fuseserver

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// Node is one filesystem entry: either the root, or identified by its
// blob's UUID (key). Its type (file/dir/symlink) is fixed for the inode's
// lifetime, matching ARCHITECTURE.md's model where identity never changes.
type Node struct {
	fs.Inode
	fsys *icbfs.Filesystem
	key  string
	typ  icbfs.EntryType
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
)

// Root builds the node representing fsys's root directory, for use with
// fs.Mount.
func Root(fsys *icbfs.Filesystem) *Node {
	return &Node{fsys: fsys, key: fsys.RootKey(), typ: icbfs.TypeDir}
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
	default:
		return syscall.EIO
	}
}

func (n *Node) newChild(ctx context.Context, key string, typ icbfs.EntryType) *fs.Inode {
	child := &Node{fsys: n.fsys, key: key, typ: typ}
	stable := fs.StableAttr{Ino: icbfs.Ino(key), Mode: typeToFuseMode(typ)}
	// StableAttr.Ino dedups against any already-known inode with the same
	// number — this is exactly how hardlinks (Link, below) end up sharing
	// one Inode across multiple directory entries.
	return n.NewInode(ctx, child, stable)
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
	newUUID, attr, err := n.fsys.Create(ctx, n.key, name, mode&0o7777, uid, gid)
	if err != nil {
		return nil, nil, 0, errnoFromErr(err)
	}
	fillAttr(&out.Attr, newUUID, icbfs.TypeFile, attr)
	fh := &FileHandle{fsys: n.fsys, key: newUUID}
	return n.newChild(ctx, newUUID, icbfs.TypeFile), fh, 0, 0
}

func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	data, _, err := n.fsys.ReadFile(ctx, n.key)
	if err != nil {
		return nil, 0, errnoFromErr(err)
	}
	return &FileHandle{fsys: n.fsys, key: n.key, data: data}, 0, 0
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

// FileHandle buffers one open file's content in memory and flushes it back
// as a single whole-object write on close — matching the "no chunking, an
// object is an object" model: there is no partial/range write against the
// store, so there is no reason to do partial writes here either.
type FileHandle struct {
	mu    sync.Mutex
	fsys  *icbfs.Filesystem
	key   string
	data  []byte
	dirty bool
}

var (
	_ fs.FileHandle  = (*FileHandle)(nil)
	_ fs.FileReader  = (*FileHandle)(nil)
	_ fs.FileWriter  = (*FileHandle)(nil)
	_ fs.FileFlusher = (*FileHandle)(nil)
)

func (h *FileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if off >= int64(len(h.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := off + int64(len(dest))
	if end > int64(len(h.data)) {
		end = int64(len(h.data))
	}
	return fuse.ReadResultData(h.data[off:end]), 0
}

func (h *FileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	end := off + int64(len(data))
	if end > int64(len(h.data)) {
		grown := make([]byte, end)
		copy(grown, h.data)
		h.data = grown
	}
	copy(h.data[off:end], data)
	h.dirty = true
	return uint32(len(data)), 0
}

func (h *FileHandle) Flush(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.dirty {
		return 0
	}
	if _, err := h.fsys.WriteFile(ctx, h.key, h.data); err != nil {
		return errnoFromErr(err)
	}
	h.dirty = false
	return 0
}
