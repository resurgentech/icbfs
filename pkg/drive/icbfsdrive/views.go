package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// viewCache keeps a few read-only as-of filesystems, by time, over one
// history cache: the drive's (Config.History), or a temporary one opened
// on first use.
type viewCache struct {
	d       *Drive
	mu      sync.Mutex
	views   map[time.Time]*view
	history *objstore.HistoryCache
	ownHist bool // history is the temporary one, for closeAll to close
}

type view struct {
	fsys *icbfs.Filesystem
	used time.Time
}

const maxViews = 8

// at resolves an At to the filesystem to read: the live one for now, a
// read-only one as of a time or a named version's time.
func (d *Drive) at(ctx context.Context, at drive.At) (*icbfs.Filesystem, error) {
	t := at.Time
	if at.Name != "" {
		nv, err := d.lookupName(ctx, at.Name)
		if err != nil {
			return nil, err
		}
		t = nv.Time
	}
	if t.IsZero() {
		return d.fs, nil
	}
	return d.views.get(ctx, t)
}

func (c *viewCache) get(ctx context.Context, t time.Time) (*icbfs.Filesystem, error) {
	t = t.UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.views[t]; ok {
		v.used = time.Now()
		return v.fsys, nil
	}
	if c.history == nil {
		if c.history = c.d.cfg.History; c.history == nil {
			h, err := objstore.OpenTempHistoryCache(ctx, c.d.cfg.Store, c.d.cfg.CacheDir)
			if err != nil {
				return nil, err
			}
			c.history, c.ownHist = h, true
		}
	}
	asOf, err := objstore.NewAsOf(ctx, c.history, t)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", drive.ErrInvalid, err)
	}
	fsys := icbfs.New(asOf, c.d.cfg.Filesystem)
	if err := fsys.BootstrapReadOnly(ctx); err != nil {
		if errors.Is(err, icbfs.ErrNotFound) {
			return nil, fmt.Errorf("the filesystem as of %s: %w", t.Format(time.RFC3339), drive.ErrNotFound)
		}
		return nil, err
	}
	if c.views == nil {
		c.views = map[time.Time]*view{}
	}
	if len(c.views) >= maxViews {
		var oldest time.Time
		var oldestUse time.Time
		for k, v := range c.views {
			if oldestUse.IsZero() || v.used.Before(oldestUse) {
				oldest, oldestUse = k, v.used
			}
		}
		delete(c.views, oldest)
	}
	c.views[t] = &view{fsys: fsys, used: time.Now()}
	return fsys, nil
}

func (c *viewCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.views)
	if c.ownHist {
		c.history.Close()
	}
	c.history, c.ownHist = nil, false
}
