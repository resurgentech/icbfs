package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// TrashEntry records one deleted item: a file whose last name went, or a
// removed directory. Entries are kept forever, under this filesystem's
// prefix (<id>-trash-<inverted time>-<entry id>, so a listing is newest
// first). Restoring works because an item is stored under its id, not its
// name: deleting it left its versions in place, so restoring makes the
// last one current again and gives it a name — and its history carries on.
type TrashEntry struct {
	EntryID   string    `json:"entryId"`
	ItemID    string    `json:"itemId"` // the item's key, its stable id
	Type      EntryType `json:"type"`
	Name      string    `json:"name"`
	ParentID  string    `json:"parentId"`       // the directory it was in
	Path      string    `json:"path,omitempty"` // as the deleting access layer saw it, when it knew
	DeletedAt time.Time `json:"deletedAt"`
	DeletedBy string    `json:"deletedBy,omitempty"`
	Size      int64     `json:"size"`
	Version   string    `json:"version,omitempty"` // the content version current when it was deleted
	// Batch is shared by everything one recursive delete removed; the
	// directory it started from has Batch == EntryID.
	Batch string `json:"batch,omitempty"`
	// Origin is where the item had been copied from (its AppDriveOrigin
	// app data, as the drive API encodes it) when it was deleted, so a
	// mirror can find its entry when the source brings the item back.
	Origin []byte `json:"origin,omitempty"`
	// Pass is the sync pass that removed it, when one did (DeleteInfo.Pass),
	// so a whole pass's removals can be found and undone.
	Pass string `json:"pass,omitempty"`
}

// DeleteInfo is what the core can't know about a delete: who made it,
// the path the deleted item had (the core has no parent pointers), and,
// for a recursive delete, the batch it belongs to. Access layers attach it
// to the context of the call that deletes (WithDeleteInfo).
type DeleteInfo struct {
	By    string
	Path  string
	Batch string
	// EntryID, if set, is the trash entry's id: a recursive delete gives
	// the directory it started from its batch's id.
	EntryID string
	// NoTrash: an access layer's internal delete (replacing a placeholder),
	// not a user's, so nothing is recorded.
	NoTrash bool
	// Pass names the sync pass deleting (a mirror's), recorded on the
	// entry so the pass's removals can be undone together.
	Pass string
}

type deleteInfoKey struct{}

// WithDeleteInfo attaches who is deleting, and what path, to ctx.
func WithDeleteInfo(ctx context.Context, info DeleteInfo) context.Context {
	return context.WithValue(ctx, deleteInfoKey{}, info)
}

func deleteInfoFrom(ctx context.Context) DeleteInfo {
	info, _ := ctx.Value(deleteInfoKey{}).(DeleteInfo)
	return info
}

func (f *Filesystem) trashPrefix() string { return f.id + "-trash-" }

// trashByKey is the object naming an item's newest trash entry (its key),
// so finding a trashed item's entry is one read, not a scan of the trash,
// which is kept forever and only grows. Written with the entry, removed by
// a restore, erased by a purge.
func (f *Filesystem) trashByKey(itemID string) string {
	return f.id + "-trashby-" + strings.TrimPrefix(itemID, f.id+"-")
}

// TrashedItem returns the newest trash entry for the item itemID (the
// item's key), in three reads, or ErrNotFound if it isn't in the trash
// (an entry whose item is live again is stale).
// Entries made before items had a pointer to theirs aren't found this way;
// ListTrash still lists them.
func (f *Filesystem) TrashedItem(ctx context.Context, itemID string) (TrashEntry, error) {
	body, _, err := f.store.Get(ctx, f.trashByKey(itemID))
	if err != nil {
		return TrashEntry{}, mapNotFound(err)
	}
	key, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return TrashEntry{}, err
	}
	e, err := f.readTrash(ctx, string(key))
	if err != nil {
		return TrashEntry{}, err
	}
	if e.ItemID != itemID {
		return TrashEntry{}, fmt.Errorf("trashed item %s: %w", itemID, ErrNotFound)
	}
	if live, err := f.itemLive(ctx, itemID, e.Type); err != nil {
		return TrashEntry{}, err
	} else if live {
		return TrashEntry{}, fmt.Errorf("trashed item %s is live: %w", itemID, ErrNotFound)
	}
	return e, nil
}

