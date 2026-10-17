// Package namecache remembers, for a short while, what a directory's names
// lead to, for an access layer that resolves paths itself: WinFsp hands
// the Windows driver a whole path on every open and keeps no names of its
// own, so without this each open walks the path from the root again, a
// store round trip or more per component, twice (the access check, then
// the open). Linux mounts don't need it: the kernel's dentry and attribute
// caches do this there.
//
// An entry is keyed by directory id and name. Ids never change, so a
// directory renamed leaves everything under it valid: only the entry for
// its old name is stale. Entries are found ("name → item, with its
// attributes") or not ("no such name"), and expire after the cache's
// time to live, the mount's -meta-cache. A directory listed whole also
// answers "no such name" for anything not in the listing, until it too
// expires: what Explorer's probes for desktop.ini and the like need.
//
// What this machine changes it changes here at once (Put, Forget,
// ForgetItem); another machine's changes show when entries expire. The
// cache holds at most a set number of bytes (an estimate of each entry's
// size), dropping expired entries as it goes and the least recently used
// when full, so it stays small: at a one-second time to live, about what
// was touched in the last second.
package namecache

import (
	"container/list"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
)

// Value is what a name leads to: an item and its attributes, or nothing
// (Found false).
type Value struct {
	Found bool
	Entry block.Entry
	Attr  icbfs.Attr
}

type key struct{ dir, name string }

type entry struct {
	k       key
	v       Value
	expires time.Time
	size    int64
	elem    *list.Element
}

// Cache is safe for concurrent use. The zero value isn't usable; see New.
type Cache struct {
	ttl      time.Duration
	maxBytes int64
	fold     func(string) string
	now      func() time.Time

	mu       sync.Mutex
	entries  map[key]*entry
	byItem   map[string]map[key]bool // item id → the keys naming it
	listed   map[string]time.Time    // directory id → when its whole listing expires
	lru      *list.List              // *entry, least recently used first
	bytes    int64
	inflight map[key]*call
	// gen counts this machine's changes (Put, Forget, ForgetItem,
	// ForgetDir). A lookup or listing that read the store before a change
	// doesn't store what it read, which may be from before the change.
	gen uint64
}

type call struct {
	done chan struct{}
	v    Value
	err  error
}

// New is a cache whose entries live for ttl, holding at most maxBytes.
// fold maps a name to its key: block.FoldKey for case-insensitive lookups,
// nil for exact ones. A ttl of 0 or less caches nothing.
func New(ttl time.Duration, maxBytes int64, fold func(string) string) *Cache {
	if fold == nil {
		fold = func(s string) string { return s }
	}
	return &Cache{
		ttl: ttl, maxBytes: maxBytes, fold: fold, now: time.Now,
		entries: map[key]*entry{}, byItem: map[string]map[key]bool{},
		listed: map[string]time.Time{}, lru: list.New(), inflight: map[key]*call{},
	}
}

// Enabled reports whether the cache keeps anything.
func (c *Cache) Enabled() bool { return c != nil && c.ttl > 0 }

func (c *Cache) k(dir, name string) key { return key{dir, c.fold(name)} }

