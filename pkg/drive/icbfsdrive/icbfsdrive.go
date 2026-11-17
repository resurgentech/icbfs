// Package icbfsdrive is the drive library's icbfs backend: a Drive on an
// icbfs filesystem, or on a directory in one.
package icbfsdrive

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"maps"
	"mime"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// Config says which drive: Filesystem in Store, from its directory Root
// ("/" or "" for the whole filesystem).
type Config struct {
	Store      objstore.Store
	Filesystem string
	Root       string
	// ID is the drive's id in the host's registry, recorded in the
	// origin of copies made from it.
	ID string
	// History is the history cache as-of views read through. If it's
	// nil, the drive opens a temporary one in CacheDir, which Close
	// removes.
	History *objstore.HistoryCache
	// CacheDir holds the drive's temporary files ("" for the temp
	// directory).
	CacheDir string
	// ReadOnly makes every change through this drive fail with
	// drive.ErrReadOnly, whatever the actor's profile and the item's
	// permissions: a host serving a filesystem something else writes (a
	// mirror's) to users who may only read it. Reads are as usual.
	ReadOnly bool
	// TrustFor is how long a directory the drive has read is used
	// without asking the store again: right for a daemon that is its
	// filesystem's only writer, which then walks a path it walked a
	// moment ago with no requests. At 0, the default, each step of a walk
	// checks the directory it holds (two conditional reads, no bodies
	// when nothing changed). Changes made through this drive are seen at
	// once either way; changes made elsewhere, after at most TrustFor.
	TrustFor time.Duration
	// DirCacheBytes bounds the directories the drive keeps (TrustFor's
	// cache), by an estimate of their bytes (icbfs.Attr.HeldSize): past
	// it, the least recently used go. 0 is the default,
	// DefaultDirCacheBytes.
	DirCacheBytes int64
	// NoLocking turns locks off for this drive, as a mount's
	// -locking=false does: Lock is refused, and Write, Move and Delete
	// look for no one's locks, which saves a read of the file's lock
	// object per write. With it off (the default) the drive is a
	// drive.Locker, and its writes refuse a file another holder has
	// locked, from a mount or through a drive.
	NoLocking bool
	// SearchBudget bounds a search of the tree for an item by id (an id
	// with no path that leads to it, and none remembered): it reads at
	// most this many folders in one call, then stops with
	// drive.ErrNotSearched, keeping where it got to so the next call for
	// that id goes on from there (stoppedSearches). 0, the default, is no
	// budget: a search reads up to every folder, as before T82-15. A host
	// that answers requests under a deadline sets one
	// (DefaultSearchBudget). A cut context stops a search the same way,
	// with or without a budget.
	SearchBudget int
}

// DefaultSearchBudget is the search budget for a drive that answers
// requests (T82-15): 128 folders, one store read each for a folder in
// one block, so about 14 s at the 110 ms a read measured against
// production's bucket, and a request that makes two such lookups (a
// Draft's folder now and as of its base) still ends inside Workspaces'
// 30-second client and far inside Drives' 60-second deadline. It covers
// a Draft's own filesystem (a few dozen folders) and the top of a
// library (every folder down to depth 4 of a ternary tree) in one call;
// a deeper search goes on over the calls that ask again.
const DefaultSearchBudget = 128

// Drive is an icbfs drive.
type Drive struct {
	cfg     Config
	fs      *icbfs.Filesystem
	root    string // fs-relative, "" for the filesystem's root, else "/a/b"
	rootKey string
	windows bool

	views  viewCache
	events broadcaster
	// idPaths is where items are now, by id: the present tree's memory.
	// A view of the past keeps its own (viewMemory), so a lookup as of a
	// time never overwrites where an item is now (T82-15).
	idPaths idPathCache
	// stopped are the searches by id that stopped before they ended.
	stopped stoppedSearches
	// searches counts the walks of the tree for an item by id (findPaths,
	// search).
	searches atomic.Int64
	dirs     *dirCache
	past     pastMarker
}

var _ drive.Drive = (*Drive)(nil)

