package icbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// ErrHasLinks: a permanent delete of a file that still has other names.
var ErrHasLinks = errors.New("file has other names")

// PurgeReport is what a purge did beyond erasing: the items it retired.
type PurgeReport struct {
	Retired []RetiredItem `json:"retired,omitempty"`
}

// RetiredItem is an item a purge retired rather than erased, because
// stubs elsewhere name its versions (see Purge): its key and name, and
// its live referrers.
type RetiredItem struct {
	Key       string            `json:"key"`
	Name      string            `json:"name"`
	Referrers []RetiredReferrer `json:"referrers"`
}

// RetiredReferrer is one referrer of a retired item: the stub's key, and
// a path to it if one resolves (a trashed referrer has none).
type RetiredReferrer struct {
	Key  string `json:"key"`
	Path string `json:"path,omitempty"`
}

// Purge permanently deletes the item dirKey/name — a directory with
// everything under it: every version of its content, its .metadata and
// its streams is erased, and so is any trash entry for it, so neither
// the trash nor a past-time mount can bring it back. What stays is its
// name in its directory's own history (a past-time listing still shows
// the name; opening it finds nothing). A file with other names, or a
// directory holding one, is ErrHasLinks unless allNames, which finds the
// other names (a walk of the whole tree) and removes them too.
//
// An item whose versions stubs elsewhere name (live referrers, counted
// once the purge's own items have dropped theirs) is retired instead:
// everything above goes, but the versions they name stay under its key,
// behind a delete marker, with its .refs beside them, for the stubs to
// read on; the last referrer to go erases them (dropRef). Before it
// erases anything, the purge marks each item's .refs retired, so a stub
// written meanwhile is refused rather than left naming nothing.
// PurgeWithReport says which items were retired.
func (f *Filesystem) Purge(ctx context.Context, dirKey, name string, allNames bool) error {
	_, err := f.PurgeWithReport(ctx, dirKey, name, allNames)
	return err
}

