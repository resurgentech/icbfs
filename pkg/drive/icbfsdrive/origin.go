package icbfsdrive

import (
	"context"
	"errors"
	"fmt"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.OriginFinder = (*Drive)(nil)

// recordOrigin indexes the item key, at drive path p, as the copy of o's
// source item (icbfs.OriginRecord), so FindByOrigin finds it in a lookup.
// The newest claim wins: a second copy of one source item (drive.Copy
// twice, say) takes the record over. Each of o's aliases gets a record too,
// marked an alias, naming key; but an alias never takes over a record that
// names another item (one given an item of its own since). prev is the
// item's origin before this one, if known: its records that o no longer
// claims, and that still name key, are deleted (an alias dropped, or the
// origin handed to an alias).
func (d *Drive) recordOrigin(ctx context.Context, key, p string, o drive.Origin, prev *drive.Origin) error {
	if o.Drive == "" || o.ID == "" {
		return nil
	}
	fsPath := d.fsPath(p)
	if err := d.putOrigin(ctx, icbfs.OriginRecord{Drive: o.Drive, ID: o.ID, ItemID: key, Version: o.Version, Path: fsPath}); err != nil {
		return err
	}
	claimed := map[string]bool{o.ID: true}
	for _, a := range o.Aliases {
		if a.ID == "" || claimed[a.ID] {
			continue
		}
		claimed[a.ID] = true
		rec := icbfs.OriginRecord{Drive: o.Drive, ID: a.ID, ItemID: key, Version: a.Version, Path: fsPath, Alias: true}
		if err := d.putOrigin(ctx, rec); err != nil {
			return err
		}
	}
	if prev == nil || prev.Drive != o.Drive {
		return nil
	}
	ids := []string{prev.ID}
	for _, a := range prev.Aliases {
		ids = append(ids, a.ID)
	}
	for _, id := range ids {
		if id == "" || claimed[id] {
			continue
		}
		cur, _, err := d.fs.OriginRecord(ctx, o.Drive, id)
		if errors.Is(err, icbfs.ErrNotFound) || err == nil && cur.ItemID != key {
			continue // gone, or another item's now
		}
		if err != nil {
			return mapErr(err)
		}
		if err := d.fs.DeleteOriginRecord(ctx, o.Drive, id); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// putOrigin writes rec unless it's there already. An alias's record is
// written only over none, or over one naming the same item.
func (d *Drive) putOrigin(ctx context.Context, rec icbfs.OriginRecord) error {
	for attempt := 0; attempt < 5; attempt++ {
		cur, etag, err := d.fs.OriginRecord(ctx, rec.Drive, rec.ID)
		switch {
		case errors.Is(err, icbfs.ErrNotFound):
			etag = ""
		case err != nil:
			return mapErr(err)
		case cur == rec:
			return nil
		case rec.Alias && cur.ItemID != rec.ItemID:
			return nil // the source item has an item of its own
		}
		err = d.fs.PutOriginRecord(ctx, rec, etag)
		if !errors.Is(err, icbfs.ErrExists) {
			return mapErr(err)
		}
		// Someone else wrote it meanwhile: read it again and go over it.
	}
	return drive.ErrBusy
}

// FindByOrigin finds the item copied from a source item: live, at the
// path its record last saw it (one check) or wherever it is now (a search
// of the drive, after a move made where the record didn't see it, as
// through a mount); or trashed, by its trash entry. It's what makes
// drive.Mirror's lookups cheap on icbfs. An alias's id (drive.Origin's
// Aliases) finds the live item it's folded into, with Item.Alias set to
// that id; one whose item is trashed, or no longer lists it, is
// ErrNotFound.
func (d *Drive) FindByOrigin(ctx context.Context, originDrive, originID string) (drive.Item, *drive.TrashEntry, error) {
	rec, _, err := d.fs.OriginRecord(ctx, originDrive, originID)
	if err != nil {
		if errors.Is(err, icbfs.ErrNotFound) {
			return drive.Item{}, nil, fmt.Errorf("source item %s: %w", originID, drive.ErrNotFound)
		}
		return drive.Item{}, nil, mapErr(err)
	}
	found := func(it drive.Item) (drive.Item, *drive.TrashEntry, error) {
		if !rec.Alias {
			return it, nil, nil
		}
		if it.Origin == nil || it.Origin.Drive != originDrive || !hasAlias(it.Origin, originID) {
			return drive.Item{}, nil, fmt.Errorf("source item %s (no longer an alias): %w", originID, drive.ErrNotFound)
		}
		it.Alias = originID
		return it, nil, nil
	}
	recPath, recOK := d.drivePath(rec.Path)
	if recOK {
		if n, err := d.resolvePath(ctx, d.fs, recPath); err == nil && n.key == rec.ItemID {
			if err := d.check(ctx, n, needRead); err != nil {
				return drive.Item{}, nil, err
			}
			return found(d.item(ctx, n, true))
		}
	}
	// Where this drive last saw the item, if it moved it since: no search,
	// and no look in the trash first.
	if p, ok := d.idPaths.get(rec.ItemID); ok && !(recOK && p == recPath) {
		if n, err := d.resolvePath(ctx, d.fs, p); err == nil && n.key == rec.ItemID {
			if err := d.check(ctx, n, needRead); err != nil {
				return drive.Item{}, nil, err
			}
			return found(d.item(ctx, n, true))
		}
	}
	if e, err := d.fs.TrashedItem(ctx, rec.ItemID); err == nil {
		if rec.Alias {
			return drive.Item{}, nil, fmt.Errorf("source item %s (an alias of a deleted item): %w", originID, drive.ErrNotFound)
		}
		if te, ok := d.toTrash(e); ok {
			return drive.Item{}, &te, nil
		}
	} else if !errors.Is(err, icbfs.ErrNotFound) {
		return drive.Item{}, nil, mapErr(err)
	}
	n, err := d.resolve(ctx, d.fs, drive.ByID(rec.ItemID))
	if err != nil {
		// Neither live nor in the trash: purged, or outside this drive.
		return drive.Item{}, nil, fmt.Errorf("source item %s: %w", originID, drive.ErrNotFound)
	}
	if err := d.check(ctx, n, needRead); err != nil {
		return drive.Item{}, nil, err
	}
	rec.Path = d.fsPath(n.path)
	if _, etag, err := d.fs.OriginRecord(ctx, originDrive, originID); err == nil {
		_ = d.fs.PutOriginRecord(ctx, rec, etag) // best effort: the next lookup is quicker
	}
	return found(d.item(ctx, n, true))
}

// hasAlias reports whether id is one of o's aliases.
func hasAlias(o *drive.Origin, id string) bool {
	for _, a := range o.Aliases {
		if a.ID == id {
			return true
		}
	}
	return false
}
