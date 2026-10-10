package fuseserver

import (
	"strings"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/resurgentech/icbfs/internal/notify"
)

// watchRegistry maps a UUID key this mount has looked up to the live
// go-fuse Inode representing it, so an incoming notify.Signal naming
// that key can be turned into a real kernel notification (task E3).
//
// register is first-writer-wins (never overwrites an existing entry)
// for a real, verified-not-assumed reason: (*fs.Inode).NewInode does
// NOT deduplicate by StableAttr.Ino itself when an explicit Ino is
// given (confirmed by reading newInodeUnlocked directly) — it always
// allocates a fresh Inode wrapper. The actual canonical-node
// resolution happens later, inside go-fuse's own bridge.addNewChild/
// addNewNode (called by the framework *after* our Lookup/Create
// method returns), which discards our freshly-built Inode in favor of
// whichever one was registered first, if this Ino was already known.
// Registering unconditionally on every newChild call would therefore
// overwrite an already-correct entry with a reference to an Inode the
// kernel was never actually told about — confirmed empirically: a
// second lookup of an already-known file produced a NotifyContent
// call against a stale, kernel-unknown nodeId and a literal ENOENT
// from the kernel. First-writer-wins is correct because the very
// first newChild call for any given Ino is, by construction, always
// the one go-fuse's own dedup lets win (nothing else was registered
// yet to lose to).
//
// Per ROADMAP.md's task E3 scope, this intentionally does not solve
// everything else: entries are never removed, which means (a) the map
// grows for as long as the mount runs, bounded only by how many
// distinct objects get accessed, and (b) holding a reference here
// prevents go-fuse from ever fully releasing that Inode. Both are
// accepted, documented gaps for a first cut — proving delivery works
// (this task's actual "Done when") doesn't require solving inode
// lifecycle/pruning, and doing so properly would need hooking into
// go-fuse's FORGET handling, not attempted here. Flagged for whoever
// revisits this, same treatment ROADMAP.md itself gives the separate,
// still-open rename wrinkle.
type watchRegistry struct {
	mu    sync.Mutex
	byKey map[string]*fs.Inode
}

func newWatchRegistry() *watchRegistry {
	return &watchRegistry{byKey: make(map[string]*fs.Inode)}
}

func (w *watchRegistry) register(key string, inode *fs.Inode) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.byKey[key]; exists {
		return
	}
	w.byKey[key] = inode
}

func (w *watchRegistry) lookup(key string) (*fs.Inode, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	inode, ok := w.byKey[key]
	return inode, ok
}

// startNotifyDispatch consumes source's signal channel for as long as
// the mount runs (or until the channel closes), translating each
// signal into a real go-fuse/kernel notification for whichever
// currently-tracked UUID it names. A signal naming a key nobody has
// looked up yet (so there's no live Inode to notify) is simply
// dropped — consistent with ARCHITECTURE.md's governing principle
// that a signal is just a "go check current state" hint, never
// something a consumer is required to act on.
//
// A signal naming a <uuid>.metadata key is matched against the same
// registry entry as <uuid> itself (the suffix is stripped before
// lookup) — an attribute-only change (chmod/chown from elsewhere)
// still invalidates cached content via the same NotifyContent call, a
// deliberately coarse approximation rather than a separate
// attribute-change notification path.
//
// notifyContentHook, when non-nil, is called with each dispatched
// NotifyContent attempt's key and result. nil in production — Root
// never sets it. This exists purely as a test seam: verified directly
// (not assumed) that a successful NotifyContent call here does not
// reliably produce a raw inotify event through this exact kernel/
// go-fuse version in this environment, and this filesystem doesn't
// opt into FOPEN_KEEP_CACHE, so there's no page-cache staleness to
// observe either — see ASSUMPTIONS.md's E3 entry. This hook lets a
// test confirm the actual deliverable (the signal correctly reached
// the kernel against the right, live Inode, accepted without error)
// without depending on either of those unreliable, environment-
// dependent side effects.
var notifyContentHook func(key string, errno syscall.Errno)

func startNotifyDispatch(signals <-chan notify.Signal, watches *watchRegistry) {
	go func() {
		for sig := range signals {
			key := strings.TrimSuffix(sig.Key, ".metadata")
			inode, ok := watches.lookup(key)
			if !ok {
				continue
			}
			errno := inode.NotifyContent(0, 0)
			if notifyContentHook != nil {
				notifyContentHook(key, errno)
			}
		}
	}()
}
