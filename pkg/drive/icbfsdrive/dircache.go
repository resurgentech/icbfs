package icbfsdrive

import (
	"container/list"
	"context"
	"hash/maphash"
	"io"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// DefaultDirCacheBytes is Config.DirCacheBytes's default: 4 MiB by
// icbfs.Attr.HeldSize, which measured at 0.6 of the heap a cached
// folder holds, so up to about 7 MiB of heap. T80-1 measured 4 to 11 KB
// of heap a cached folder (its block's entries, its .metadata, both
// objects' descriptions), so it holds the 600 to 1,000 folders a mount
// or a daemon last walked. A path's ancestors, read at every step of
// every resolve, are the most recently used and stay. A rebuild of a
// mirror's map, which reads each file's path from the root, holds that
// much at most, where with no bound in bytes it held every folder of
// the tree (6.2 MiB at 1,500 folders; 30 to 65 MB predicted at the
// full-size mirror's 4,694), against T80's 100 MB for a whole first
// pass.
const DefaultDirCacheBytes = 4 << 20

// dirCache remembers directories as a walk down a path last read them:
// their attributes, which hold the block and .metadata they came from.
// Config.TrustFor says how long one is used without asking; after that,
// or at once when it's 0, it's revalidated (icbfs Revalidate: two
// conditional reads, no bodies if nothing changed).
//
// It's bounded by bytes (maxBytes, by icbfs.Attr.HeldSize): past it,
// the least recently used go, but never the one just put, which a walk
// is about to use. A directory evicted is read again when it's next
// needed; what a caller holds of it stays usable (an Attr is a value).
//
// Every write through the drive's store drops what it wrote (dropOnWrite),
// so the drive sees its own changes at once, whatever TrustFor says. A
// read can't put back what a write it overlapped dropped: each write
// bumps a generation for its key's bucket, and a read caches its result
// only if its bucket's generation hasn't moved since it began.
type dirCache struct {
	trust    time.Duration
	maxBytes int64
	seed     maphash.Seed

	mu    sync.Mutex
	nodes map[string]*list.Element // of *cachedDir, most recently used at the front
	lru   list.List
	bytes int64
	gens  [64]uint64
}

// cachedDirSelf is a cachedDir's own bytes besides its Attr's
// HeldSize: its key, its list element and its map slot.
const cachedDirSelf = 160

type cachedDir struct {
	key     string
	attr    icbfs.Attr
	checked time.Time
	size    int64
}

// newDirCache is a cache trusting what it read for trust, of at most
// maxBytes (0: DefaultDirCacheBytes).
func newDirCache(trust time.Duration, maxBytes int64) *dirCache {
	if maxBytes <= 0 {
		maxBytes = DefaultDirCacheBytes
	}
	return &dirCache{trust: trust, maxBytes: maxBytes, seed: maphash.MakeSeed(), nodes: map[string]*list.Element{}}
}

func (c *dirCache) bucket(key string) int {
	return int(maphash.String(c.seed, key) % uint64(len(c.gens)))
}

// gen is key's bucket's generation now, for a put after a read.
func (c *dirCache) gen(key string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gens[c.bucket(key)]
}

// get is key's directory, if it's held, now the most recently used.
func (c *dirCache) get(key string) (cachedDir, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.nodes[key]
	if !ok {
		return cachedDir{}, false
	}
	c.lru.MoveToFront(el)
	return *el.Value.(*cachedDir), true
}

// put keeps attr as key's, checked at checked, unless a write to its
// bucket happened since gen; then drops the least recently used others
// until the cache is within its bound.
func (c *dirCache) put(key string, attr icbfs.Attr, checked time.Time, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gens[c.bucket(key)] != gen {
		return
	}
	c.remove(key)
	d := &cachedDir{key: key, attr: attr, checked: checked, size: attr.HeldSize() + cachedDirSelf}
	c.nodes[key] = c.lru.PushFront(d)
	c.bytes += d.size
	for c.bytes > c.maxBytes && c.lru.Len() > 1 {
		c.remove(c.lru.Back().Value.(*cachedDir).key)
	}
}

// remove forgets key, if it's held. c.mu is held.
func (c *dirCache) remove(key string) {
	if el, ok := c.nodes[key]; ok {
		c.bytes -= el.Value.(*cachedDir).size
		c.lru.Remove(el)
		delete(c.nodes, key)
	}
}

// drop forgets key, and keeps a read that overlapped the write from
// putting it back.
func (c *dirCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gens[c.bucket(key)]++
	c.remove(key)
}

// stats is how many directories the cache holds, and their bytes as
// HeldSize estimates them.
func (c *dirCache) stats() (n int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.nodes), c.bytes
}

func (c *dirCache) dropAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.gens {
		c.gens[i]++
	}
	clear(c.nodes)
	c.lru.Init()
	c.bytes = 0
}

// dirAttr is directory key's attributes for a walk, holding its block:
// from the cache within TrustFor, else revalidated or read, and cached.
// Only the live filesystem's are cached; a view of the past is read.
func (d *Drive) dirAttr(ctx context.Context, fsys *icbfs.Filesystem, key string) (icbfs.Attr, error) {
	if fsys != d.fs {
		return fsys.StatStep(ctx, key, icbfs.TypeDir)
	}
	gen := d.dirs.gen(key)
	cached, ok := d.dirs.get(key)
	now := time.Now()
	if ok && d.dirs.trust > 0 && now.Sub(cached.checked) < d.dirs.trust {
		return cached.attr, nil
	}
	var attr icbfs.Attr
	var err error
	if ok {
		attr, _, err = fsys.Revalidate(ctx, key, cached.attr)
	} else {
		attr, err = fsys.StatStep(ctx, key, icbfs.TypeDir)
	}
	if err != nil {
		d.dirs.drop(key)
		return icbfs.Attr{}, err
	}
	d.dirs.put(key, attr, now, gen)
	return attr, nil
}

// dropOnWrite is the drive's store: every write drops what it wrote from
// the directory cache, once it's done.
type dropOnWrite struct {
	objstore.Store
	dirs *dirCache
}

func (s *dropOnWrite) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	defer s.dirs.drop(objstore.BaseKey(key))
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func (s *dropOnWrite) PutIfAbsent(ctx context.Context, key string, body io.Reader, md map[string]string) (*objstore.Object, error) {
	defer s.dirs.drop(objstore.BaseKey(key))
	return s.Store.PutIfAbsent(ctx, key, body, md)
}

func (s *dropOnWrite) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	defer s.dirs.drop(objstore.BaseKey(key))
	return s.Store.PutParts(ctx, key, parts, md, ifMatch)
}

func (s *dropOnWrite) Delete(ctx context.Context, key string) error {
	defer s.dirs.drop(objstore.BaseKey(key))
	return s.Store.Delete(ctx, key)
}

func (s *dropOnWrite) Undelete(ctx context.Context, key string) error {
	defer s.dirs.drop(objstore.BaseKey(key))
	return s.Store.Undelete(ctx, key)
}

func (s *dropOnWrite) Erase(ctx context.Context, key, versionID string) error {
	defer s.dirs.drop(objstore.BaseKey(key))
	return s.Store.Erase(ctx, key, versionID)
}

func (s *dropOnWrite) EraseAll(ctx context.Context, prefix string) error {
	defer s.dirs.dropAll()
	return s.Store.EraseAll(ctx, prefix)
}
