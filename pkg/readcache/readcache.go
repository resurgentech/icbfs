// Package readcache is a read cache of file contents on local disk, shared
// by the programs and modules built on icbfs: icbfs mount, the drive API
// (icbfsdrive's Config.ReadCache), and icbfs-gitd.
//
// An entry is the bytes of one store version of one file: it is named by
// the file's key and the store's version id, which never name other
// bytes. So an entry is never stale; which version a reader wants is
// decided by the reader's own stat of the file (a live one, or one as of a
// time), never by the cache. A version that is no longer current is simply
// not asked for again, and goes when the least recently used entries are
// evicted.
//
// Layout of a cache directory (made 0700; every file 0600):
//
//	index.lock        locked (flock) while an entry is published or evicted
//	data/<sha256>     the entries, hex of sha256(key "\x00" version)
//	proc/<id>/        one a process using the directory: its fetches in
//	                  progress (tmp-*) and private copies (scratch-*), with
//	                  owner.lock held for as long as the process uses it
//
// Several processes of one user may use one directory at once: an entry is
// published by a rename (the same bytes whoever wins), and eviction runs
// under index.lock, after a process that sees data/ changed by another
// reads data/ again. What a process killed mid-fetch left (its proc/<id>)
// is removed by the next Open of the directory, which also reads data/ into
// the index, so the cache survives a restart within its bound.
//
// The cache holds file contents only, never a key or a credential. Open
// refuses a directory that is not the process's user's or that others can
// write (on Unix), so no other user can read an entry or plant one.
package readcache

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultMaxBytes is the default bound of a cache, 512 MiB.
const DefaultMaxBytes = 512 << 20

// entryUnit is the size an entry is counted as at least, for the bound on
// the number of entries: max(minMaxEntries, maxBytes/entryUnit). A cache
// of empty files is bounded too.
const (
	entryUnit     = 4096
	minMaxEntries = 1024
)

// rescanEvery is how often an insert reads data/ again even when its
// modified time did not move (a filesystem with coarse times).
const rescanEvery = time.Minute

// Fetch streams the bytes of exactly the version the caller named: never
// the current version of the key, which may have moved since the caller's
// stat. The cache calls it at most once a miss.
type Fetch func(ctx context.Context) (io.ReadCloser, error)

var (
	// ErrClosed is the answer of a closed cache.
	ErrClosed = errors.New("readcache: closed")
	// ErrShort is a fetch whose byte count is not the size the caller gave.
	ErrShort = errors.New("readcache: the fetched size is not the version's")
	// ErrUnsafeDir is Open's refusal of a directory another user owns or
	// can write.
	ErrUnsafeDir = errors.New("readcache: the cache directory must be the user's own and writable by no one else")
)

// Stats are a cache's counts since Open, for tests and logs.
type Stats struct {
	Hits, Misses int64 // Gets answered from an entry, and not
	Fetches      int64 // calls of a Fetch
	Waits        int64 // Gets that waited for another's fetch of the same entry
	Evictions    int64
	Entries      int
	Bytes        int64
}

// Cache is one cache directory, as this process uses it.
type Cache struct {
	dir        string
	maxBytes   int64
	maxEntries int

	procDir  string
	procLock *os.File

	mu       sync.Mutex
	closed   bool
	entries  map[string]*list.Element // name -> element of lru, value *entry
	lru      *list.List               // front: least recently used
	total    int64
	flights  map[string]*flight
	dataTime time.Time // data/'s modified time after this process's last scan or change
	scanned  time.Time
	stats    Stats
}

type entry struct {
	name string
	size int64
}

type flight struct {
	done chan struct{}
	err  error
}

// Options are Open's.
type Options struct {
	// MaxBytes bounds the entries' bytes (DefaultMaxBytes if 0 or less).
	MaxBytes int64
	// OwnDir refuses a directory that is not this process's user's or that
	// group or other can write (CheckDir): a program whose users must not
	// share a cache (icbfs-gitd, an agent a Linux user) sets it.
	OwnDir bool
}

