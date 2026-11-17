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
	d        *Drive
	mu       sync.Mutex
	views    map[time.Time]*view
	history  *objstore.HistoryCache
	ownHist  bool      // history is the temporary one, for closeAll to close
	pastSeen time.Time // when the filesystem's past-changed mark was last looked at
	// forget: this drive changed the past, so views forget what they kept
	// on next use, without waiting for the mark (sent at most once a
	// second: pastMarker).
	forget bool
	// seenMark is the past-changed mark these views last saw (seen: at
	// all). The history cache's own note is shared by every drive and
	// program over it, and says whether the cache is up to date; views
	// hold more (each item's dates), and start again when the mark moves
	// past the one they saw, whoever's it is but this drive's own.
	seenMark string
	seen     bool
}

// pastCheck is how often views look for a change to the past (a purge, a
// change made under a source time) since the last look: until then a
// view can show the past as it was before.
var pastCheck = 30 * time.Second // a variable for the tests

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
	history, err := c.cacheLocked(ctx)
	if err != nil {
		return nil, err
	}
	if c.forget {
		if err := icbfs.ForgetPast(ctx, c.d.fs, history); err != nil {
			return nil, err
		}
		clear(c.views)
		c.forget = false
	}
	if time.Since(c.pastSeen) >= pastCheck {
		if _, err := c.checkPastLocked(ctx, history); err != nil {
			return nil, err
		}
	}
	if v, ok := c.views[t]; ok {
		v.used = time.Now()
		return v.fsys, nil
	}
	asOf, err := objstore.NewAsOf(ctx, history, t)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", drive.ErrInvalid, err)
	}
	fsys := icbfs.New(asOf, c.d.cfg.Filesystem)
	if err := fsys.BootstrapViewOf(ctx, c.d.fs); err != nil {
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

// cache is the history cache views read through: the drive's
// (Config.History), or a temporary one opened on first use.
func (c *viewCache) cache(ctx context.Context) (*objstore.HistoryCache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cacheLocked(ctx)
}

func (c *viewCache) cacheLocked(ctx context.Context) (*objstore.HistoryCache, error) {
	if c.history == nil {
		if c.history = c.d.cfg.History; c.history == nil {
			h, err := objstore.OpenTempHistoryCache(ctx, c.d.cfg.Store, c.d.cfg.CacheDir)
			if err != nil {
				return nil, err
			}
			c.history, c.ownHist = h, true
		}
	}
	return c.history, nil
}

// CheckPast makes the drive's views forget what they kept of the past if
// it has changed since they last looked (a purge, a change made under a
// source time: icbfs.MarkPastChanged), and reports whether it had. Views
// look by themselves every 30 seconds; a caller keeping things of its own
// from them (a history app's listings) looks when it needs to know.
func (d *Drive) CheckPast(ctx context.Context) (bool, error) {
	c := &d.views
	c.mu.Lock()
	defer c.mu.Unlock()
	history, err := c.cacheLocked(ctx)
	if err != nil {
		return false, err
	}
	return c.checkPastLocked(ctx, history)
}

func (c *viewCache) checkPastLocked(ctx context.Context, history *objstore.HistoryCache) (bool, error) {
	mark, forgot, err := icbfs.CheckPastMark(ctx, c.d.fs, history)
	if err != nil {
		return false, err
	}
	c.pastSeen = time.Now()
	moved := !c.seen || mark != c.seenMark
	if forgot || moved {
		clear(c.views)
	}
	c.seenMark, c.seen = mark, true
	return forgot || moved, nil
}

// fresh is the history cache, brought up to date for a reader that goes
// round views (a settle): what this drive changed of the past forgotten
// (lookAgain), and the past-changed mark looked at now.
func (c *viewCache) fresh(ctx context.Context) (*objstore.HistoryCache, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	history, err := c.cacheLocked(ctx)
	if err != nil {
		return nil, err
	}
	if c.forget {
		if err := icbfs.ForgetPast(ctx, c.d.fs, history); err != nil {
			return nil, err
		}
		clear(c.views)
		c.forget = false
	}
	if _, err := c.checkPastLocked(ctx, history); err != nil {
		return nil, err
	}
	return history, nil
}

// forgetKeys makes the history cache, if one is open, forget keys now, and
// views start again (their memo of each item's dates goes with them):
// this drive has just written them. The rest of the cache stays. A
// forget that fails falls back on forgetting everything (lookAgain).
func (c *viewCache) forgetKeys(ctx context.Context, keys []string, prefixes ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.views)
	if c.history == nil {
		return
	}
	err := c.history.ForgetKeys(ctx, keys...)
	for _, p := range prefixes {
		if err == nil {
			err = c.history.ForgetUnder(ctx, p)
		}
	}
	if err != nil {
		c.forget = true
	}
}

// markPast sends the drive's past-changed mark (icbfs.MarkPastChanged)
// with the drive's own history cache, if one is open, told that this
// mark is one it has seen (icbfs.MarkPastChangedSeen): the drive's own
// writes already forgot what they changed (forgetKeys), so its cache
// needn't forget everything when its views next look for a mark.
func (c *viewCache) markPast(ctx context.Context) error {
	c.mu.Lock()
	history := c.history
	c.mu.Unlock()
	if history == nil {
		return c.d.fs.MarkPastChanged(ctx)
	}
	before, after, err := icbfs.MarkPastChangedSeen(ctx, c.d.fs, history)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.seen && c.seenMark == before {
		c.seenMark = after // its own: what it changed, its views forgot
	}
	c.mu.Unlock()
	return nil
}

// lookAgain makes the next view forget what views kept of the past:
// this drive has just changed it.
func (c *viewCache) lookAgain() {
	c.mu.Lock()
	c.forget = true
	c.mu.Unlock()
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
