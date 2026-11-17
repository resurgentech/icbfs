package icbfsdrive

import (
	"context"
	"fmt"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// The branch: a deferring drive whose writes collect their late changes.
func rvCostConfig() Config { return Config{DeferLateFolders: true} }

func rvCostCtx(at time.Time) (context.Context, func() string) {
	late := &drive.LateChanges{}
	return drive.WithSourceTime(drive.WithLateChanges(system(), late), at), func() string {
		rows := late.Take()
		if len(rows) == 0 {
			return "no rows"
		}
		return fmt.Sprintf("%d rows, until %s", len(rows), rows[0].Until.Format(time.RFC3339Nano))
	}
}