// New attaches to the filesystem and finds the drive's root directory.
func New(ctx context.Context, cfg Config) (*Drive, error) {
	dirs := newDirCache(cfg.TrustFor, cfg.DirCacheBytes)
	fsys := icbfs.New(&dropOnWrite{Store: cfg.Store, dirs: dirs}, cfg.Filesystem)
	if err := fsys.Attach(ctx); err != nil {
		return nil, mapErr(err)
	}
	fsys.EnableLocking(!cfg.NoLocking)
	d := &Drive{cfg: cfg, fs: fsys, windows: fsys.PrimaryWindows(), dirs: dirs}
	d.root = cleanPath(cfg.Root)
	d.views.d = d
	d.past.mark = fsys.MarkPastChanged
	fsys.SetPastChanged(d.past.changed)
	key := fsys.RootKey()
	for _, part := range splitPath(d.root) {
		e, _, err := d.lookup(ctx, fsys, key, part)
		if err != nil {
			return nil, fmt.Errorf("drive root %s: %w", cfg.Root, mapErr(err))
		}
		if e.Type != icbfs.TypeDir {
			return nil, fmt.Errorf("drive root %s: %w", cfg.Root, drive.ErrNotDir)
		}
		key = e.UUID
	}
	d.rootKey = key
	return d, nil
}

// Filesystem is the drive's icbfs filesystem.
func (d *Drive) Filesystem() *icbfs.Filesystem { return d.fs }

// Close sends a past-changed mark still put off (pastMarker), and
// releases the drive's as-of views and its temporary history cache.
func (d *Drive) Close() {
	d.past.flush()
	d.views.closeAll()
}

// Capabilities: everything.
func (d *Drive) Capabilities() drive.Capabilities {
	perms := "posix"
	if d.windows {
		perms = "windows"
	}
	return drive.Capabilities{IDs: true, Versions: true, AsOf: true, Names: true, Trash: true, Purge: true,
		ConditionalWrite: true, Events: true, Origin: true, Permissions: perms, Diff: "computed", AppObjects: true,
		ConditionalAppObjects: true, Locks: !d.cfg.NoLocking}
}

// --- paths ---