func (f *Filesystem) trashKey(e TrashEntry) string {
	return fmt.Sprintf("%s%019d-%s", f.trashPrefix(), math.MaxInt64-e.DeletedAt.UnixNano(), e.EntryID)
}

// A delete writes its trash entry first, then removes the item
// (trashFirst): a delete that can't record itself fails before it has
// changed anything, and one that stops halfway leaves an entry whose item
// is live, which everything reading the trash checks for (itemLive) and
// passes over. The other order would leave a deleted item nothing points
// to: not restorable, not found by a mirror's undo, never purged.

// pendingTrash is a trash entry written ahead of the delete it records.
type pendingTrash struct {
	e   TrashEntry
	key string
}

// trashFirst writes the trash entry for deleting item from dirKey under
// name, before the delete: nil if there's none to write (an access
// layer's own delete, DeleteInfo.NoTrash, or a file that has other
// names). attr is the item's, if the caller has it.
func (f *Filesystem) trashFirst(ctx context.Context, item block.Entry, dirKey, name string, attr *Attr) (*pendingTrash, error) {
	if deleteInfoFrom(ctx).NoTrash {
		return nil, nil
	}
	if attr == nil {
		a, err := f.Stat(ctx, item.UUID, item.Type)
		if err != nil {
			return nil, err
		}
		attr = &a
	}
	var size int64
	var version string
	if item.Type != TypeDir {
		if attr.Nlink > 1 {
			return nil, nil // not its last name: the item stays
		}
		size = attr.Size
		if obj, ok := attr.ContentVersion(); ok {
			version = obj.VersionID
		}
	}
	e, err := f.newTrashEntry(ctx, item.UUID, item.Type, dirKey, name, size, version, attr.App[AppDriveOrigin])
	if err != nil {
		return nil, err
	}
	if err := f.writeTrash(ctx, e); err != nil {
		return nil, err
	}
	return &pendingTrash{e: e, key: f.trashKey(e)}, nil
}

// cancel removes an entry written ahead of a delete that didn't happen.
// Left behind, it would only be stale.
func (p *pendingTrash) cancel(ctx context.Context, f *Filesystem) {
	if p != nil {
		f.dropStaleEntry(context.WithoutCancel(ctx), p.e, p.key)
	}
}

// recordTrash writes a trash entry for an item just deleted from dirKey
// under name, when trashFirst couldn't know it would be: a file whose
// other name went at the same time.
func (f *Filesystem) recordTrash(ctx context.Context, itemID string, typ EntryType, dirKey, name string, size int64, version string, origin []byte) error {
	if deleteInfoFrom(ctx).NoTrash {
		return nil
	}
	e, err := f.newTrashEntry(ctx, itemID, typ, dirKey, name, size, version, origin)
	if err != nil {
		return err
	}
	return f.writeTrash(ctx, e)
}

func (f *Filesystem) newTrashEntry(ctx context.Context, itemID string, typ EntryType, dirKey, name string, size int64, version string, origin []byte) (TrashEntry, error) {
	info := deleteInfoFrom(ctx)
	id := info.EntryID
	if id == "" {
		var err error
		if id, err = NewUUIDv7(); err != nil {
			return TrashEntry{}, err
		}
	}
	return TrashEntry{
		EntryID: id, ItemID: itemID, Type: typ, Name: name, ParentID: dirKey,
		Path: info.Path, DeletedAt: changeTime(ctx).UTC(), DeletedBy: info.By,
		Size: size, Version: version, Batch: info.Batch, Origin: origin, Pass: info.Pass,
	}, nil
}

