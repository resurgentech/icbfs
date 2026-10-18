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
// item's key), in two reads, or ErrNotFound if it isn't in the trash.
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
	return e, nil
}

func (f *Filesystem) trashKey(e TrashEntry) string {
	return fmt.Sprintf("%s%019d-%s", f.trashPrefix(), math.MaxInt64-e.DeletedAt.UnixNano(), e.EntryID)
}

// recordTrash writes a trash entry for an item just deleted from dirKey
// under name. Best effort, like touchDir: the delete itself has happened.
func (f *Filesystem) recordTrash(ctx context.Context, itemID string, typ EntryType, dirKey, name string, size int64, version string, origin []byte) {
	info := deleteInfoFrom(ctx)
	if info.NoTrash {
		return
	}
	id := info.EntryID
	if id == "" {
		var err error
		if id, err = NewUUIDv7(); err != nil {
			return
		}
	}
	e := TrashEntry{
		EntryID: id, ItemID: itemID, Type: typ, Name: name, ParentID: dirKey,
		Path: info.Path, DeletedAt: time.Now().UTC(), DeletedBy: info.By,
		Size: size, Version: version, Batch: info.Batch, Origin: origin, Pass: info.Pass,
	}
	data, err := encodeTrashEntry(e)
	if err != nil {
		return
	}
	key := f.trashKey(e)
	if _, err := f.store.PutIfAbsent(ctx, key, bytes.NewReader(data), nil); err != nil {
		return
	}
	_, _ = f.store.Put(ctx, f.trashByKey(itemID), strings.NewReader(key), nil, "")
}

// TrashOptions narrows ListTrash.
type TrashOptions struct {
	PathPrefix string // only items deleted from under this path (entries without a path are left out)
	Batch      string // only this batch
	Pass       string // only what this sync pass removed
	Limit      int    // at most this many (0: all)
	After      string // continue after this entry id (a previous page's last)
}

// ListTrash lists deleted items, newest first.
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
	f.touchDir(ctx, dir)
	_ = f.store.Delete(ctx, key)
	_ = f.store.Delete(ctx, f.trashByKey(e.ItemID))

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
		return f.writeMetadata(ctx, e.ItemID, e.Type, true, func(a *Attr) { a.Nlink = 1 })
	}
	return nil
}