// cleanPath normalizes a path to "" (the top) or "/a/b". "/" is the only
// separator: a backslash is a character of a name (T82-14). Turning it
// into "/" here, after a caller (icbfs-git, a mirror) had checked the
// path with "/" alone, let "..\" climb out of a Draft's folder and a
// Clio name land in another matter. "." and ".." are still tidied, as
// before, within the drive's root: Drafts and mirrors refuse them
// themselves (drive.SplitPath) before they call the drive.
func cleanPath(p string) string {
	p = path.Clean("/" + p)
	if p == "/" {
		return ""
	}
	return p
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// fsPath is a drive-relative path as a filesystem path.
func (d *Drive) fsPath(p string) string {
	return d.root + cleanPath(p)
}

// drivePath is a filesystem path as drive-relative ("/" for the drive's
// root), and whether it's in the drive at all.
func (d *Drive) drivePath(fsPath string) (string, bool) {
	fsPath = cleanPath(fsPath)
	if d.root == "" {
		if fsPath == "" {
			return "/", true
		}
		return fsPath, true
	}
	if fsPath == d.root {
		return "/", true
	}
	if strings.HasPrefix(fsPath, d.root+"/") {
		return fsPath[len(d.root):], true
	}
	return "", false
}

func (d *Drive) lookup(ctx context.Context, fsys *icbfs.Filesystem, dirKey, name string) (block.Entry, icbfs.Attr, error) {
	if d.windows {
		return fsys.LookupFold(ctx, dirKey, name)
	}
	return fsys.Lookup(ctx, dirKey, name)
}

// step is lookup for a walk down a path from dir, whose block its stat
// read (icbfs.Filesystem.FindStep); a directory found comes from the
// directory cache (dirAttr).
func (d *Drive) step(ctx context.Context, dir *node, name string) (block.Entry, icbfs.Attr, error) {
	e, err := dir.fsys.FindStep(ctx, dir.key, dir.attr, name, d.windows)
	if err != nil {
		return block.Entry{}, icbfs.Attr{}, err
	}
	var a icbfs.Attr
	if e.Type == icbfs.TypeDir {
		a, err = d.dirAttr(ctx, dir.fsys, e.UUID)
	} else {
		a, err = dir.fsys.Stat(ctx, e.UUID, e.Type)
	}
	return e, a, err
}

// lookupIn is lookup in dir, reading its block afresh.
func (d *Drive) lookupIn(ctx context.Context, dir *node, name string) (block.Entry, icbfs.Attr, error) {
	return d.lookup(ctx, dir.fsys, dir.key, name)
}

// node is a resolved item, with the directories from the filesystem's
// root down to it — what its inherited ACL and traversal checks need.
type node struct {
	fsys  *icbfs.Filesystem
	key   string
	typ   icbfs.EntryType
	name  string // as stored
	path  string // drive-relative, "/" for the root
	attr  icbfs.Attr
	chain []ancestor // the filesystem's root first, down to its parent
}

type ancestor struct {
	key  string
	path string // drive-relative; "" above the drive's root
	attr icbfs.Attr
}

func (n *node) parentKey() string { return n.chain[len(n.chain)-1].key }

// child is the node of entry e in directory n, with attributes a.
func (n *node) child(e block.Entry, a icbfs.Attr) *node {
	return &node{fsys: n.fsys, key: e.UUID, typ: e.Type, name: e.Name, path: joinPath(n.path, e.Name), attr: a,
		chain: append(append([]ancestor(nil), n.chain...), ancestor{key: n.key, path: n.path, attr: n.attr})}
}

// rootNode is the drive's root, with the directories above it.
func (d *Drive) rootNode(ctx context.Context, fsys *icbfs.Filesystem) (*node, error) {
	key := fsys.RootKey()
	attr, err := d.dirAttr(ctx, fsys, key)
	if err != nil {
		return nil, mapErr(err)
	}
	n := &node{fsys: fsys, key: key, typ: icbfs.TypeDir, name: "", path: "/", attr: attr}
	for _, part := range splitPath(d.root) {
		e, a, err := d.step(ctx, n, part)
		if err != nil {
			return nil, mapErr(err)
		}
		n.chain = append(n.chain, ancestor{key: n.key, attr: n.attr})
		n.key, n.typ, n.name, n.attr = e.UUID, e.Type, e.Name, a
	}
	if n.key != d.rootKey && fsys == d.fs {
		return nil, fmt.Errorf("drive root moved: %w", drive.ErrNotFound)
	}
	return n, nil
}

// resolvePath walks p from the drive's root.
func (d *Drive) resolvePath(ctx context.Context, fsys *icbfs.Filesystem, p string) (*node, error) {
	n, err := d.rootNode(ctx, fsys)
	if err != nil {
		return nil, err
	}
	for _, part := range splitPath(cleanPath(p)) {
		if n.typ != icbfs.TypeDir {
			return nil, drive.ErrNotDir
		}
		if err := d.checkTraverse(ctx, n); err != nil {
			return nil, err
		}
		e, a, err := d.step(ctx, n, part)
		if err != nil {
			return nil, mapErr(err)
		}
		n = n.child(e, a)
	}
	return n, nil
}

func joinPath(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// resolve finds an item by ref; by id it searches the drive's tree (the
// filesystem keeps no parent pointers), remembering where it found it.
func (d *Drive) resolve(ctx context.Context, fsys *icbfs.Filesystem, ref drive.ItemRef) (*node, error) {
	if ref.ID == "" {
		return d.resolvePath(ctx, fsys, ref.Path)
	}
	if ref.ID == d.rootKey {
		return d.rootNode(ctx, fsys)
	}
	// The id decides, but a path given with it says where to look: one
	// walk down, checked against the id. Without it a new item, which no
	// search has found yet, costs a search of the whole tree (one read
	// per directory) to learn a path the caller had in hand.
	if ref.Path != "" {
		if n, err := d.resolvePath(ctx, fsys, ref.Path); err == nil && n.key == ref.ID {
			return n, nil
		}
	}
	// What this view remembers: the present's memory for the present,
	// the view's own for a view of the past (T82-15).
	mem := d.memory(fsys)
	if p, ok := mem.get(ref.ID); ok {
		if n, err := d.resolvePath(ctx, fsys, p); err == nil && n.key == ref.ID {
			return n, nil
		}
	}
	p, err := d.search(ctx, fsys, mem, ref.ID)
	if err != nil {
		return nil, err
	}
	if p == "" {
		return nil, fmt.Errorf("id %s: %w", ref.ID, drive.ErrNotFound)
	}
	n, err := d.resolvePath(ctx, fsys, p)
	if err != nil {
		return nil, err
	}
	if n.key != ref.ID {
		// Moved while it was searched for (a search that went on from an
		// earlier call read that call's paths): asked again, it searches
		// afresh.
		return nil, fmt.Errorf("lookup by id %s: found at %s, which no longer leads to it: %w", ref.ID, p, drive.ErrNotSearched)
	}
	mem.put(ref.ID, p)
	return n, nil
}

// memory is the id → path memory of fsys: the present's (d.idPaths) for
// the drive's filesystem, else the view's own (viewCache.memoryOf). A
// view no longer kept gets one that is thrown away after the call.
func (d *Drive) memory(fsys *icbfs.Filesystem) *idPathCache {
	if fsys == d.fs {
		return &d.idPaths
	}
	if m := d.views.memoryOf(fsys); m != nil {
		return m
	}
	return &idPathCache{}
}

// Walks is how many walks of the drive's tree for an item by id it has
// made since it was opened (findPaths, search: an id it doesn't
// remember, with no path, record or hint that finds it; a read a folder,
// up to the whole tree or the search budget). A search that goes on from
// one that stopped is a walk again. For tests and diagnostics.
func (d *Drive) Walks() int64 { return d.searches.Load() }

// search is the path of item id in fsys's tree, "" if it isn't there:
// breadth first, one ReadDir a folder, going on from where a search for
// it in the same view stopped (stoppedSearches). It stops when ctx ends,
// or once it has read the drive's budget of folders in this call
// (Config.SearchBudget), with drive.ErrNotSearched, and keeps the folders
// it still had to read for the next call. A folder it can't read is
// skipped, as before; one cut by the context is read again next time.
func (d *Drive) search(ctx context.Context, fsys *icbfs.Filesystem, mem *idPathCache, id string) (string, error) {
	d.searches.Add(1)
	key := stoppedKey{mem: mem, id: id}
	s := d.stopped.take(key)
	if s == nil {
		s = &stoppedSearch{queue: []searchDir{{d.rootKey, "/"}}}
	}
	budget := d.cfg.SearchBudget
	read := 0
	stop := func(cause error) error {
		d.stopped.keep(key, s)
		if cause != nil {
			return fmt.Errorf("lookup by id %s: searched %d folders (%d in this call), %d still to read: %w (%w)",
				id, s.read, read, len(s.queue), drive.ErrNotSearched, cause)
		}
		return fmt.Errorf("lookup by id %s: searched %d folders (%d in this call), %d still to read: %w (its budget of %d folders a call)",
			id, s.read, read, len(s.queue), drive.ErrNotSearched, budget)
	}
	for len(s.queue) > 0 {
		if err := ctx.Err(); err != nil {
			return "", stop(err)
		}
		if budget > 0 && read >= budget {
			return "", stop(nil)
		}
		cur := s.queue[0]
		entries, err := fsys.ReadDir(ctx, cur.key)
		if err != nil && ctx.Err() != nil {
			return "", stop(ctx.Err())
		}
		s.queue = s.queue[1:]
		read++
		s.read++
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := joinPath(cur.path, e.Name)
			if e.UUID == id {
				return p, nil
			}
			if e.Type == icbfs.TypeDir {
				s.queue = append(s.queue, searchDir{e.UUID, p})
			}
		}
	}
	return "", nil
}

// findPaths is up to max (0: all) drive-relative paths naming id: a
// whole walk, no budget (Stat's every path to a file with more than one
// link; a lookup by id is search).
func (d *Drive) findPaths(ctx context.Context, fsys *icbfs.Filesystem, id string, max int) ([]string, error) {
	d.searches.Add(1)
	type dir struct{ key, path string }
	var out []string
	queue := []dir{{d.rootKey, "/"}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		entries, err := fsys.ReadDir(ctx, cur.key)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := joinPath(cur.path, e.Name)
			if e.UUID == id {
				out = append(out, p)
				if max > 0 && len(out) == max {
					return out, nil
				}
			}
			if e.Type == icbfs.TypeDir {
				queue = append(queue, dir{e.UUID, p})
			}
		}
	}
	return out, nil
}

