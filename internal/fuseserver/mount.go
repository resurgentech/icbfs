package fuseserver

import (
	"context"
	"sync"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// Mount is fs.Mount, with three differences.
//
// A close's FLUSH tells FileHandle.Flush whose close it is (flushOwner),
// which go-fuse doesn't pass on. POSIX releases a process's fcntl locks on
// a file when it closes any descriptor for it, and the kernel leaves that
// to the filesystem: FLUSH carries the closing process's lock owner, the
// same token its fcntl calls carry.
//
// The kernel leaves the umask to the filesystem (CAP_DONT_MASK), and each
// create's umask reaches the Node (createUmask), which go-fuse doesn't pass
// on either. POSIX ignores the umask for an item created in a directory
// with a default ACL; the kernel can't tell, as the default ACL is ours.
//
// And the kernel keeps an open file's cached pages until the file is next
// opened (ExplicitDataCacheControl). Otherwise, with no attribute caching,
// it asks for the attributes before every read to see whether its pages
// are stale, a store round trip each: a program reading 4 bytes at a time
// ran a thousand times slower. Those checks bought nothing, since a
// session reads the content as it was when the file was opened, and the
// next open still starts from the store (close-to-open, as NFS), and
// change notifications still invalidate pages at once.
func Mount(dir string, root fs.InodeEmbedder, options *fs.Options) (*fuse.Server, error) {
	var opts fs.Options
	if options != nil {
		opts = *options
	}
	opts.ExplicitDataCacheControl = true
	if rn, ok := root.(*Node); ok && opts.RootStableAttr == nil {
		// The root's inode number, as every other node's, is its key's;
		// left unset, go-fuse reports it as 0.
		opts.RootStableAttr = &fs.StableAttr{Ino: icbfs.Ino(rn.key)}
	}
	opts.ExtraCapabilities |= fuse.CAP_DONT_MASK
	raw := &ownerRawFS{RawFileSystem: fs.NewNodeFS(root, &opts)}
	server, err := fuse.NewServer(raw, dir, &opts.MountOptions)
	if err != nil {
		return nil, err
	}
	go server.Serve()
	if err := server.WaitMount(); err != nil {
		return nil, err
	}
	return server, nil
}

// ownerRawFS records what go-fuse drops from a request, a FLUSH's lock
// owner or a create's umask, under the request's cancel channel while
// go-fuse handles it. The context go-fuse gives the handler carries that
// channel, and no other request in flight has the same one.
type ownerRawFS struct {
	fuse.RawFileSystem
}

var requestExtras sync.Map // <-chan struct{} -> uint64

func (r *ownerRawFS) Flush(cancel <-chan struct{}, in *fuse.FlushIn) fuse.Status {
	requestExtras.Store(cancel, in.LockOwner)
	defer requestExtras.Delete(cancel)
	return r.RawFileSystem.Flush(cancel, in)
}

func (r *ownerRawFS) Mkdir(cancel <-chan struct{}, in *fuse.MkdirIn, name string, out *fuse.EntryOut) fuse.Status {
	requestExtras.Store(cancel, uint64(in.Umask))
	defer requestExtras.Delete(cancel)
	return r.RawFileSystem.Mkdir(cancel, in, name, out)
}

func (r *ownerRawFS) Mknod(cancel <-chan struct{}, in *fuse.MknodIn, name string, out *fuse.EntryOut) fuse.Status {
	requestExtras.Store(cancel, uint64(in.Umask))
	defer requestExtras.Delete(cancel)
	return r.RawFileSystem.Mknod(cancel, in, name, out)
}

func (r *ownerRawFS) Create(cancel <-chan struct{}, in *fuse.CreateIn, name string, out *fuse.CreateOut) fuse.Status {
	requestExtras.Store(cancel, uint64(in.Umask))
	defer requestExtras.Delete(cancel)
	return r.RawFileSystem.Create(cancel, in, name, out)
}

// requestExtra is what ownerRawFS recorded for the request ctx is for.
func requestExtra(ctx context.Context) (uint64, bool) {
	fc, ok := ctx.(*fuse.Context)
	if !ok || fc.Cancel == nil {
		return 0, false
	}
	v, ok := requestExtras.Load(fc.Cancel)
	if !ok {
		return 0, false
	}
	return v.(uint64), true
}

// flushOwner is the lock owner of the close ctx is for, if the mount was
// made with Mount.
func flushOwner(ctx context.Context) (uint64, bool) { return requestExtra(ctx) }

// createUmask is the umask of the create ctx is for, if the mount was made
// with Mount.
func createUmask(ctx context.Context) (uint32, bool) {
	v, ok := requestExtra(ctx)
	return uint32(v), ok
}
