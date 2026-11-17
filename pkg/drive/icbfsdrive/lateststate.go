package icbfsdrive

import (
	"context"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.LatestStater = (*Drive)(nil)

// LatestState is the date of item's newest state (drive.LatestStater):
// the latest date in its dates table, which is what a change in source
// time is checked against to be late (RecordDates). One read of the
// table (its .version object) once the item is resolved; zero if the
// item has no dated version.
func (d *Drive) LatestState(ctx context.Context, ref drive.ItemRef) (time.Time, error) {
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return time.Time{}, err
	}
	if err := d.check(ctx, n, needRead); err != nil {
		return time.Time{}, err
	}
	dates, err := objstore.ReadDates(ctx, d.fs.Store(), n.key)
	if err != nil {
		return time.Time{}, mapErr(err)
	}
	var latest time.Time
	for _, t := range dates {
		if t.After(latest) {
			latest = t
		}
	}
	return latest, nil
}
