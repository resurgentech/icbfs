package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// --- named versions ---

// A name made on a drive has the drive's root as its scope; a drive sees
// the filesystem's names and the names of drives at, above or below its
// own root — not a sibling's.
func (d *Drive) nameVisible(scope string) bool {
	scope = cleanPath(scope)
	if scope == "" || d.root == "" {
		return true
	}
	return scope == d.root || strings.HasPrefix(d.root, scope+"/") || strings.HasPrefix(scope, d.root+"/")
}

func (d *Drive) toName(nv icbfs.NamedVersion) drive.Name {
	n := drive.Name{Name: nv.Name, Time: nv.Time, CreatedBy: nv.CreatedBy, CreatedAt: nv.CreatedAt}
	if nv.Scope == "" {
		n.FilesystemWide = true
	} else if p, ok := d.drivePath(nv.Scope); ok {
		n.Scope = p
	} else {
		n.Scope = "(above)"
	}
	return n
}

// Names lists the names this drive sees, oldest moment first.
func (d *Drive) Names(ctx context.Context) ([]drive.Name, error) {
	if _, err := profile(ctx); err != nil {
		return nil, err
	}
	all, err := icbfs.ListNames(ctx, d.cfg.Store, d.cfg.Filesystem)
	if err != nil {
		return nil, mapErr(err)
	}
	out := []drive.Name{}
	for _, nv := range all {
		if d.nameVisible(nv.Scope) {
			out = append(out, d.toName(nv))
		}
	}
	return out, nil
}

func (d *Drive) lookupName(ctx context.Context, name string) (icbfs.NamedVersion, error) {
	nv, err := icbfs.ResolveName(ctx, d.cfg.Store, d.cfg.Filesystem, name)
	if err != nil {
		return icbfs.NamedVersion{}, mapErr(err)
	}
	if !d.nameVisible(nv.Scope) {
		return icbfs.NamedVersion{}, fmt.Errorf("version name %q: %w", name, drive.ErrNotFound)
	}
	return nv, nil
}

// CreateName names now (a zero at) or a past moment, for this drive's
// subtree or the whole filesystem.
func (d *Drive) CreateName(ctx context.Context, name string, at time.Time, filesystemWide bool) (drive.Name, error) {
	if _, err := profile(ctx); err != nil {
		return drive.Name{}, err
	}
	scope := d.root
	if filesystemWide {
		scope = ""
	}
	if err := icbfs.CheckVersionName(name); err != nil {
		return drive.Name{}, fmt.Errorf("%w: %v", drive.ErrInvalidName, err)
	}
	nv, err := icbfs.CreateName(ctx, d.cfg.Store, d.cfg.Filesystem, icbfs.NamedVersion{Name: name, Scope: scope, CreatedBy: drive.Actor(ctx)}, at)
	if err != nil {
		if errors.Is(err, icbfs.ErrExists) || errors.Is(err, icbfs.ErrNotFound) {
			return drive.Name{}, mapErr(err)
		}
		return drive.Name{}, fmt.Errorf("%w: %v", drive.ErrInvalid, err)
	}
	return d.toName(nv), nil
}

// DeleteName removes a name this drive sees.
func (d *Drive) DeleteName(ctx context.Context, name string) error {
	if _, err := profile(ctx); err != nil {
		return err
	}
	if _, err := d.lookupName(ctx, name); err != nil {
		return err
	}
	return mapErr(icbfs.DeleteName(ctx, d.cfg.Store, d.cfg.Filesystem, name))
}

// --- trash ---

func (d *Drive) toTrash(e icbfs.TrashEntry) (drive.TrashEntry, bool) {
	out := drive.TrashEntry{ID: e.EntryID, ItemID: e.ItemID, Name: e.Name, Type: typeName(e.Type),
		DeletedAt: e.DeletedAt, DeletedBy: e.DeletedBy, Size: e.Size, Version: e.Version, Batch: e.Batch}
	if e.Path == "" {
		// Deleted where no path was known: only the whole filesystem's
		// drive can claim it.
		return out, d.root == ""
	}
	p, ok := d.drivePath(e.Path)
	if !ok || p == "/" {
		return out, false
	}
	out.Path = p
	return out, true
}

// Trash lists what was deleted from this drive, newest first.
func (d *Drive) Trash(ctx context.Context, opts drive.TrashOptions) (drive.TrashPage, error) {
	root, err := d.rootNode(ctx, d.fs)
	if err != nil {
		return drive.TrashPage{}, err
	}
	if err := d.check(ctx, root, needRead); err != nil {
		return drive.TrashPage{}, err
	}
	prefix := d.root
	if opts.Path != "" {
		prefix = d.fsPath(opts.Path)
	}
	entries, err := d.fs.ListTrash(ctx, icbfs.TrashOptions{PathPrefix: prefix, After: opts.Cursor})
	if err != nil {
		return drive.TrashPage{}, mapErr(err)
	}
	page := drive.TrashPage{Entries: []drive.TrashEntry{}}
	for _, e := range entries {
		te, ok := d.toTrash(e)
		if !ok {
			continue
		}
		if opts.Limit > 0 && len(page.Entries) == opts.Limit {
			page.Next = page.Entries[len(page.Entries)-1].ID
			break
		}
		page.Entries = append(page.Entries, te)
	}
	return page, nil
}

