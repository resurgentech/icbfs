package icbfsdrive

import (
	"context"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.LeftoverDater = (*Drive)(nil)

// DateLeftovers is drive.LeftoverDater's: what a write in source time
// killed before its dating left unlisted is listed at its own date
// (icbfs Filesystem.DateLeftovers). An item's ID is its key: nothing is
// resolved. The views forget what it dated, and the past's mark moves.
func (d *Drive) DateLeftovers(ctx context.Context, items []drive.Item) (_ drive.LeftoversDated, err error) {
	if d.cfg.ReadOnly {
		return drive.LeftoversDated{}, errDriveReadOnly
	}
	p, err := profile(ctx)
	if err != nil {
		return drive.LeftoversDated{}, err
	}
	if !p.System {
		return drive.LeftoversDated{}, drive.ErrNoAccess
	}
	var in []icbfs.LeftoverItem
	for _, it := range items {
		if it.ID == "" || it.ID == d.rootKey && it.Type != drive.TypeDir {
			continue
		}
		typ := icbfs.TypeFile
		if it.ID == d.rootKey || it.Type == drive.TypeDir {
			typ = icbfs.TypeDir
		}
		in = append(in, icbfs.LeftoverItem{Key: it.ID, Type: typ})
	}
	if len(in) == 0 {
		return drive.LeftoversDated{}, nil
	}
	written := &writtenKeys{}
	wctx := withWrittenKeys(ctx, written)
	got, err := d.fs.DateLeftovers(wctx, in)
	if got.Dated > 0 {
		d.past.changed(ctx)
		keys, prefixes := written.forgettable()
		d.views.forgetKeys(context.WithoutCancel(ctx), keys, prefixes...)
	}
	return drive.LeftoversDated{Dated: got.Dated, Undatable: got.Undatable}, mapErr(err)
}
