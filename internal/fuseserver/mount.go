package fuseserver

import (
	"context"
	"sync"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Mount is fs.Mount, except that a close's FLUSH tells FileHandle.Flush
// whose close it is (flushOwner), which go-fuse doesn't pass on. POSIX
// releases a process's fcntl locks on a file when it closes any descriptor
// for it, and the kernel leaves that to the filesystem: FLUSH carries the
// closing process's lock owner, the same token its fcntl calls carry.
func Mount(dir string, root fs.InodeEmbedder, options *fs.Options) (*fuse.Server, error) {
	raw := &ownerRawFS{RawFileSystem: fs.NewNodeFS(root, options)}
	var mountOptions *fuse.MountOptions
	if options != nil {
		mountOptions = &options.MountOptions
	}
	server, err := fuse.NewServer(raw, dir, mountOptions)
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