// PurgeWithReport is Purge, reporting the items it retired.
func (f *Filesystem) PurgeWithReport(ctx context.Context, dirKey, name string, allNames bool) (PurgeReport, error) {
	if err := f.checkWritable(); err != nil {
		return PurgeReport{}, err
	}
	entry, _, err := f.Lookup(ctx, dirKey, name)
	if err != nil {
		return PurgeReport{}, err
	}
	var items []block.Entry
	linked := map[string]bool{}
	if err := f.collectForPurge(ctx, entry, &items, linked); err != nil {
		return PurgeReport{}, err
	}
	if len(linked) > 0 && !allNames {
		return PurgeReport{}, fmt.Errorf("purge %s: %w", name, ErrHasLinks)
	}
	if _, err := f.removeEntry(ctx, dirKey, name); err != nil {
		return PurgeReport{}, err
	}
	touched := f.touchDir(ctx, dirKey) // reported once the purge is done
	if len(linked) > 0 {
		// The purged subtree is out of the tree now, so whatever names
		// the walk finds are the other ones.
		if err := f.removeNamesOf(ctx, f.RootKey(), linked); err != nil {
			return PurgeReport{}, err
		}
	}
	rep, err := f.eraseAndMark(ctx, items)
	if err != nil {
		return rep, err
	}
	return rep, touched
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
			if err := f.touchDir(ctx, dirKey); err != nil {
				return err
			}
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
//
// An item with live referrers is retired, as Purge's are.
func (f *Filesystem) PurgeTrash(ctx context.Context, entryID string) error {
	_, err := f.PurgeTrashWithReport(ctx, entryID)
	return err
}

// PurgeTrashWithReport is PurgeTrash, reporting the items it retired.
func (f *Filesystem) PurgeTrashWithReport(ctx context.Context, entryID string) (PurgeReport, error) {
	if err := f.checkWritable(); err != nil {
		return PurgeReport{}, err
	}
	e, key, err := f.trashEntry(ctx, entryID)
	if err != nil {
		return PurgeReport{}, err
	}
	if live, err := f.itemLive(ctx, e.ItemID, e.Type); err != nil {
		return PurgeReport{}, err
	} else if live {
		f.dropStaleEntry(ctx, e, key)
		return PurgeReport{}, fmt.Errorf("trash entry %s: %s isn't deleted: %w", entryID, e.Name, ErrNotFound)
	}
	items := []block.Entry{{Name: e.Name, UUID: e.ItemID, Type: e.Type}}
	if e.Type == TypeDir && e.Batch == e.EntryID {
		rest, err := f.ListTrash(ctx, TrashOptions{Batch: e.Batch})
		if err != nil {
			return PurgeReport{}, err
		}
		for _, r := range rest {
			if r.EntryID == e.EntryID {
				continue
			}
			live, err := f.itemLive(ctx, r.ItemID, r.Type)
			if err != nil {
				return PurgeReport{}, err
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
func (f *Filesystem) eraseAndMark(ctx context.Context, items []block.Entry) (PurgeReport, error) {
	rep, err := f.eraseItems(ctx, items)
	if markErr := f.markPast(ctx); err == nil {
		err = markErr
	}
	return rep, err
}

// eraseItems erases every version of each item's objects, then its trash
// entries; an item with live referrers is retired (retireItem) instead
// of erased whole. In this order:
//  1. each item's .refs is marked retired, before anything is erased;
//  2. each file drops the entries its stubs have in their targets'
//     .refs (from its .metadata's refs), so the referrers counted next
//     are the ones outside the purge;
//  3. each item is erased whole, or retired.
func (f *Filesystem) eraseItems(ctx context.Context, items []block.Entry) (PurgeReport, error) {
	var rep PurgeReport
	gone := map[string]bool{}
	for _, it := range items {
		if it.UUID == f.RootKey() {
			return rep, fmt.Errorf("purge: the root can't be purged")
		}
		gone[it.UUID] = true
	}
	for _, it := range items {
		if err := f.markRetired(ctx, it.UUID); err != nil {
			return rep, fmt.Errorf("purge %s: %w", it.Name, err)
		}
	}
	for _, it := range items {
		if it.Type != TypeFile {
			continue
		}
		if err := f.dropStubsOf(ctx, it.UUID); err != nil {
			return rep, fmt.Errorf("purge %s: %w", it.Name, err)
		}
	}
	for _, it := range items {
		f.openMu.Lock()
		delete(f.opens.orphans, it.UUID)
		f.openMu.Unlock()
		if it.Type == TypeDir {
			if err := f.eraseDirBlocks(ctx, it.UUID); err != nil {
				return rep, err
			}
		}
		live, err := f.liveReferrers(ctx, it.UUID, gone)
		if err != nil {
			return rep, fmt.Errorf("purge %s: %w", it.Name, err)
		}
		if len(live) > 0 {
			if err := f.retireItem(ctx, it.UUID, live); err != nil {
				return rep, fmt.Errorf("purge %s: %w", it.Name, err)
			}
			r := RetiredItem{Key: it.UUID, Name: it.Name}
			listed := map[string]bool{}
			for _, e := range live {
				if !listed[e.ReferrerKey] {
					listed[e.ReferrerKey] = true
					r.Referrers = append(r.Referrers, RetiredReferrer{Key: e.ReferrerKey})
				}
			}
			rep.Retired = append(rep.Retired, r)
		} else if err := f.store.EraseAll(ctx, it.UUID); err != nil {
			// The item's key, its .metadata, its streams and its .refs all
			// start with it (its id is fixed-length, so nothing else
			// does), the key's own versions listed, and erased, first.
			return rep, fmt.Errorf("purge %s: %w", it.Name, err)
		}
		if err := f.store.EraseAll(ctx, f.trashByKey(it.UUID)); err != nil {
			return rep, fmt.Errorf("purge %s: %w", it.Name, err)
		}
	}
	objs, err := f.store.List(ctx, f.trashPrefix())
	if err != nil {
		return rep, err
	}
	for _, o := range objs {
		te, err := f.readTrash(ctx, o.Key)
		if err != nil || !gone[te.ItemID] {
			continue
		}
		if err := f.store.EraseAll(ctx, o.Key); err != nil {
			return rep, err
		}
	}
	f.resolveReferrerPaths(ctx, &rep)
	return rep, nil
}

// dropStubsOf removes, from each target's .refs, the entries of file
// key's stubs (its .metadata's refs, or a trashed file's last one's): for
// a purge of key. A target already gone is fine.
func (f *Filesystem) dropStubsOf(ctx context.Context, key string) error {
	refs, err := f.contentRefs(ctx, key)
	if err != nil {
		return err
	}
	targets := map[string]bool{}
	for _, r := range refs {
		targets[r.Key] = true
	}
	for target := range targets {
		if err := f.dropRef(ctx, target, func(e *pb.Referrer) bool { return e.ReferrerKey == key }); err != nil {
			return err
		}
	}
	return nil
}

// retireItem leaves of item key only a retired holder: the content
// versions live names, behind a delete marker on key (put first, so a
// plain Get finds nothing; a store can't erase a current version), and
// its .refs. Every other version of the key goes, and every version of
// its other objects (.metadata, streams, .lock, .version).
func (f *Filesystem) retireItem(ctx context.Context, key string, live []*pb.Referrer) error {
	ctx = undeclared(ctx)
	keep := map[string]bool{}
	for _, e := range live {
		keep[e.TargetVersion] = true
	}
	if err := f.store.Delete(ctx, key); err != nil && !objstore.IsNotFound(err) {
		return err
	}
	history, err := f.store.History(ctx, key)
	if err != nil {
		return err
	}
	for i, v := range history {
		if i == 0 && v.Deleted || !v.Deleted && keep[v.VersionID] {
			continue // the marker just put, and what the referrers name
		}
		if err := f.store.Erase(ctx, key, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	under, err := f.store.HistoryUnder(ctx, key)
	if err != nil {
		return err
	}
	others := map[string]bool{}
	for _, v := range under {
		if v.Key != key && v.Key != refsObjectKey(key) {
			others[v.Key] = true
		}
	}
	for k := range others {
		// No other object's key is a start of the .refs's.
		if err := f.store.EraseAll(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// resolveReferrerPaths fills in a path for each retired item's referrers
// it can find by a walk of the tree, best effort: a referrer with no name
// (a trashed one) or a walk that fails leaves it empty.
func (f *Filesystem) resolveReferrerPaths(ctx context.Context, rep *PurgeReport) {
	want := map[string]string{}
	for _, r := range rep.Retired {
		for _, ref := range r.Referrers {
			want[ref.Key] = ""
		}
	}
	if len(want) == 0 {
		return
	}
	left := len(want)
	type dir struct{ key, path string }
	queue := []dir{{f.RootKey(), "/"}}
	seen := map[string]bool{}
	for len(queue) > 0 && left > 0 {
		d := queue[0]
		queue = queue[1:]
		entries, err := f.ReadDir(ctx, d.key)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := path.Join(d.path, e.Name)
			if cur, ok := want[e.UUID]; ok && cur == "" {
				want[e.UUID] = p
				left--
			}
			if e.Type == TypeDir && !seen[e.UUID] {
				seen[e.UUID] = true
				queue = append(queue, dir{e.UUID, p})
			}
		}
	}
	for i := range rep.Retired {
		for j := range rep.Retired[i].Referrers {
			rep.Retired[i].Referrers[j].Path = want[rep.Retired[i].Referrers[j].Key]
		}
	}
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