// resolveParent resolves the directory p would be in, and p's base name;
// with parents, missing directories are made.
func (d *Drive) resolveParent(ctx context.Context, p string, parents bool) (*node, string, error) {
	p = cleanPath(p)
	if p == "" {
		return nil, "", fmt.Errorf("the drive's root: %w", drive.ErrInvalidName)
	}
	dir, base := path.Split(p)
	var parent *node
	var err error
	if parents {
		parent, err = d.mkdirAll(ctx, dir)
	} else {
		parent, err = d.resolvePath(ctx, d.fs, dir)
	}
	if err != nil {
		return nil, "", err
	}
	if parent.typ != icbfs.TypeDir {
		return nil, "", drive.ErrNotDir
	}
	if err := d.checkTraverse(ctx, parent); err != nil {
		return nil, "", err
	}
	return parent, base, nil
}

// --- items ---

func typeName(t icbfs.EntryType) string {
	switch t {
	case icbfs.TypeDir:
		return drive.TypeDir
	case icbfs.TypeSymlink:
		return drive.TypeSymlink
	case icbfs.TypeFile:
		return drive.TypeFile
	}
	return ""
}

func contentType(name string, attr icbfs.Attr) string {
	if ct, ok := attr.App[icbfs.AppDriveContentType]; ok {
		return string(ct)
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// item describes n; withVersion fetches a file's current version.
func (d *Drive) item(ctx context.Context, n *node, withVersion bool) drive.Item {
	it := drive.Item{ID: n.key, Name: n.name, Path: n.path, Type: typeName(n.typ), Size: n.attr.Size,
		Modified: n.attr.Mtime, Changed: n.attr.Ctime, Created: n.attr.Btime}
	if n.path == "/" {
		it.Name = ""
	}
	switch n.typ {
	case icbfs.TypeFile:
		it.ContentType = contentType(n.name, n.attr)
		if c, ok := n.attr.ContentChecksums(); ok {
			it.Checksums = toChecksums(c)
		}
		if withVersion {
			it.Version = d.currentVersion(ctx, n, nil)
		}
	case icbfs.TypeSymlink:
		if t, err := n.fsys.Readlink(ctx, n.key); err == nil {
			it.Target = t
		}
	}
	if o, ok := n.attr.App[icbfs.AppDriveOrigin]; ok {
		it.Origin = decodeOrigin(o)
	}
	it.App = maps.Clone(n.attr.App) // a directory's attributes are shared (dirCache)
	it.Mode = n.attr.Mode
	if bits, ok := n.attr.WindowsAttributes(); ok {
		it.WindowsAttrs = &bits
	}
	return it
}

// currentVersion is file n's current version: the one its stat read,
// dated by dates if given, else by its .version record if it has one.
func (d *Drive) currentVersion(ctx context.Context, n *node, dates objstore.Dates) *drive.Version {
	obj, ok := n.attr.ContentVersion() // its Size is already a stub's real one
	if !ok {
		head, err := n.fsys.Store().Head(ctx, n.key)
		if err != nil {
			return nil
		}
		obj = *head
		if obj.Size, err = icbfs.ContentSize(head); err != nil {
			return nil
		}
	}
	if dates == nil {
		dates = objstore.Dates{}
		if n.attr.Dated() {
			dates, _ = objstore.ReadDates(ctx, n.fsys.Store(), n.key)
		}
	}
	return &drive.Version{ID: obj.VersionID, Time: dates.Time(&obj), Stored: obj.LastModified, Size: obj.Size, Current: true,
		Checksums: toChecksums(n.attr.Checksums[obj.ETag])}
}

// visible: FIFOs, sockets and devices aren't items.
func visible(t icbfs.EntryType) bool {
	return t == icbfs.TypeFile || t == icbfs.TypeDir || t == icbfs.TypeSymlink
}

// --- errors ---

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, drive.ErrNotFound), errors.Is(err, drive.ErrForbidden), errors.Is(err, drive.ErrPrecondition),
		errors.Is(err, drive.ErrNoAccess), errors.Is(err, drive.ErrReadOnly),
		errors.Is(err, drive.ErrInvalid), errors.Is(err, drive.ErrChecksum),
		errors.Is(err, drive.ErrLocked), errors.Is(err, drive.ErrNotLockHolder), errors.Is(err, drive.ErrNotSupported):
		return err
	case errors.Is(err, icbfs.ErrNotFound), objstore.IsNotFound(err):
		return fmt.Errorf("%w (%v)", drive.ErrNotFound, err)
	case errors.Is(err, icbfs.ErrExists):
		return fmt.Errorf("%w (%v)", drive.ErrExists, err)
	case errors.Is(err, icbfs.ErrNotEmpty):
		return fmt.Errorf("%w (%v)", drive.ErrNotEmpty, err)
	case errors.Is(err, icbfs.ErrNotDir):
		return fmt.Errorf("%w (%v)", drive.ErrNotDir, err)
	case errors.Is(err, icbfs.ErrIsDir):
		return fmt.Errorf("%w (%v)", drive.ErrIsDir, err)
	case errors.Is(err, icbfs.ErrHasLinks):
		return fmt.Errorf("%w (%v)", drive.ErrHasLinks, err)
	case errors.Is(err, icbfs.ErrInvalidName), errors.Is(err, icbfs.ErrNameTooLong), errors.Is(err, icbfs.ErrInvalidRename), errors.Is(err, icbfs.ErrInvalidArgument):
		return fmt.Errorf("%w (%v)", drive.ErrInvalidName, err)
	case errors.Is(err, icbfs.ErrFileTooLarge):
		return fmt.Errorf("%w (%v)", drive.ErrTooLarge, err)
	case errors.Is(err, icbfs.ErrReadOnly), errors.Is(err, icbfs.ErrArchived), errors.Is(err, objstore.ErrReadOnly):
		return fmt.Errorf("%w (%v)", drive.ErrReadOnly, err)
	case errors.Is(err, icbfs.ErrLocked):
		return fmt.Errorf("%w (%v)", drive.ErrLocked, err)
	case errors.Is(err, icbfs.ErrNotLockHolder):
		return fmt.Errorf("%w (%v)", drive.ErrNotLockHolder, err)
	case errors.Is(err, icbfs.ErrLockingDisabled):
		return fmt.Errorf("%w (%v)", drive.ErrNotSupported, err)
	case objstore.IsPreconditionFailed(err):
		return fmt.Errorf("%w (%v)", drive.ErrBusy, err)
	}
	return err
}

