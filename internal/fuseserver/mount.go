package fuseserver

import (
	"context"
	"sync"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Mount is fs.Mount, with two differences.
//
// A close's FLUSH tells FileHandle.Flush whose close it is (flushOwner),
// which go-fuse doesn't pass on. POSIX releases a process's fcntl locks on
// a file when it closes any descriptor for it, and the kernel leaves that
// to the filesystem: FLUSH carries the closing process's lock owner, the
// same token its fcntl calls carry.
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

// ownerRawFS records each FLUSH's lock owner under its request's cancel
// channel while go-fuse handles it. The context go-fuse gives
// FileHandle.Flush carries that channel, and no other request in flight
// has the same one.
type ownerRawFS struct {
	fuse.RawFileSystem
}

var flushOwners sync.Map // <-chan struct{} -> uint64

func (r *ownerRawFS) Flush(cancel <-chan struct{}, in *fuse.FlushIn) fuse.Status {
	flushOwners.Store(cancel, in.LockOwner)
	defer flushOwners.Delete(cancel)
	return r.RawFileSystem.Flush(cancel, in)
}

// flushOwner is the lock owner of the close ctx is for, if the mount was
// made with Mount.
func flushOwner(ctx context.Context) (uint64, bool) {
	fc, ok := ctx.(*fuse.Context)
	if !ok || fc.Cancel == nil {
		return 0, false
	}
	v, ok := flushOwners.Load(fc.Cancel)
	if !ok {
		return 0, false
	}
	return v.(uint64), true
}