// Open opens (making it, 0700) the cache directory dir: it removes what
// processes that died left, reads the entries there into its index and
// evicts down to the bound. Close lets go of this process's part of it;
// the entries stay.
func Open(dir string, o Options) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("readcache: no directory")
	}
	maxBytes := o.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if o.OwnDir {
		if err := CheckDir(dir); err != nil {
			return nil, err
		}
	}
	c := &Cache{dir: dir, maxBytes: maxBytes, maxEntries: int(max(int64(minMaxEntries), maxBytes/entryUnit)),
		entries: map[string]*list.Element{}, lru: list.New(), flights: map[string]*flight{}}
	for _, d := range []string{c.dataDir(), filepath.Join(dir, "proc")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := c.makeProcDir(); err != nil {
		return nil, err
	}
	err := c.withDirLock(func() error {
		sweepDeadProcDirs(filepath.Join(dir, "proc"), c.procDir)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.scanLocked()
		c.evictLocked("")
		c.noteDataTimeLocked()
		return nil
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Dir is the cache's directory.
func (c *Cache) Dir() string { return c.dir }

// MaxBytes is the cache's bound in bytes.
func (c *Cache) MaxBytes() int64 { return c.maxBytes }

// MaxEntries is the cache's bound in entries.
func (c *Cache) MaxEntries() int { return c.maxEntries }

// Stats are the cache's counts now.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries, s.Bytes = len(c.entries), c.total
	return s
}

// Close lets go of this process's part of the directory (its proc/<id>,
// with any private copies still there); the entries stay for the next
// Open. Later calls fail with ErrClosed. Close again does nothing.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.entries, c.lru, c.total = map[string]*list.Element{}, list.New(), 0
	lf := c.procLock
	c.procLock = nil
	c.mu.Unlock()
	if lf != nil {
		lf.Close()
	}
	if c.procDir != "" {
		return os.RemoveAll(c.procDir)
	}
	return nil
}

func (c *Cache) dataDir() string             { return filepath.Join(c.dir, "data") }
func (c *Cache) dataPath(name string) string { return filepath.Join(c.dataDir(), name) }

// Name is the name of (key, version)'s entry in data/.
func Name(key, version string) string {
	h := sha256.Sum256([]byte(key + "\x00" + version))
	return hex.EncodeToString(h[:])
}

// Has reports whether (key, version) has an entry this process knows of.
func (c *Cache) Has(key, version string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[Name(key, version)]
	return ok
}

// Get returns a read-only file holding the bytes of (key, version), and
// its size: the entry's, or, on a miss, fetched once (concurrent Gets of
// the same entry in this process wait for the one fetch) and published.
// size is the version's size if the caller knows it, else -1; a fetch of
// another length is ErrShort and publishes nothing. The caller closes the
// file. An entry evicted while its file is open stays readable through it.
// Content larger than the whole bound is not cached: the file is then a
// private copy, already unlinked where the system allows.
func (c *Cache) Get(ctx context.Context, key, version string, size int64, fetch Fetch) (*os.File, int64, error) {
	f, n, _, err := c.get(ctx, key, version, size, fetch)
	return f, n, err
}

// Seed returns a private, writable copy of (key, version)'s bytes, in
// this process's part of the directory, through Get: the caller writes it
// as it likes and removes it (os.Remove of its name) when done.
func (c *Cache) Seed(ctx context.Context, key, version string, size int64, fetch Fetch) (*os.File, int64, error) {
	f, n, private, err := c.get(ctx, key, version, size, fetch)
	if err != nil {
		return nil, 0, err
	}
	if private {
		return f, n, nil
	}
	defer f.Close()
	s, err := c.NewScratch()
	if err != nil {
		return nil, 0, err
	}
	if _, err := io.Copy(s, f); err != nil {
		s.Close()
		_ = os.Remove(s.Name())
		return nil, 0, err
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		s.Close()
		_ = os.Remove(s.Name())
		return nil, 0, err
	}
	return s, n, nil
}

// NewScratch makes a private, writable, empty file in this process's part
// of the directory; the caller removes it. What a process killed leaves is
// removed by the next Open.
func (c *Cache) NewScratch() (*os.File, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.procDir, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(c.procDir, "scratch-*")
}

// Put makes size bytes of r (from offset 0) the entry of (key, version):
// what a write just stored, so the next Get of that version fetches
// nothing. Content larger than the bound is not cached. A failure costs
// only a later fetch.
func (c *Cache) Put(key, version string, r io.ReaderAt, size int64) error {
	if version == "" {
		return errors.New("readcache: no version")
	}
	if err := c.live(); err != nil {
		return err
	}
	name := Name(key, version)
	if size > c.maxBytes {
		return nil
	}
	c.mu.Lock()
	if el, ok := c.entries[name]; ok {
		c.lru.MoveToBack(el)
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	tmp, err := c.newTmp()
	if err != nil {
		return err
	}
	n, err := io.Copy(tmp, io.NewSectionReader(r, 0, size))
	if err == nil && n != size {
		err = ErrShort
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return c.publish(tmp.Name(), name, n, nil)
}

func (c *Cache) live() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	return nil
}

// get is Get; private reports a file that is no entry (content larger
// than the bound, or one whose publish failed): the caller's alone.
func (c *Cache) get(ctx context.Context, key, version string, size int64, fetch Fetch) (*os.File, int64, bool, error) {
	if version == "" {
		return nil, 0, false, errors.New("readcache: no version")
	}
	name := Name(key, version)
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, 0, false, ErrClosed
		}
		if el, ok := c.entries[name]; ok {
			c.lru.MoveToBack(el)
			c.mu.Unlock()
			if f, n, ok := c.openEntry(name); ok {
				c.mu.Lock()
				c.stats.Hits++
				c.mu.Unlock()
				return f, n, false, nil
			}
			c.dropRow(name) // gone under us (another process evicted it, or the directory went)
			continue
		}
		if fl, ok := c.flights[name]; ok {
			c.stats.Waits++
			c.mu.Unlock()
			select {
			case <-fl.done:
			case <-ctx.Done():
				return nil, 0, false, ctx.Err()
			}
			if fl.err == nil {
				continue // published: a hit now
			}
			if ctx.Err() == nil && (errors.Is(fl.err, context.Canceled) || errors.Is(fl.err, context.DeadlineExceeded)) {
				continue // the leader's call was cut, not this one: fetch
			}
			return nil, 0, false, fl.err
		}
		fl := &flight{done: make(chan struct{})}
		c.flights[name] = fl
		c.stats.Misses++
		c.mu.Unlock()

		f, n, private, err := c.fill(ctx, name, size, fetch)

		c.mu.Lock()
		delete(c.flights, name)
		fl.err = err
		close(fl.done)
		c.mu.Unlock()
		return f, n, private, err
	}
}

