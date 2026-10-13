package fuseserver

import (
	"context"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/fslog"
)

// Logger receives the actions a mount logs — fslog.Client, relaying to
// the logging daemon. Log may block (synchronous logging waits until
// the server has written the event), and the operation waits with it.
type Logger interface {
	Log(op, path string, bytes int64, uid, gid, pid uint32)
}

// Option configures Root.
type Option func(*Node)

// WithLogger makes the mount log, to l: a read when a file is opened for
// reading, a write when a file that was written is closed (with its
// size), a delete for each unlink and rmdir, and an ls for each
// directory listing.
func WithLogger(l Logger) Option {
	return func(n *Node) { n.logger = l }
}

// path is n's path from the mount's root, "/" for the root itself.
func (n *Node) path() string {
	return "/" + n.Path(nil)
}

// childPath is the path of name inside directory n.
func (n *Node) childPath(name string) string {
	if p := n.Path(nil); p != "" {
		return "/" + p + "/" + name
	}
	return "/" + name
}

// logEvent logs op on path by ctx's caller, if this mount logs.
func (n *Node) logEvent(ctx context.Context, op, path string, bytes int64) {
	if n.logger == nil {
		return
	}
	var uid, gid, pid uint32
	if caller, ok := fuse.FromContext(ctx); ok {
		uid, gid, pid = caller.Uid, caller.Gid, caller.Pid
	}
	n.logger.Log(op, path, bytes, uid, gid, pid)
}

var _ Logger = (*fslog.Client)(nil)
