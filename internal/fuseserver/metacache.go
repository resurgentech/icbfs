package fuseserver

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// The metadata cache, as the Linux NFS client's attribute and lookup
// caches: the kernel trusts names (found and not found) and attributes for
// a while instead of asking for them again on every path walk and stat,
// each of which is a store round trip. What this mount changes, the
// kernel learns at once; only other machines' changes can take up to the
// timeout to show. Content stays close-to-open: an open always starts
// from the store (revalidateOpen), and locks bring an open file up to date
// (FileHandle.syncAfterLock).

// WithMetaCache makes the mount's nodes revalidate at open what the
// kernel may be trusting from its cache (see revalidateOpen); d is the
// timeout passed to the kernel (MetaCacheTimeouts). Without it, or with
// 0, the kernel caches nothing and there is nothing to revalidate.
func WithMetaCache(d time.Duration) Option {
	return func(n *Node) { n.metaCache = d }
}

// MetaCacheTimeouts sets opts' kernel entry, negative-entry and attribute
// timeouts to d. Mount with it and Root with WithMetaCache(d).
func MetaCacheTimeouts(opts *fs.Options, d time.Duration) {
	opts.EntryTimeout = &d
	opts.NegativeTimeout = &d
	opts.AttrTimeout = &d
}

// revalidateOpen is what an open checks that a cached path walk skipped,
// as NFS's close-to-open does:
//
//   - The name the file was reached by still leads to it. The kernel
//     trusted the name without asking, so another machine may have removed
//     or renamed it, or put another item there. If so, ESTALE: the kernel
//     then walks the path again, asking for every name (LOOKUP_REVAL), and
//     opens whatever the name means now.
//   - The kernel's cached attributes for the file are expired, so its size
//     is fetched again before a read past what it thought was the end: the
//     open's content may be a newer version than the attributes describe,
//     and the kernel ends reads at the size it knows. (Its pages are
//     dropped by the open itself: no FOPEN_KEEP_CACHE.)
//
// A file with more than one link isn't checked by name: which of its names
// the open came through isn't known.
func (n *Node) revalidateOpen(ctx context.Context) syscall.Errno {
	if n.metaCache <= 0 {
		return 0
	}
	if last := n.last.Load(); last == nil || last.Nlink <= 1 {
		if name, parent := n.Parent(); parent != nil {
			if p, ok := parent.Operations().(*Node); ok {
				entry, _, err := n.fsys.Lookup(ctx, p.key, name)
				switch {
				case errors.Is(err, icbfs.ErrNotFound):
					return syscall.ESTALE
				case err != nil:
					return errnoFromErr(err)
				case entry.UUID != n.key:
					return syscall.ESTALE
				}
			}
		}
	}
	n.NotifyContent(-1, 0) // attributes only
	return 0
}