// openEntry opens name's entry and marks it used (its modified time is
// the last use, for the index read at the next Open).
func (c *Cache) openEntry(name string) (*os.File, int64, bool) {
	f, err := os.Open(c.dataPath(name))
	if err != nil {
		return nil, 0, false
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, false
	}
	now := time.Now()
	_ = os.Chtimes(c.dataPath(name), now, now)
	return f, st.Size(), true
}

// fill is a miss: another process may have published the entry (it is
// taken into the index), else it is fetched and published.
func (c *Cache) fill(ctx context.Context, name string, size int64, fetch Fetch) (*os.File, int64, bool, error) {
	if st, err := os.Stat(c.dataPath(name)); err == nil && st.Mode().IsRegular() && (size < 0 || st.Size() == size) {
		c.addRow(name, st.Size())
		if f, n, ok := c.openEntry(name); ok {
			return f, n, false, nil
		}
	}
	tmp, err := c.newTmp()
	if err != nil {
		return nil, 0, false, err
	}
	fail := func(err error) (*os.File, int64, bool, error) {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, 0, false, err
	}
	c.mu.Lock()
	c.stats.Fetches++
	c.mu.Unlock()
	body, err := fetch(ctx)
	if err != nil {
		return fail(err)
	}
	n, err := io.Copy(tmp, body)
	body.Close()
	if err != nil {
		return fail(err)
	}
	if size >= 0 && n != size {
		return fail(fmt.Errorf("%w: %d bytes, want %d", ErrShort, n, size))
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if n > c.maxBytes {
		// Larger than the whole bound: the caller's alone.
		_ = os.Remove(tmp.Name()) // Unix: the open file is all that holds it
		return tmp, n, true, nil
	}
	var opened *os.File
	err = c.publish(tmp.Name(), name, n, func() error {
		f, err := os.Open(c.dataPath(name))
		opened = f
		return err
	})
	if err != nil || opened == nil {
		// Not published: the fetched copy is still good, as the caller's.
		_ = os.Remove(tmp.Name())
		return tmp, n, true, nil
	}
	tmp.Close()
	return opened, n, false, nil
}

func (c *Cache) newTmp() (*os.File, error) {
	if err := os.MkdirAll(c.procDir, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(c.procDir, "tmp-*")
}

// publish renames tmp onto name's entry, under the directory's lock, and
// evicts down to the bound, sparing name; then, still under the lock (so
// no other process evicts it first), calls after.
func (c *Cache) publish(tmp, name string, size int64, after func() error) error {
	return c.withDirLock(func() error {
		if err := os.MkdirAll(c.dataDir(), 0o700); err != nil {
			return err
		}
		c.mu.Lock()
		if c.otherChangedDataLocked() {
			c.scanLocked()
		}
		c.mu.Unlock()
		if err := os.Rename(tmp, c.dataPath(name)); err != nil {
			return err
		}
		c.mu.Lock()
		c.addRowLocked(name, size)
		c.evictLocked(name)
		c.noteDataTimeLocked()
		c.mu.Unlock()
		if after != nil {
			return after()
		}
		return nil
	})
}

func (c *Cache) addRow(name string, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addRowLocked(name, size)
}

func (c *Cache) addRowLocked(name string, size int64) {
	if el, ok := c.entries[name]; ok {
		e := el.Value.(*entry)
		c.total += size - e.size
		e.size = size
		c.lru.MoveToBack(el)
		return
	}
	c.entries[name] = c.lru.PushBack(&entry{name: name, size: size})
	c.total += size
}

func (c *Cache) dropRow(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[name]; ok {
		c.total -= el.Value.(*entry).size
		c.lru.Remove(el)
		delete(c.entries, name)
	}
}

// evictLocked removes the least recently used entries, and their files,
// until the cache is within both bounds, sparing keep. The caller holds
// the directory's lock (or is Open).
func (c *Cache) evictLocked(keep string) {
	for c.total > c.maxBytes || len(c.entries) > c.maxEntries {
		el := c.lru.Front()
		if el != nil && el.Value.(*entry).name == keep {
			el = el.Next()
		}
		if el == nil {
			return
		}
		e := el.Value.(*entry)
		_ = os.Remove(c.dataPath(e.name)) // an open file of it still reads (Unix)
		c.total -= e.size
		c.lru.Remove(el)
		delete(c.entries, e.name)
		c.stats.Evictions++
	}
}

// otherChangedDataLocked reports whether data/ may hold what another
// process did since this one last looked (its modified time moved, or a
// while passed).
func (c *Cache) otherChangedDataLocked() bool {
	st, err := os.Stat(c.dataDir())
	if err != nil {
		return true
	}
	return !st.ModTime().Equal(c.dataTime) || time.Since(c.scanned) > rescanEvery
}

func (c *Cache) noteDataTimeLocked() {
	if st, err := os.Stat(c.dataDir()); err == nil {
		c.dataTime = st.ModTime()
	}
}

// scanLocked reads data/ into the index: each entry's size, and its
// modified time as its last use (the order of the LRU). Names that are
// not entries are left alone. It reads a page of names at a time.
func (c *Cache) scanLocked() {
	type found struct {
		name string
		size int64
		used time.Time
	}
	var all []found
	d, err := os.Open(c.dataDir())
	if err == nil {
		for {
			ents, rerr := d.ReadDir(1024)
			for _, e := range ents {
				if !isEntryName(e.Name()) || !e.Type().IsRegular() {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				all = append(all, found{e.Name(), info.Size(), info.ModTime()})
			}
			if rerr != nil || len(ents) == 0 {
				break
			}
		}
		d.Close()
	}
	// Oldest first; what this process used since its last scan keeps its
	// place by its file's time (openEntry sets it).
	sort.Slice(all, func(i, j int) bool { return all[i].used.Before(all[j].used) })
	c.entries, c.lru, c.total = make(map[string]*list.Element, len(all)), list.New(), 0
	for _, f := range all {
		c.entries[f.name] = c.lru.PushBack(&entry{name: f.name, size: f.size})
		c.total += f.size
	}
	c.scanned = time.Now()
	c.noteDataTimeLocked()
}

func isEntryName(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// withDirLock runs fn holding the directory's index.lock (between
// processes) and nothing else.
func (c *Cache) withDirLock(fn func() error) error {
	lf, err := os.OpenFile(filepath.Join(c.dir, "index.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := lockFile(lf); err != nil {
		return err
	}
	return fn()
}

// makeProcDir makes this process's proc/<id> and takes its owner.lock,
// written with one byte once locked, as icbfs's own cache directories are
// (a sweep removes only a directory whose lock it takes and whose file
// has its byte).
func (c *Cache) makeProcDir() error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	dir := filepath.Join(c.dir, "proc", hex.EncodeToString(b[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(dir, ownerLockName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	if tryLockFile(lf) {
		if _, err := lf.Write([]byte{1}); err != nil {
			lf.Close()
			_ = os.RemoveAll(dir)
			return err
		}
	}
	c.procDir, c.procLock = dir, lf
	return nil
}

const ownerLockName = "owner.lock"

// sweepDeadProcDirs removes each directory in parent but own whose owner
// let go of its lock: a process that died (killed, or never closed).
func sweepDeadProcDirs(parent, own string) int {
	d, err := os.Open(parent)
	if err != nil {
		return 0
	}
	defer d.Close()
	removed := 0
	for {
		ents, err := d.ReadDir(1024)
		for _, e := range ents {
			p := filepath.Join(parent, e.Name())
			if !e.IsDir() || p == own {
				continue
			}
			if RemoveIfDead(p) {
				removed++
			}
		}
		if err != nil || len(ents) == 0 {
			return removed
		}
	}
}

// RemoveIfDead removes dir if it holds an owner.lock with its byte whose
// lock can be taken: its maker let go (exited or was killed). A directory
// with no lock file, an empty one (being made) or one held is left.
func RemoveIfDead(dir string) bool {
	lf, err := os.OpenFile(filepath.Join(dir, ownerLockName), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	if !tryLockFile(lf) {
		lf.Close()
		return false
	}
	st, err := lf.Stat()
	if err != nil || st.Size() == 0 {
		lf.Close()
		return false
	}
	lf.Close()
	return os.RemoveAll(dir) == nil
}
