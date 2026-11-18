package icbfsdrive

import (
	"context"
	"errors"
	"fmt"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var (
	_ drive.OriginFinder       = (*Drive)(nil)
	_ drive.HintedOriginFinder = (*Drive)(nil)
)

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
	return d.findByOrigin(ctx, originDrive, originID, "")
}

// FindByOriginHint is FindByOrigin told where the caller believes the
// item is (drive.HintedOriginFinder): hint, a drive path, a daemon's own
// record of where it put the item. The hint is tried first, once (a
// resolve of the path), and is the answer only if the item there is the
// one the origin record names, the item FindByOrigin finds; then an
// origin's record (not an alias's) is corrected to name it there (best
// effort), so the next lookup needs no hint. A hint that misses (stale, wrong, another item's path)
// costs that resolve and the lookup goes on as FindByOrigin's: the same
// answers, whatever the hint. After a folder's move, which leaves the
// records of the items under it naming their old paths, in a process
// that doesn't remember where it put them (a daemon's start), it is what
// spares each of them a walk of the whole tree (T82-13).
func (d *Drive) FindByOriginHint(ctx context.Context, originDrive, originID, hint string) (drive.Item, *drive.TrashEntry, error) {
	return d.findByOrigin(ctx, originDrive, originID, hint)
}

// findByOrigin is FindByOrigin, trying hint ("": none) first.
func (d *Drive) findByOrigin(ctx context.Context, originDrive, originID, hint string) (drive.Item, *drive.TrashEntry, error) {
	rec, recTag, err := d.fs.OriginRecord(ctx, originDrive, originID)
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
	if hint != "" && !(recOK && cleanPath(hint) == cleanPath(recPath)) {
		if n, err := d.resolvePath(ctx, d.fs, hint); err == nil && n.key == rec.ItemID {
			if err := d.check(ctx, n, needRead); err != nil {
				return drive.Item{}, nil, err
			}
			it, te, err := found(d.item(ctx, n, true))
			if err == nil && !rec.Alias {
				// Best effort: the next lookup needs no hint. Only an
				// origin's record: an alias's is the mirror's to rewrite as
				// the item's aliases change hands (a fold, an unfold, a
				// hand-over), and one written back here from an earlier
				// read was measured to turn a hand-over (Clio's
				// TestFoldSyncPathsAndRetryKeepTheFold, 6 of 12 runs red).
				rec.Path = d.fsPath(n.path)
				_ = d.fs.PutOriginRecord(ctx, rec, recTag)
			}
			return it, te, err
		}
	}
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
	if errors.Is(err, drive.ErrNotSearched) {
		return drive.Item{}, nil, fmt.Errorf("source item %s: %w", originID, err) // not searched to the end: not gone
	}
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
