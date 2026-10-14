package icbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// ErrHasLinks: a permanent delete of a file that still has other names.
var ErrHasLinks = errors.New("file has other names")

// Purge permanently deletes the item dirKey/name — a directory with
// everything under it: every version of its content, its .metadata and
// its streams is erased, and so is any trash entry for it, so neither
// the trash nor a past-time mount can bring it back. What stays is its
// name in its directory's own history (a past-time listing still shows
// the name; opening it finds nothing). A file with other names, or a
// directory holding one, is ErrHasLinks unless allNames, which finds the
// other names (a walk of the whole tree) and removes them too.
func (f *Filesystem) Purge(ctx context.Context, dirKey, name string, allNames bool) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	entry, _, err := f.Lookup(ctx, dirKey, name)
	if err != nil {
		return err
	}
	var items []block.Entry
	linked := map[string]bool{}
	if err := f.collectForPurge(ctx, entry, &items, linked); err != nil {
		return err
	}
	if len(linked) > 0 && !allNames {
		return fmt.Errorf("purge %s: %w", name, ErrHasLinks)
	}
	if _, err := f.removeEntry(ctx, dirKey, name); err != nil {
		return err
	}
	f.touchDir(ctx, dirKey)
	if len(linked) > 0 {
		// The purged subtree is out of the tree now, so whatever names
		// the walk finds are the other ones.
		if err := f.removeNamesOf(ctx, f.RootKey(), linked); err != nil {
			return err
		}
	}
	return f.eraseItems(ctx, items)
}

// collectForPurge lists e and, for a directory, everything under it
// (children before their directory), noting files with other names.
func (f *Filesystem) collectForPurge(ctx context.Context, e block.Entry, out *[]block.Entry, linked map[string]bool) error {
	if e.Type == TypeDir {
		children, err := f.ReadDir(ctx, e.UUID)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := f.collectForPurge(ctx, c, out, linked); err != nil {
				return err
			}
		}
	} else if e.Type != TypeSymlink {
		attr, err := f.Stat(ctx, e.UUID, e.Type)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if attr.Nlink > 1 {
			linked[e.UUID] = true
		}
	}
	*out = append(*out, e)
	return nil
}

// removeNamesOf removes, under dirKey, every entry naming one of ids.
func (f *Filesystem) removeNamesOf(ctx context.Context, dirKey string, ids map[string]bool) error {
	entries, err := f.ReadDir(ctx, dirKey)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch {
		case ids[e.UUID]:
			if _, err := f.removeEntry(ctx, dirKey, e.Name); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			f.touchDir(ctx, dirKey)
		case e.Type == TypeDir:
			if err := f.removeNamesOf(ctx, e.UUID, ids); err != nil {
				return err
			}
		}
	}
	return nil
}

// PurgeTrash permanently deletes a deleted item from the trash: its
// history is erased as Purge's is, and its trash entry goes. A directory
// that started a recursive delete takes its whole batch with it.
func (f *Filesystem) PurgeTrash(ctx context.Context, entryID string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	e, _, err := f.trashEntry(ctx, entryID)
	if err != nil {
		return err
	}
	items := []block.Entry{{Name: e.Name, UUID: e.ItemID, Type: e.Type}}
	if e.Type == TypeDir && e.Batch == e.EntryID {
		rest, err := f.ListTrash(ctx, TrashOptions{Batch: e.Batch})
		if err != nil {
			return err
		}
		for _, r := range rest {
			if r.EntryID != e.EntryID {
				items = append(items, block.Entry{Name: r.Name, UUID: r.ItemID, Type: r.Type})
			}
		}
	}
	return f.eraseItems(ctx, items)
}

// eraseItems erases every version of each item's objects, then its trash
// entries.
func (f *Filesystem) eraseItems(ctx context.Context, items []block.Entry) error {
	erased := map[string]bool{}
	for _, it := range items {
		if it.UUID == f.RootKey() {
			return fmt.Errorf("purge: the root can't be purged")
		}
		f.openMu.Lock()
		delete(f.opens.orphans, it.UUID)
		f.openMu.Unlock()
		if it.Type == TypeDir {
			if err := f.eraseDirBlocks(ctx, it.UUID); err != nil {
				return err
			}
		}
		// The item's key, its .metadata and its streams all start with it
		// (its id is fixed-length, so nothing else does).
		if err := eraseAllVersions(ctx, f.store, it.UUID); err != nil {
			return fmt.Errorf("purge %s: %w", it.Name, err)
		}
		erased[it.UUID] = true
	}
	objs, err := f.store.ListByPrefix(ctx, f.trashPrefix())
	if err != nil {
		return err
	}
	for _, o := range objs {
		te, err := f.readTrash(ctx, o.Key)
		if err != nil || !erased[te.ItemID] {
			continue
		}
		if err := eraseAllVersions(ctx, f.store, o.Key); err != nil {
			return err
		}
	}
	return nil
}

// eraseDirBlocks erases the blocks a split directory's tree has had
// under other keys: every version of its root block names the child
// blocks it had then, and theirs theirs.
func (f *Filesystem) eraseDirBlocks(ctx context.Context, rootKey string) error {
	seen := map[string]bool{rootKey: true}
	queue := []string{rootKey}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		versions, err := f.store.ListVersions(ctx, key)
		if err != nil {
			return err
		}
		for _, v := range versions {
			if v.DeleteMarker {
				continue
			}
			body, _, err := f.store.GetVersion(ctx, key, v.VersionID)
			if err != nil {
				continue
			}
			data, err := io.ReadAll(body)
			body.Close()
			if err != nil {
				return err
			}
			blk, err := block.Decode(data)
			if err != nil || blk.Kind != block.Internal {
				continue
			}
			for _, c := range blk.Children {
				if !seen[c.UUID] {
					seen[c.UUID] = true
					queue = append(queue, c.UUID)
				}
			}
		}
		if key != rootKey {
			if err := eraseAllVersions(ctx, f.store, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// eraseAllVersions deletes every version, delete markers included, of
// every key starting with prefix.
func eraseAllVersions(ctx context.Context, store objstore.Store, prefix string) error {
	versions, err := store.ListAllVersions(ctx, prefix)
	if err != nil {
		return err
	}
	for _, v := range versions {
		if !strings.HasPrefix(v.Key, prefix) {
			continue
		}
		if err := store.DeleteVersion(ctx, v.Key, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return fmt.Errorf("erase %s@%s: %w", v.Key, v.VersionID, err)
		}
	}
	return nil
}