// trashEntry finds a trash entry of this drive by its id.
func (d *Drive) trashEntry(ctx context.Context, id string) (icbfs.TrashEntry, drive.TrashEntry, error) {
	entries, err := d.fs.ListTrash(ctx, icbfs.TrashOptions{})
	if err != nil {
		return icbfs.TrashEntry{}, drive.TrashEntry{}, mapErr(err)
	}
	for _, e := range entries {
		if e.EntryID == id {
			if te, ok := d.toTrash(e); ok {
				return e, te, nil
			}
			break
		}
	}
	return icbfs.TrashEntry{}, drive.TrashEntry{}, fmt.Errorf("trash entry %s: %w", id, drive.ErrNotFound)
}

// trashedItem is the newest trash entry of this drive for an item id.
func (d *Drive) trashedItem(ctx context.Context, itemID string) (drive.TrashEntry, error) {
	entries, err := d.fs.ListTrash(ctx, icbfs.TrashOptions{})
	if err != nil {
		return drive.TrashEntry{}, mapErr(err)
	}
	for _, e := range entries {
		if e.ItemID == itemID {
			if te, ok := d.toTrash(e); ok {
				return te, nil
			}
		}
	}
	return drive.TrashEntry{}, fmt.Errorf("id %s: %w", itemID, drive.ErrNotFound)
}

// Restore brings a deleted item back: to where it was, or to opts.To.
func (d *Drive) Restore(ctx context.Context, id string, opts drive.RestoreOptions) (drive.Item, error) {
	if err := d.writable(ctx); err != nil {
		return drive.Item{}, err
	}
	_, te, err := d.trashEntry(ctx, id)
	if err != nil {
		return drive.Item{}, err
	}
	to := opts.To
	if to == "" {
		if te.Path == "" {
			return drive.Item{}, fmt.Errorf("where it was isn't known; say where: %w", drive.ErrInvalid)
		}
		to = te.Path
	}
	parent, name, err := d.resolveParent(ctx, to, opts.Parents)
	if err != nil {
		return drive.Item{}, err
	}
	what := needAddFile
	if te.Type == drive.TypeDir {
		what = needAddDir
	}
	if err := d.check(ctx, parent, what); err != nil {
		return drive.Item{}, err
	}
	if opts.Replace {
		if existing, err := d.resolvePath(ctx, d.fs, joinPath(parent.path, name)); err == nil {
			if err := d.canDelete(ctx, existing); err != nil {
				return drive.Item{}, err
			}
		}
	}
	rctx := icbfs.WithDeleteInfo(ctx, icbfs.DeleteInfo{By: drive.Actor(ctx), Path: d.fsPath(joinPath(parent.path, name))})
	if _, err := d.fs.Restore(rctx, id, icbfs.RestoreOptions{ToDir: parent.key, ToName: name, Replace: opts.Replace}); err != nil {
		return drive.Item{}, mapErr(err)
	}
	d.changed(parent)
	return d.statItem(ctx, joinPath(parent.path, name))
}

// Purge permanently deletes a live or trashed item.
func (d *Drive) Purge(ctx context.Context, ref drive.ItemRef, allNames bool) error {
	if err := d.writable(ctx); err != nil {
		return err
	}
	n, err := d.resolve(ctx, d.fs, ref)
	if err == nil {
		if n.path == "/" {
			return fmt.Errorf("the drive's root: %w", drive.ErrInvalidName)
		}
		if err := d.canDelete(ctx, n); err != nil {
			return err
		}
		if err := d.fs.Purge(ctx, n.parentKey(), n.name, allNames); err != nil {
			return mapErr(err)
		}
		d.changed(parentNode(n))
		return nil
	}
	if ref.ID == "" || !errors.Is(err, drive.ErrNotFound) {
		return err
	}
	te, terr := d.trashedItem(ctx, ref.ID)
	if terr != nil {
		return err
	}
	root, err := d.rootNode(ctx, d.fs)
	if err != nil {
		return err
	}
	// A trashed item has no directory left to ask: writing to the
	// drive's root decides.
	if err := d.check(ctx, root, needAddFile); err != nil {
		return err
	}
	return mapErr(d.fs.PurgeTrash(ctx, te.ID))
}

// --- change events ---

type broadcaster struct {
	mu   sync.Mutex
	subs map[chan drive.Change]struct{}
}

// Changes streams this drive's changes until ctx ends.
func (d *Drive) Changes(ctx context.Context) (<-chan drive.Change, error) {
	ch := make(chan drive.Change, 64)
	b := &d.events
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[chan drive.Change]struct{}{}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

func (b *broadcaster) send(c drive.Change) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- c:
		default: // a slow subscriber misses changes, as with any notification
		}
	}
}

// changed reports that directory dir changed.
func (d *Drive) changed(dir *node) {
	if dir == nil {
		return
	}
	p := dir.path
	if p == "" {
		p = "/"
	}
	d.events.send(drive.Change{Path: p, ID: dir.key})
}

// Notify feeds changes from elsewhere (mounts' writes, through icbfs
// change notifications) to Changes subscribers as bare changes.
func (d *Drive) Notify(signals <-chan struct{}) {
	go func() {
		for range signals {
			d.events.send(drive.Change{})
		}
	}()
}
