package icbfsdrive

import (
	"context"
	"hash/maphash"
	"io"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// maxCachedDirs bounds the directory cache: past it, an entry goes for
// each one added.
const maxCachedDirs = 50000

// dirCache remembers directories as a walk down a path last read them:
// their attributes, which hold the block and .metadata they came from.
// Config.TrustFor says how long one is used without asking; after that,
// or at once when it's 0, it's revalidated (icbfs Revalidate: two
// conditional reads, no bodies if nothing changed).
//
// Every write through the drive's store drops what it wrote (dropOnWrite),
// so the drive sees its own changes at once, whatever TrustFor says. A
// read can't put back what a write it overlapped dropped: each write
// bumps a generation for its key's bucket, and a read caches its result
// only if its bucket's generation hasn't moved since it began.
type dirCache struct {
	trust time.Duration
	seed  maphash.Seed

	mu    sync.Mutex
	nodes map[string]cachedDir
	gens  [64]uint64
}

type cachedDir struct {
	attr    icbfs.Attr
	checked time.Time
}

func newDirCache(trust time.Duration) *dirCache {
	return &dirCache{trust: trust, seed: maphash.MakeSeed(), nodes: map[string]cachedDir{}}
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

func (c *dirCache) get(key string) (cachedDir, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.nodes[key]
	return d, ok
}

// put keeps attr as key's, checked at checked, unless a write to its
// bucket happened since gen.
func (c *dirCache) put(key string, attr icbfs.Attr, checked time.Time, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gens[c.bucket(key)] != gen {
		return
	}
	if _, ok := c.nodes[key]; !ok && len(c.nodes) >= maxCachedDirs {
		for k := range c.nodes {
			delete(c.nodes, k)
			break
		}
	}
	c.nodes[key] = cachedDir{attr: attr, checked: checked}
}

// drop forgets key, and keeps a read that overlapped the write from
// putting it back.
func (c *dirCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gens[c.bucket(key)]++
	delete(c.nodes, key)
}

func (c *dirCache) dropAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.gens {
		c.gens[i]++
	}
	clear(c.nodes)
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
