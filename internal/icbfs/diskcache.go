package icbfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// defaultCacheMaxBytes is the default total local disk budget for the
// shared read cache (ROADMAP.md's Part G, task G3) — a sensible
// default is required, not optional, since caching here isn't an
// opt-in feature the way Locking/Change-notifications are: every
// OpenFile session goes through it. 512 MiB is comfortably enough to
// avoid redundant re-fetches for ordinary interactive/dev use without
// committing to a large default footprint on a dev machine; a real
// deployment is expected to size this explicitly via SetCacheOptions
// (cmd/icbfs's -cache-size flag) for its own workload.
const defaultCacheMaxBytes = 512 << 20

// diskCache is a shared, LRU-bounded cache of validated byte
// snapshots, one per content key, on local disk — the replacement for
// materializing a file's entire content as one in-memory []byte on
// every single Open (the root cause of three real crash/OOM bugs this
// project already found and fixed the hard way; see
// maxSetAttrSize's own doc comment). Deliberately NOT the thing an
// OpenFile session reads/writes against directly: each session gets
// its own private scratch file (seedScratch), copied from this
// cache's validated snapshot (or fetched fresh, if missing/stale).
// Keeping the shared cache's own files immutable-once-written and
// read-only from every session's perspective is what makes this safe
// under concurrent opens without the far harder problem of letting
// multiple sessions' in-progress, uncommitted edits share one
// physical file — this project's existing CAS-retry-and-reapply
// correctness (task B1) depends on each session's edits staying
// invisible to every other session until Flush, exactly the isolation
// a shared mutable file would break.
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

// evictLocked removes the oldest shared entries (deleting their local
// files) until totalBytes is back within budget, skipping newKey
// itself (the entry this eviction pass is making room for — evicting
// what was just written would be pointless).
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

// newScratchFile creates a fresh, private local scratch file for one
// OpenFile session — never shared with any other session, never
// reused across sessions, removed by the caller (OpenFile.Close) once
// the session ends.
func (c *diskCache) newScratchFile() (*os.File, error) {
	if err := os.MkdirAll(c.scratchDir(), 0700); err != nil {
		return nil, err
	}
	return os.CreateTemp(c.scratchDir(), "scratch-*")
}

// seedScratch returns a fresh private scratch file seeded with key's
// content as of wantETag: copied from this cache's own shared
// snapshot if it's already validated against wantETag, or fetched
// fresh (via fetch, called at most once) and used to populate both
// the new scratch file and the shared cache otherwise. Either way, no
// caller of this function ever needs to hold the full content as a
// single in-memory []byte — fetch streams directly onto local disk via
// io.Copy, which uses a small fixed internal buffer regardless of file
// size.
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
			// A validated shared entry failed to actually read back
			// (e.g. deleted out from under us by a concurrent evict) —
			// fall through to a real fetch rather than erroring out
			// outright; the cache is an optimization, not a dependency.
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
		// Same reasoning as the read-back failure above: a failure to
		// populate the shared cache never fails the caller's own
		// successful fetch — it only means the next Open won't get a
		// cache hit either, no worse than if this cache didn't exist.
		_, _ = scratch.Seek(0, io.SeekStart)
	}
	return scratch, size, nil
}

// updateSharedFrom copies size bytes from content (left seeked to
// wherever the caller wants copying to start) into the shared cache
// entry for key, replacing whatever was there, then restores
// content's position to the start — used both by seedScratch (to
// populate the cache after a real fetch) and by OpenFile.Flush (to
// keep the shared cache representing what was actually just written,
// so the very next Open doesn't redundantly re-fetch it).
func (c *diskCache) updateSharedFrom(key, etag string, content *os.File, size int64) error {
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

// invalidate drops key's shared cache entry outright (no replacement)
// — used when a write fails partway through in a way that leaves no
// trustworthy new content to cache (simpler and safer than caching
// something that might not match what's actually stored).
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