// writeTrash writes entry e and the item's pointer to it, both or
// neither.
func (f *Filesystem) writeTrash(ctx context.Context, e TrashEntry) error {
	data, err := encodeTrashEntry(e)
	if err != nil {
		return err
	}
	key := f.trashKey(e)
	if _, err := f.store.PutIfAbsent(ctx, key, bytes.NewReader(data), nil); err != nil {
		return fmt.Errorf("trash entry for %s: %w", e.Name, err)
	}
	if _, err := f.store.Put(ctx, f.trashByKey(e.ItemID), strings.NewReader(key), nil, ""); err != nil {
		_ = f.store.Delete(context.WithoutCancel(ctx), key)
		return fmt.Errorf("trash entry for %s: %w", e.Name, err)
	}
	return nil
}

// TrashOptions narrows ListTrash.
type TrashOptions struct {
	PathPrefix string // only items deleted from under this path (entries without a path are left out)
	Batch      string // only this batch
	Pass       string // only what this sync pass removed
	Limit      int    // at most this many (0: all)
	After      string // continue after this entry id (a previous page's last)
}

// ListTrash lists deleted items, newest first. An entry whose item is
// live (a delete that stopped after writing it, a restore that couldn't
// remove it) isn't listed.
func (f *Filesystem) ListTrash(ctx context.Context, opts TrashOptions) ([]TrashEntry, error) {
	objs, err := f.store.List(ctx, f.trashPrefix())
	if err != nil {
		return nil, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	var out []TrashEntry
	skipping := opts.After != ""
	for _, o := range objs {
		if skipping {
			if strings.HasSuffix(o.Key, "-"+opts.After) {
				skipping = false
			}
			continue
		}
		e, err := f.readTrash(ctx, o.Key)
		if err != nil {
			continue // gone since the listing
		}
		if opts.Batch != "" && e.Batch != opts.Batch {
			continue
		}
		if opts.Pass != "" && e.Pass != opts.Pass {
			continue
		}
		if opts.PathPrefix != "" && !underPath(e.Path, opts.PathPrefix) {
			continue
		}
		if live, err := f.itemLive(ctx, e.ItemID, e.Type); err != nil {
			return nil, err
		} else if live {
			continue
		}
		out = append(out, e)
		if opts.Limit > 0 && len(out) == opts.Limit {
			break
		}
	}
	return out, nil
}

func underPath(p, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return p == prefix || strings.HasPrefix(p, prefix+"/") || prefix == ""
}

func (f *Filesystem) readTrash(ctx context.Context, key string) (TrashEntry, error) {
	body, _, err := f.store.Get(ctx, key)
	if err != nil {
		return TrashEntry{}, mapNotFound(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return TrashEntry{}, err
	}
	return decodeTrashEntry(data)
}

// trashEntry finds an entry by its id, with its key.
func (f *Filesystem) trashEntry(ctx context.Context, entryID string) (TrashEntry, string, error) {
	objs, err := f.store.List(ctx, f.trashPrefix())
	if err != nil {
		return TrashEntry{}, "", err
	}
	for _, o := range objs {
		if strings.HasSuffix(o.Key, "-"+entryID) {
			e, err := f.readTrash(ctx, o.Key)
			return e, o.Key, err
		}
	}
	return TrashEntry{}, "", fmt.Errorf("trash entry %s: %w", entryID, ErrNotFound)
}

// RestoreOptions says where a restored item goes: into directory ToDir
// (default: the directory it was deleted from) as ToName (default: its
// old name). Replace moves whatever has that name now to the trash first;
// without it, a taken name is ErrExists.
type RestoreOptions struct {
	ToDir, ToName string
	Replace       bool
}

// Restore brings a deleted item back, with its history. A directory that
// was the start of a recursive delete brings back the whole batch, into
// itself, as it was.
func (f *Filesystem) Restore(ctx context.Context, entryID string, opts RestoreOptions) (TrashEntry, error) {
	if err := f.checkWritable(); err != nil {
		return TrashEntry{}, err
	}
	e, key, err := f.trashEntry(ctx, entryID)
	if err != nil {
		return TrashEntry{}, err
	}
	if live, err := f.itemLive(ctx, e.ItemID, e.Type); err != nil {
		return TrashEntry{}, err
	} else if live {
		f.dropStaleEntry(ctx, e, key)
		return TrashEntry{}, fmt.Errorf("trash entry %s: %s isn't deleted: %w", entryID, e.Name, ErrNotFound)
	}
	dir, name := e.ParentID, e.Name
	if opts.ToDir != "" {
		dir = opts.ToDir
	}
	if opts.ToName != "" {
		name = opts.ToName
	}
	if err := f.checkName(name); err != nil {
		return TrashEntry{}, err
	}
	if _, err := f.statDir(ctx, dir); err != nil {
		return TrashEntry{}, fmt.Errorf("restore into %s: %w", dir, err)
	}
	if existing, ok, err := f.findInTree(ctx, dir, name); err != nil {
		return TrashEntry{}, err
	} else if ok {
		if !opts.Replace {
			return TrashEntry{}, fmt.Errorf("restore as %q: %w", name, ErrExists)
		}
		if existing.Type == TypeDir {
			err = f.Rmdir(ctx, dir, name)
		} else {
			err = f.Unlink(ctx, dir, name)
		}
		if err != nil {
			return TrashEntry{}, err
		}
	}
	if err := f.undelete(ctx, e); err != nil {
		return TrashEntry{}, err
	}
	if err := f.insertEntry(ctx, dir, name, e.ItemID, e.Type); err != nil {
		return TrashEntry{}, err
	}
	if err := f.touchDir(ctx, dir); err != nil {
		return e, err
	}
	// The entry goes with the restore. One left behind would be stale,
	// which nothing acts on, but the restore says so.
	if err := f.store.Delete(ctx, key); err != nil {
		return e, fmt.Errorf("restored %s, its trash entry stays: %w", e.Name, err)
	}
	if err := f.store.Delete(ctx, f.trashByKey(e.ItemID)); err != nil && !objstore.IsNotFound(err) {
		return e, fmt.Errorf("restored %s, its trash pointer stays: %w", e.Name, err)
	}

	// A directory that started a recursive delete brings its batch back,
	// newest first: the reverse of the order the delete removed them,
	// so each directory is back before what was in it.
	if e.Type == TypeDir && e.Batch == e.EntryID {
		rest, err := f.ListTrash(ctx, TrashOptions{Batch: e.Batch})
		if err != nil {
			return e, err
		}
		for _, child := range rest {
			if _, err := f.Restore(ctx, child.EntryID, RestoreOptions{}); err != nil && !errors.Is(err, ErrExists) {
				return e, fmt.Errorf("restore %s from the batch: %w", child.Name, err)
			}
		}
	}
	return e, nil
}

// undelete makes an item's objects current again: the delete left a
// delete marker as each object's newest version, and removing it puts
// the version before it back, so history simply continues. A file that
// was deleted while still open here, and isn't closed yet, has no markers
// yet: its deletion is just called off.
func (f *Filesystem) undelete(ctx context.Context, e TrashEntry) error {
	f.openMu.Lock()
	delete(f.opens.orphans, e.ItemID)
	f.openMu.Unlock()

	keys := []string{e.ItemID, metadataObjectKey(e.ItemID)}
	for _, k := range keys {
		if err := f.store.Undelete(ctx, k); err != nil {
			return err
		}
	}
	attr, err := f.Stat(ctx, e.ItemID, e.Type)
	if err != nil {
		return fmt.Errorf("restore %s: %w", e.Name, err)
	}
	for _, id := range attr.Streams {
		if err := f.store.Undelete(ctx, StreamKey(e.ItemID, id)); err != nil {
			return err
		}
	}
	if e.Type != TypeDir {
		// The last unlink left nlink 0 in .metadata; it has a name again.
		return f.writeMetadata(ctx, e.ItemID, true, func(a *Attr) { a.Nlink = 1 })
	}
	return nil
}
