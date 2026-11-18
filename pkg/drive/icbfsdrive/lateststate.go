package icbfsdrive

import (
	"context"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.StateSpanner = (*Drive)(nil)

// LatestState is the date of item's newest state (drive.LatestStater):
// the latest date in its dates table, which is what a change in source
// time is checked against to be late (RecordDates). One read of the
// table (its .version object) once the item is resolved; zero if the
// item has no dated version.
func (d *Drive) LatestState(ctx context.Context, ref drive.ItemRef) (time.Time, error) {
	_, latest, err := d.StateSpan(ctx, ref)
	return latest, err
}

// StateSpan is the dates of item's first and newest states
// (drive.StateSpanner): the earliest and the latest date in its dates
// table, from the one read LatestState makes; zero if the item has no
// dated version.
func (d *Drive) StateSpan(ctx context.Context, ref drive.ItemRef) (first, latest time.Time, err error) {
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := d.check(ctx, n, needRead); err != nil {
		return time.Time{}, time.Time{}, err
	}
	dates, err := objstore.ReadDates(ctx, d.fs.Store(), n.key)
	if err != nil {
		return time.Time{}, time.Time{}, mapErr(err)
	}
	for _, t := range dates {
		if t.After(latest) {
			latest = t
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	return first, latest, nil
}
