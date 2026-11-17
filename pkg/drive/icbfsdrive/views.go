package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"weak"

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
	// mems are the views' id → path memories, by time (T82-15): kept
	// apart from views, so a view made again for the same time (after
	// it was dropped, or the past changed) still knows where it found
	// items; a remembered path is checked against the id on use.
	mems map[time.Time]*viewMemory
	// moments are the moment of every view this cache made that is still
	// in use, by its filesystem, weakly: a view dropped while a lookup
	// holds it is still known by its moment (ME-5), and gone once nothing
	// holds it.
	moments map[weak.Pointer[icbfs.Filesystem]]time.Time
}

// viewMemory is one time's id → path memory.
type viewMemory struct {
	paths idPathCache
	used  time.Time
}

// maxViewMemories is how many times' memories are kept: twice the views,
// the least recently used dropped first. Each is an idPathCache.
const maxViewMemories = 2 * maxViews

// pastCheck is how often views look for a change to the past (a purge, a
// change made under a source time) since the last look: until then a
// view can show the past as it was before.
const pastCheck = 30 * time.Second

type view struct {
	fsys *icbfs.Filesystem
	used time.Time
	mem  *viewMemory
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
		c.forgetLocked()
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
	c.views[t] = &view{fsys: fsys, used: time.Now(), mem: c.memLocked(t)}
	c.noteMomentLocked(fsys, t)
	return fsys, nil
}

// memLocked is time t's memory, made if there is none.
func (c *viewCache) memLocked(t time.Time) *viewMemory {
	if m, ok := c.mems[t]; ok {
		return m
	}
	if c.mems == nil {
		c.mems = map[time.Time]*viewMemory{}
	}
	for len(c.mems) >= maxViewMemories {
		var oldest time.Time
		var oldestUse time.Time
		for k, m := range c.mems {
			if oldestUse.IsZero() || m.used.Before(oldestUse) {
				oldest, oldestUse = k, m.used
			}
		}
		delete(c.mems, oldest)
	}
	m := &viewMemory{used: time.Now()}
	c.mems[t] = m
	return m
}

// noteMomentLocked records fsys's moment, and lets go of the filesystems
// nothing holds any more.
func (c *viewCache) noteMomentLocked(fsys *icbfs.Filesystem, t time.Time) {
	if c.moments == nil {
		c.moments = map[weak.Pointer[icbfs.Filesystem]]time.Time{}
	}
	for k := range c.moments {
		if k.Value() == nil {
			delete(c.moments, k)
		}
	}
	c.moments[weak.Make(fsys)] = t
}

// memoryOf is the id → path memory of fsys, a view this cache made, and
// its moment: the view's, or, for a view dropped since (evicted, or the
// past changed) that a lookup still holds, its moment's, made again if it
// was let go (ME-5). ok is false for a filesystem this cache never made.
func (c *viewCache) memoryOf(fsys *icbfs.Filesystem) (mem *idPathCache, at time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t, v := range c.views {
		if v.fsys == fsys {
			v.mem.used = time.Now()
			return &v.mem.paths, t, true
		}
	}
	t, ok := c.moments[weak.Make(fsys)]
	if !ok {
		return nil, time.Time{}, false
	}
	m := c.memLocked(t)
	m.used = time.Now()
	return &m.paths, t, true
}

// forgetLocked drops the views, the past having changed, and the
// searches in them that stopped; the memories stay (their paths are
// checked on use).
func (c *viewCache) forgetLocked() {
	clear(c.views)
	c.d.stopped.dropPast()
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
	forgot, err := icbfs.ForgetChangedPast(ctx, c.d.fs, history)
	if err != nil {
		return false, err
	}
	c.pastSeen = time.Now()
	if forgot {
		c.forgetLocked()
	}
	return forgot, nil
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