// Get is what name in directory dir leads to, if the cache knows: from an
// entry, or, for a name not in a whole listing of dir, nothing.
func (c *Cache) Get(dir, name string) (Value, bool) {
	if !c.Enabled() {
		return Value{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(c.k(dir, name))
}

func (c *Cache) getLocked(k key) (Value, bool) {
	now := c.now()
	c.sweepLocked(now)
	if e, ok := c.entries[k]; ok {
		if now.Before(e.expires) {
			c.lru.MoveToBack(e.elem)
			return e.v, true
		}
		c.removeLocked(e)
	}
	if until, ok := c.listed[k.dir]; ok {
		if now.Before(until) {
			return Value{}, true // not in a whole listing: no such name
		}
		delete(c.listed, k.dir)
	}
	return Value{}, false
}

// Lookup is Get, falling back on load (a lookup in the store) and
// remembering its answer; concurrent lookups of one name share one load.
// A load that finds nothing answers Value{} with a nil error: the caller's
// load decides what "not found" is (it should return Value{}, nil for it).
func (c *Cache) Lookup(dir, name string, load func() (Value, error)) (Value, error) {
	if !c.Enabled() {
		return load()
	}
	k := c.k(dir, name)
	c.mu.Lock()
	if v, ok := c.getLocked(k); ok {
		c.mu.Unlock()
		return v, nil
	}
	if cl, ok := c.inflight[k]; ok {
		c.mu.Unlock()
		<-cl.done
		return cl.v, cl.err
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[k] = cl
	gen := c.gen
	c.mu.Unlock()

	cl.v, cl.err = load()

	c.mu.Lock()
	delete(c.inflight, k)
	if cl.err == nil && c.gen == gen {
		c.putLocked(k, cl.v)
	}
	c.mu.Unlock()
	close(cl.done)
	return cl.v, cl.err
}

// Put records what name in dir leads to now: after this machine creates,
// renames or changes it, or a lookup in the store.
func (c *Cache) Put(dir, name string, v Value) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.putLocked(c.k(dir, name), v)
}

// Refresh records what a lookup in the store, begun at gen (Gen), found
// for name in dir: an open's fresh lookup. It's a read, not a change, so
// it moves no mark, and it's dropped if this machine changed anything
// since gen.
func (c *Cache) Refresh(dir, name string, v Value, gen uint64) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen {
		c.putLocked(c.k(dir, name), v)
	}
}

// Gen is a mark to pass to PutListing: taken before reading a listing
// from the store, it keeps a listing read before one of this machine's
// changes from being stored.
func (c *Cache) Gen() uint64 {
	if !c.Enabled() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// PutListing records a whole listing of dir, read from the store after
// gen (Gen): each entry, and that a name not among them doesn't exist,
// until they expire. Of names that fold together, the first in byte order
// is the one a lookup finds. A listing read before a change this machine
// made since gen is dropped.
func (c *Cache) PutListing(dir string, values []Value, gen uint64) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	seen := map[key]string{}
	for _, v := range values {
		k := c.k(dir, v.Entry.Name)
		if prev, ok := seen[k]; ok && prev <= v.Entry.Name {
			continue
		}
		seen[k] = v.Entry.Name
		v.Found = true
		c.putLocked(k, v)
	}
	c.listed[dir] = c.now().Add(c.ttl)
}

// Forget drops what the cache knows of name in dir, and any whole listing
// of dir: the next lookup asks the store.
func (c *Cache) Forget(dir, name string) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	if e, ok := c.entries[c.k(dir, name)]; ok {
		c.removeLocked(e)
	}
	delete(c.listed, dir)
}

// ForgetItem drops every entry naming the item id (all its hard links),
// after its attributes changed here: the next lookup reads them again.
func (c *Cache) ForgetItem(id string) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for k := range c.byItem[id] {
		if e, ok := c.entries[k]; ok {
			c.removeLocked(e)
		}
	}
}

// ForgetDir drops a directory's whole listing, after a name in it changed
// in a way the caller didn't Put.
func (c *Cache) ForgetDir(dir string) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	delete(c.listed, dir)
}

// Bytes is the estimated size of what's cached.
func (c *Cache) Bytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *Cache) putLocked(k key, v Value) {
	if old, ok := c.entries[k]; ok {
		c.removeLocked(old)
	}
	size := sizeOf(k, v)
	if size > c.maxBytes/16 {
		return // one entry mustn't crowd out the rest: a huge xattr, say
	}
	now := c.now()
	e := &entry{k: k, v: v, expires: now.Add(c.ttl), size: size}
	e.elem = c.lru.PushBack(e)
	c.entries[k] = e
	if v.Found {
		if c.byItem[v.Entry.UUID] == nil {
			c.byItem[v.Entry.UUID] = map[key]bool{}
		}
		c.byItem[v.Entry.UUID][k] = true
	}
	c.bytes += size
	c.sweepLocked(now)
}

// sweepLocked drops entries from the least recently used end while they
// have expired, or while the cache is over its size: on every lookup and
// every Put, so a quiet cache doesn't hold on to a burst.
func (c *Cache) sweepLocked(now time.Time) {
	for front := c.lru.Front(); front != nil; front = c.lru.Front() {
		fe := front.Value.(*entry)
		if c.bytes <= c.maxBytes && now.Before(fe.expires) {
			break
		}
		c.removeLocked(fe)
	}
}

func (c *Cache) removeLocked(e *entry) {
	c.lru.Remove(e.elem)
	delete(c.entries, e.k)
	if e.v.Found {
		if keys := c.byItem[e.v.Entry.UUID]; keys != nil {
			delete(keys, e.k)
			if len(keys) == 0 {
				delete(c.byItem, e.v.Entry.UUID)
			}
		}
	}
	c.bytes -= e.size
}

// sizeOf estimates an entry's memory: the strings and byte slices it
// holds, and a fixed overhead for the rest.
func sizeOf(k key, v Value) int64 {
	n := int64(256 + len(k.dir) + len(k.name) + len(v.Entry.Name) + len(v.Entry.UUID))
	a := v.Attr
	n += int64(len(a.WindowsSDDL) + len(a.Reparse) + len(a.PosixACLAccess) + len(a.PosixACLDefault))
	for name, val := range a.Xattrs {
		n += int64(len(name) + len(val) + 48)
	}
	for name, val := range a.App {
		n += int64(len(name) + len(val) + 48)
	}
	for name := range a.Streams {
		n += int64(len(name) + 56)
	}
	return n
}
