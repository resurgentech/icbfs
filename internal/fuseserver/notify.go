package fuseserver

import (
	"strings"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/resurgentech/icbfs/internal/notify"
)

// watchRegistry maps the keys this mount has looked up to their live
// go-fuse inodes, so a signal naming a key can be passed to the kernel.
//
// register keeps the first entry for a key and never overwrites it.
// NewInode with an explicit Ino always makes a new Inode; go-fuse decides
// later (bridge.addNewChild) which one is canonical, keeping the first.
// Overwriting would point the registry at an inode the kernel never heard
// of (NotifyContent then gets ENOENT).
//
// Entries are never removed, so the map grows with the number of distinct
// items the mount touches, and go-fuse can't release those inodes. Fixing
// that means hooking go-fuse's FORGET handling.
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

// startNotifyDispatch passes each signal from signals to the kernel as a
// NotifyContent on the inode it names, until the channel closes. A signal
// for a key this mount hasn't looked up is dropped.
//
// A <uuid>.metadata key counts as <uuid>, so a chmod elsewhere invalidates
// the same way as a write.
//
// notifyContentHook, when set, sees each NotifyContent's key and result: a
// test seam, nil in production. A NotifyContent produces no inotify event
// on this kernel and go-fuse, and the mount keeps no page cache to go
// stale, so this is how a test sees the signal reach the right inode.
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
