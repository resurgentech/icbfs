package icbfs

import (
	"context"
	"errors"
	"fmt"
	"io"

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
	return f.eraseAndMark(ctx, items)
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
//
// Only what is still deleted: an entry whose item is live again (restored
// with its entry left behind) is stale, and goes on its own, the item
// untouched; for the entry asked for, that's ErrNotFound.
func (f *Filesystem) PurgeTrash(ctx context.Context, entryID string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	e, key, err := f.trashEntry(ctx, entryID)
	if err != nil {
		return err
	}
	if live, err := f.itemLive(ctx, e.ItemID, e.Type); err != nil {
		return err
	} else if live {
		f.dropStaleEntry(ctx, e, key)
		return fmt.Errorf("trash entry %s: %s isn't deleted: %w", entryID, e.Name, ErrNotFound)
	}
	items := []block.Entry{{Name: e.Name, UUID: e.ItemID, Type: e.Type}}
	if e.Type == TypeDir && e.Batch == e.EntryID {
		rest, err := f.ListTrash(ctx, TrashOptions{Batch: e.Batch})
		if err != nil {
			return err
		}
		for _, r := range rest {
			if r.EntryID == e.EntryID {
				continue
			}
			live, err := f.itemLive(ctx, r.ItemID, r.Type)
			if err != nil {
				return err
			}
			if live {
				f.dropStaleEntry(ctx, r, f.trashKey(r))
				continue
			}
			items = append(items, block.Entry{Name: r.Name, UUID: r.ItemID, Type: r.Type})
		}
	}
	return f.eraseAndMark(ctx, items)
}

// itemLive reports whether item id is live: its .metadata is there and,
// for a file, isn't the zero link count a delete writes first. A
// deleted item's objects end in a deletion.
func (f *Filesystem) itemLive(ctx context.Context, id string, typ EntryType) (bool, error) {
	body, _, err := f.store.Get(ctx, metadataObjectKey(id))
	if objstore.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return false, err
	}
	if typ == TypeDir {
		return true, nil
	}
	attr, err := decodeFileMetadata(data)
	if err != nil {
		return false, err
	}
	return attr.Nlink != nlinkZero, nil
}

// dropStaleEntry removes trash entry e, at key, whose item is live again,
// and the item's pointer to it. Nothing about the item depends on it.
func (f *Filesystem) dropStaleEntry(ctx context.Context, e TrashEntry, key string) {
	_ = f.store.Delete(ctx, key)
	if body, _, err := f.store.Get(ctx, f.trashByKey(e.ItemID)); err == nil {
		pointed, _ := io.ReadAll(body)
		body.Close()
		if string(pointed) == key {
			_ = f.store.Delete(ctx, f.trashByKey(e.ItemID))
		}
	}
}

// eraseAndMark erases items, then marks the filesystem's past changed,
// even if the erase failed partway: some of it may be gone.
func (f *Filesystem) eraseAndMark(ctx context.Context, items []block.Entry) error {
	err := f.eraseItems(ctx, items)
	if markErr := f.markPast(ctx); err == nil {
		err = markErr
	}
	return err
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
		if err := f.store.EraseAll(ctx, it.UUID); err != nil {
			return fmt.Errorf("purge %s: %w", it.Name, err)
		}
		if err := f.store.EraseAll(ctx, f.trashByKey(it.UUID)); err != nil {
			return fmt.Errorf("purge %s: %w", it.Name, err)
		}
		erased[it.UUID] = true
	}
	objs, err := f.store.List(ctx, f.trashPrefix())
	if err != nil {
		return err
	}
	for _, o := range objs {
		te, err := f.readTrash(ctx, o.Key)
		if err != nil || !erased[te.ItemID] {
			continue
		}
		if err := f.store.EraseAll(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

// eraseDirBlocks erases the blocks a split directory's tree has had
// under other keys: every version of its root block names the child
// blocks it had then, and theirs theirs.
func (f *Filesystem) eraseDirBlocks(ctx context.Context, rootKey string) error {
	keys, _, err := f.dirBlockHistory(ctx, rootKey)
	if err != nil {
		return err
	}
	for _, k := range keys[1:] {
		if err := f.store.EraseAll(ctx, k); err != nil {
			return err
		}
	}
	return nil
}
