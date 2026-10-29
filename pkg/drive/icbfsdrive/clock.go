package icbfsdrive

import (
	"context"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.Clock = (*Drive)(nil)

// StoreTime is the store's clock now, in whole seconds: the Date header
// of a response from it (objstore.Store.ServerTime), in UTC. It is the
// clock the store stamps versions with, so a time read from it orders
// against those stamps; this program's own clock may not.
func (d *Drive) StoreTime(ctx context.Context) (time.Time, error) {
	t, err := d.fs.Store().ServerTime(ctx)
	if err != nil {
		return time.Time{}, mapErr(err)
	}
	return t.UTC().Truncate(time.Second), nil
}
