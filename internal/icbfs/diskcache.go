package icbfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// defaultCacheMaxBytes is the read cache's default size, 512 MiB. Every
// open file goes through the cache, so there's always one; a deployment
// sizes it with SetCacheOptions (the mounts' -cache-size).
const defaultCacheMaxBytes = 512 << 20

// diskCache is a shared LRU cache on local disk of fetched content, one
// version per key. Sessions never use its files directly: each gets a
// private scratch file copied from it (seedScratch), so the cache's files
// are never written once made, and one session's unflushed edits never
// reach another's, which re-applying after a lost race depends on.
type diskCache struct {
	mu       sync.Mutex
	dir      string // lazily created on first real use
	maxBytes int64

	entries    map[string]*cacheEntry
	lru        []string // oldest-first; a key's position is refreshed (moved to the end) on every touch
	totalBytes int64
}

type cacheEntry struct {
	etag string
	size int64
}

func newDiskCache(dir string, maxBytes int64) *diskCache {
	return &diskCache{dir: dir, maxBytes: maxBytes, entries: map[string]*cacheEntry{}}
}

func (c *diskCache) sharedDir() string  { return filepath.Join(c.dir, "shared") }
func (c *diskCache) scratchDir() string { return filepath.Join(c.dir, "scratch") }

func (c *diskCache) sharedPath(key string) string {
	return filepath.Join(c.sharedDir(), key)
}

// touchLocked moves key to the most-recently-used end of the LRU
// order, adding it if not already tracked.
func (c *diskCache) touchLocked(key string) {
	for i, k := range c.lru {
		if k == key {
			c.lru = append(c.lru[:i], c.lru[i+1:]...)
			break
		}
	}
	c.lru = append(c.lru, key)
}

// evictLocked removes the oldest entries, and their files, until the cache
// is within budget, sparing newKey, the entry it's making room for.
func (c *diskCache) evictLocked(newKey string) {
	for c.totalBytes > c.maxBytes && len(c.lru) > 0 {
		oldest := c.lru[0]
		if oldest == newKey {
			if len(c.lru) == 1 {
				break
			}
			oldest = c.lru[1]
			c.lru = append(c.lru[:1], c.lru[2:]...)
		} else {
			c.lru = c.lru[1:]
		}
		entry, ok := c.entries[oldest]
		if !ok {
			continue
		}
		_ = os.Remove(c.sharedPath(oldest))
		c.totalBytes -= entry.size
		delete(c.entries, oldest)
	}
}

// validLocked reports whether key's shared cache entry, if any,
// already reflects wantETag.
func (c *diskCache) validLocked(key, wantETag string) bool {
	entry, ok := c.entries[key]
	return ok && entry.etag == wantETag
}

// newScratchFile creates a private scratch file for one session, which the
// session removes (OpenFile.Close).
func (c *diskCache) newScratchFile() (*os.File, error) {
	if err := os.MkdirAll(c.scratchDir(), 0700); err != nil {
		return nil, err
	}
	return os.CreateTemp(c.scratchDir(), "scratch-*")
}

// seedScratch returns a new scratch file holding key's content as of
// wantETag: copied from the cache if it has that version, else fetched
// (fetch is called at most once) into both the scratch file and the cache.
// The content is streamed to disk, never held in memory.
func (c *diskCache) seedScratch(ctx context.Context, key, wantETag string, fetch func(ctx context.Context) (io.ReadCloser, error)) (*os.File, int64, error) {
	scratch, err := c.newScratchFile()
	if err != nil {
		return nil, 0, err
	}

	c.mu.Lock()
	hit := c.validLocked(key, wantETag)
	c.mu.Unlock()

	if hit {
		if shared, err := os.Open(c.sharedPath(key)); err == nil {
			size, copyErr := io.Copy(scratch, shared)
			shared.Close()
			if copyErr == nil {
				c.mu.Lock()
				c.touchLocked(key)
				c.mu.Unlock()
				if _, err := scratch.Seek(0, io.SeekStart); err != nil {
					scratch.Close()
					_ = os.Remove(scratch.Name())
					return nil, 0, err
				}
				return scratch, size, nil
			}
			// The cached copy didn't read back (evicted meanwhile): fetch
			// instead. The cache only saves work.
			if _, err := scratch.Seek(0, io.SeekStart); err != nil {
				scratch.Close()
				_ = os.Remove(scratch.Name())
				return nil, 0, err
			}
			if err := scratch.Truncate(0); err != nil {
				scratch.Close()
				_ = os.Remove(scratch.Name())
				return nil, 0, err
			}
		}
	}

	body, err := fetch(ctx)
	if err != nil {
		scratch.Close()
		_ = os.Remove(scratch.Name())
		return nil, 0, err
	}
	size, err := io.Copy(scratch, body)
	body.Close()
	if err != nil {
		scratch.Close()
		_ = os.Remove(scratch.Name())
		return nil, 0, err
	}
	if _, err := scratch.Seek(0, io.SeekStart); err != nil {
		scratch.Close()
		_ = os.Remove(scratch.Name())
		return nil, 0, err
	}
	if err := c.updateSharedFrom(key, wantETag, scratch, size); err != nil {
		// A failure to fill the cache doesn't fail the fetch; the next Open
		// just fetches again.
		_, _ = scratch.Seek(0, io.SeekStart)
	}
	return scratch, size, nil
}

// updateSharedFrom puts size bytes of content (from its current position)
// in the cache as key's entry, replacing any, and seeks content back to the
// start. seedScratch uses it after a fetch, and OpenFile.Flush after a
// write, so the next Open needn't fetch.
//
// Content larger than the whole budget isn't cached (it would evict
// everything else and then itself); key's older entry is dropped instead,
// since it no longer matches.
func (c *diskCache) updateSharedFrom(key, etag string, content *os.File, size int64) error {
	if size > c.maxBytes {
		c.invalidate(key)
		_, err := content.Seek(0, io.SeekStart)
		return err
	}
	if err := os.MkdirAll(c.sharedDir(), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.sharedDir(), "tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, content); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, c.sharedPath(key)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}

	c.mu.Lock()
	if old, ok := c.entries[key]; ok {
		c.totalBytes -= old.size
	}
	c.entries[key] = &cacheEntry{etag: etag, size: size}
	c.totalBytes += size
	c.touchLocked(key)
	c.evictLocked(key)
	c.mu.Unlock()
	return nil
}

// invalidate drops key's entry, when a failed write leaves nothing
// trustworthy to cache.
func (c *diskCache) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		_ = os.Remove(c.sharedPath(key))
		c.totalBytes -= entry.size
		delete(c.entries, key)
		for i, k := range c.lru {
			if k == key {
				c.lru = append(c.lru[:i], c.lru[i+1:]...)
				break
			}
		}
	}
}
