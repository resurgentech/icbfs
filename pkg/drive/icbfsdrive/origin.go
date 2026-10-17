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
// twice, say) takes the record over.
func (d *Drive) recordOrigin(ctx context.Context, key, p string, o drive.Origin) error {
	if o.Drive == "" || o.ID == "" {
		return nil
	}
	rec := icbfs.OriginRecord{Drive: o.Drive, ID: o.ID, ItemID: key, Version: o.Version, Path: d.fsPath(p)}
	for attempt := 0; attempt < 5; attempt++ {
		cur, etag, err := d.fs.OriginRecord(ctx, o.Drive, o.ID)
		switch {
		case errors.Is(err, icbfs.ErrNotFound):
			etag = ""
		case err != nil:
			return mapErr(err)
		case cur == rec:
			return nil
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
// drive.Mirror's lookups cheap on icbfs.
func (d *Drive) FindByOrigin(ctx context.Context, originDrive, originID string) (drive.Item, *drive.TrashEntry, error) {
	rec, _, err := d.fs.OriginRecord(ctx, originDrive, originID)
	if err != nil {
		if errors.Is(err, icbfs.ErrNotFound) {
			return drive.Item{}, nil, fmt.Errorf("source item %s: %w", originID, drive.ErrNotFound)
		}
		return drive.Item{}, nil, mapErr(err)
	}
	if p, ok := d.drivePath(rec.Path); ok {
		if n, err := d.resolvePath(ctx, d.fs, p); err == nil && n.key == rec.ItemID {
			if err := d.check(ctx, n, needRead); err != nil {
				return drive.Item{}, nil, err
			}
			return d.item(ctx, n, true), nil, nil
		}
	}
	if e, err := d.fs.TrashedItem(ctx, rec.ItemID); err == nil {
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
	return d.item(ctx, n, true), nil, nil
}
