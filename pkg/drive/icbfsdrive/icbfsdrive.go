// Package icbfsdrive is the drive library's icbfs backend: a Drive on an
// icbfs filesystem, or on a directory in one.
package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"
	"sync"

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
}

// Drive is an icbfs drive.
type Drive struct {
	cfg     Config
	fs      *icbfs.Filesystem
	root    string // fs-relative, "" for the filesystem's root, else "/a/b"
	rootKey string
	windows bool

	views   viewCache
	events  broadcaster
	idPaths idPathCache
}

var _ drive.Drive = (*Drive)(nil)

// New attaches to the filesystem and finds the drive's root directory.
func New(ctx context.Context, cfg Config) (*Drive, error) {
	fsys := icbfs.New(cfg.Store, cfg.Filesystem)
	if err := fsys.Attach(ctx); err != nil {
		return nil, mapErr(err)
	}
	d := &Drive{cfg: cfg, fs: fsys, windows: fsys.PrimaryWindows()}
	d.root = cleanPath(cfg.Root)
	d.views.d = d
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

// Close releases the drive's as-of views, and its temporary history
// cache.
func (d *Drive) Close() { d.views.closeAll() }

// Capabilities: everything.
func (d *Drive) Capabilities() drive.Capabilities {
	perms := "posix"
	if d.windows {
		perms = "windows"
	}
	return drive.Capabilities{IDs: true, Versions: true, AsOf: true, Names: true, Trash: true, Purge: true,
		ConditionalWrite: true, Events: true, Origin: true, Permissions: perms, Diff: "computed", AppObjects: true}
}

// --- paths ---

// cleanPath normalizes a path to "" (the top) or "/a/b".
func cleanPath(p string) string {
	p = path.Clean("/" + strings.ReplaceAll(p, `\`, "/"))
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

// rootNode is the drive's root, with the directories above it.
func (d *Drive) rootNode(ctx context.Context, fsys *icbfs.Filesystem) (*node, error) {
	key := fsys.RootKey()
	attr, err := fsys.Stat(ctx, key, icbfs.TypeDir)
	if err != nil {
		return nil, mapErr(err)
	}
	n := &node{fsys: fsys, key: key, typ: icbfs.TypeDir, name: "", path: "/", attr: attr}
	for _, part := range splitPath(d.root) {
		e, a, err := d.lookup(ctx, fsys, n.key, part)
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
		e, a, err := d.lookup(ctx, fsys, n.key, part)
		if err != nil {
			return nil, mapErr(err)
		}
		n = &node{fsys: fsys, key: e.UUID, typ: e.Type, name: e.Name, path: joinPath(n.path, e.Name), attr: a,
			chain: append(append([]ancestor(nil), n.chain...), ancestor{key: n.key, path: n.path, attr: n.attr})}
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
	if p, ok := d.idPaths.get(ref.ID); ok {
		if n, err := d.resolvePath(ctx, fsys, p); err == nil && n.key == ref.ID {
			return n, nil
		}
	}
	paths, err := d.findPaths(ctx, fsys, ref.ID, 1)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("id %s: %w", ref.ID, drive.ErrNotFound)
	}
	d.idPaths.put(ref.ID, paths[0])
	return d.resolvePath(ctx, fsys, paths[0])
}

// findPaths is up to max (0: all) drive-relative paths naming id.
func (d *Drive) findPaths(ctx context.Context, fsys *icbfs.Filesystem, id string, max int) ([]string, error) {
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
	if parents {
		if _, err := d.mkdirAll(ctx, dir); err != nil {
			return nil, "", err
		}
	}
	parent, err := d.resolvePath(ctx, d.fs, dir)
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
			if obj, err := n.fsys.Store().Head(ctx, n.key); err == nil {
				dates, _ := objstore.ReadDates(ctx, n.fsys.Store(), n.key)
				it.Version = &drive.Version{ID: obj.VersionID, Time: dates.Time(obj), Stored: obj.LastModified, Size: obj.Size, Current: true,
					Checksums: toChecksums(n.attr.Checksums[obj.ETag])}
			}
		}
	case icbfs.TypeSymlink:
		if t, err := n.fsys.Readlink(ctx, n.key); err == nil {
			it.Target = t
		}
	}
	if o, ok := n.attr.App[icbfs.AppDriveOrigin]; ok {
		it.Origin = decodeOrigin(o)
	}
	it.App = n.attr.App
	it.Mode = n.attr.Mode
	if bits, ok := n.attr.WindowsAttributes(); ok {
		it.WindowsAttrs = &bits
	}
	return it
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
		errors.Is(err, drive.ErrInvalid), errors.Is(err, drive.ErrChecksum):
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
	case objstore.IsPreconditionFailed(err):
		return fmt.Errorf("%w (%v)", drive.ErrBusy, err)
	}
	return err
}

// --- the id → path memory ---

type idPathCache struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *idPathCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[id]
	return p, ok
}

func (c *idPathCache) put(id, p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 100000 {
		c.m = map[string]string{}
	}
	c.m[id] = p
}