// --- the id → path memory ---

// idPathEntries is how many item paths a drive remembers (T82-13): the
// items it wrote, moved or found by a walk most recently. An entry is an
// id (36 bytes), a path, a map slot and a list element: about 300 bytes
// at a 100-byte path, so a full memory is about 0.3 MiB a drive, under
// 0.5% of a daemon's 150 MiB soft limit with its two drives, and the
// same at any size of drive. The mirrors don't need it to find what they
// wrote: a lookup by origin is told where the item is
// (drive.Mirror.WithHints, FindByOriginHint), and so is a stub's target
// (drive.Ref.Path, the content map's place). What it still saves is a
// walk for an id asked again soon after a write, a move or a walk that
// found it, by a caller with no path (a stub's target whose place the
// map doesn't have yet, which the mirror then learns; Drives' reads by
// id).
const idPathEntries = 1024

// idPathCache remembers where items are, by id: at most max entries
// (idPathEntries if 0), the least recently used dropped first.
type idPathCache struct {
	mu  sync.Mutex
	max int
	m   map[string]*list.Element // of *idPath, in lru
	lru list.List                // most recently used first
}

type idPath struct{ id, path string }

func (c *idPathCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok {
		return "", false
	}
	c.lru.MoveToFront(e)
	return e.Value.(*idPath).path, true
}

func (c *idPathCache) put(id, p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(id, p)
}

func (c *idPathCache) putLocked(id, p string) {
	if e, ok := c.m[id]; ok {
		e.Value.(*idPath).path = p
		c.lru.MoveToFront(e)
		return
	}
	if c.m == nil {
		c.m = map[string]*list.Element{}
	}
	limit := c.max
	if limit <= 0 {
		limit = idPathEntries
	}
	for c.lru.Len() >= limit {
		old := c.lru.Back()
		delete(c.m, old.Value.(*idPath).id)
		c.lru.Remove(old)
	}
	c.m[id] = c.lru.PushFront(&idPath{id: id, path: p})
}

func (c *idPathCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// moved notes that id went from one path to another, and so did what it
// held: a directory's remembered descendants move with it (a pass over
// at most max entries).
func (c *idPathCache) moved(id, from, to string, dir bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(id, to)
	if !dir {
		return
	}
	for e := c.lru.Front(); e != nil; e = e.Next() {
		ip := e.Value.(*idPath)
		if rest, ok := strings.CutPrefix(ip.path, from+"/"); ok {
			ip.path = joinPath(to, rest)
		}
	}
}
