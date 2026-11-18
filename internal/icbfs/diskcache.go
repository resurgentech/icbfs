package icbfs

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/resurgentech/icbfs/pkg/readcache"
)

// defaultCacheMaxBytes is the read cache's default size, 512 MiB. Every
// open file goes through the cache, so there's always one; a deployment
// sizes it with SetCacheOptions (the mounts' -cache-size).
const defaultCacheMaxBytes = readcache.DefaultMaxBytes

// diskCache is a Filesystem's read cache: pkg/readcache (entries by key
// and store version id, an index read from the directory at start,
// fetches in flight shared, safe for several processes on one directory)
// in a directory a caller gave (SetCacheOptions) or in one of its own,
// made in the temp directory by the first use and removed by close
// (cachedir.go). Sessions never use an entry's file directly: each gets a
// private scratch file copied from it (seedScratch), so one session's
// unflushed edits never reach another's, which re-applying after a lost
// race depends on.
type diskCache struct {
	mu       sync.Mutex
	dir      string // the directory; for an own cache "" until the first use
	maxBytes int64
	own      bool
	lock     *os.File // an own directory's lock (cachedir.go)
	closed   bool
	rc       *readcache.Cache
}

func newDiskCache(dir string, maxBytes int64) *diskCache {
	return &diskCache{dir: dir, maxBytes: maxBytes}
}

// newOwnedDiskCache is a Filesystem's own cache: its directory is made
// in the temp directory by the first use and removed by close.
func newOwnedDiskCache(maxBytes int64) *diskCache {
	return &diskCache{own: true, maxBytes: maxBytes}
}

// ready opens the cache's directory at its first use (an own cache makes
// it first); an error for a closed cache.
func (c *diskCache) ready() (*readcache.Cache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errCacheClosed
	}
	if c.rc != nil {
		return c.rc, nil
	}
	if c.own && c.dir == "" {
		dir, lock, err := makeOwnedCacheDir()
		if err != nil {
			return nil, err
		}
		c.dir, c.lock = dir, lock
	}
	// icbfs mount's -cache-dir is not held to the owner-only check
	// (readcache.Options.OwnDir): a setup that worked before still does;
	// cmd/icbfs warns instead.
	rc, err := readcache.Open(c.dir, readcache.Options{MaxBytes: c.maxBytes})
	if err != nil {
		return nil, err
	}
	c.rc = rc
	return rc, nil
}

// close lets go of the cache, and removes an own cache's directory, once;
// later uses fail. A given directory keeps its entries for the next start.
func (c *diskCache) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var err error
	if c.rc != nil {
		err = c.rc.Close()
		c.rc = nil
	}
	if !c.own || c.dir == "" {
		return err
	}
	err = releaseOwnedCacheDir(c.dir, c.lock)
	c.lock = nil
	return err
}

// newScratchFile creates a private scratch file for one session, which the
// session removes (OpenFile.Close).
func (c *diskCache) newScratchFile() (*os.File, error) {
	rc, err := c.ready()
	if err != nil {
		return nil, err
	}
	return rc.NewScratch()
}

// seedScratch returns a new scratch file holding key's content at store
// version version: copied from the cache if it has that version, else
// fetched (fetch must read exactly that version; it is called at most
// once, and an Open of the same version at the same time waits for it)
// into both the scratch file and the cache. With no version (a store that
// keeps none) nothing is cached. The content is streamed to disk, never
// held in memory.
func (c *diskCache) seedScratch(ctx context.Context, key, version string, fetch func(ctx context.Context) (io.ReadCloser, error)) (*os.File, int64, error) {
	rc, err := c.ready()
	if err != nil {
		return nil, 0, err
	}
	if version != "" {
		return rc.Seed(ctx, key, version, -1, fetch)
	}
	scratch, err := rc.NewScratch()
	if err != nil {
		return nil, 0, err
	}
	body, err := fetch(ctx)
	if err == nil {
		var size int64
		size, err = io.Copy(scratch, body)
		body.Close()
		if err == nil {
			if _, err = scratch.Seek(0, io.SeekStart); err == nil {
				return scratch, size, nil
			}
		}
	}
	scratch.Close()
	_ = os.Remove(scratch.Name())
	return nil, 0, err
}

// updateSharedFrom puts size bytes of content in the cache as key's
// entry at store version version, and seeks content back to the start:
// OpenFile.Flush after a write, so the next Open of that version needn't
// fetch. Content larger than the whole budget isn't cached. Best effort.
func (c *diskCache) updateSharedFrom(key, version string, content *os.File, size int64) error {
	var err error
	if version != "" {
		var rc *readcache.Cache
		if rc, err = c.ready(); err == nil {
			err = rc.Put(key, version, content, size)
		}
	}
	if _, serr := content.Seek(0, io.SeekStart); err == nil {
		err = serr
	}
	return err
}
