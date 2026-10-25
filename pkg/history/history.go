// Package history is a filesystem's past, for a program that browses it:
// a directory listed at any moment, a file's versions, names searched in
// the tree and the trash, and the span of moments there are. It reads
// through the shared history cache (the one as-of mounts use), so each
// object's history and each version's metadata are fetched once, and a
// directory listed at some moments is listed at them again with no store
// requests, in later runs too.
//
// It acts as the profile in each call's context (drive.WithProfile), as a
// drive does: the caller says whose view it is.
package history

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/storeflags"
	"github.com/resurgentech/icbfs/pkg/drive"
	"github.com/resurgentech/icbfs/pkg/drive/icbfsdrive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// Config says which filesystem, and which directory of it ("" or "/"
// for all of it), as a drive does.
type Config struct {
	Store      pkgicbfs.StoreConfig `json:"store"`
	Filesystem string               `json:"filesystem"`
	Root       string               `json:"root,omitempty"`
	// CacheDir is the history caches' directory ("" for the default:
	// icbfs/history in the user's cache directory, shared with as-of
	// mounts), and TTL how long they keep what they fetched (0 for 30
	// days).
	CacheDir string        `json:"cacheDir,omitempty"`
	TTL      time.Duration `json:"ttl,omitempty"`
}

// pastCheck is how often a history looks for a change to the past (a
// purge, a mirror writing what happened before) since the last look:
// until then the past can show as it was.
const pastCheck = 30 * time.Second

// openStore opens a store (a var, for tests to count its requests).
var openStore = pkgicbfs.Open

// History is one filesystem's history, open.
type History struct {
	cfg     Config
	hist    *objstore.HistoryCache
	drive   *icbfsdrive.Drive
	live    *icbfs.Filesystem
	created time.Time

	mu          sync.Mutex
	pastSeen    time.Time
	pastChanges uint64
}

// Open opens cfg's filesystem's history.
func Open(ctx context.Context, cfg Config) (*History, error) {
	if cfg.CacheDir == "" {
		cfg.CacheDir = objstore.DefaultHistoryCacheDir()
	}
	if cfg.TTL == 0 {
		cfg.TTL = objstore.DefaultHistoryTTL
	}
	if err := cfg.Store.Check(); err != nil {
		return nil, err
	}
	store, err := openStore(cfg.Store)
	if err != nil {
		return nil, err
	}
	hist, err := (&storeflags.History{Dir: cfg.CacheDir, TTL: cfg.TTL}).Open(ctx, store, &cfg.Store)
	if err != nil {
		return nil, err
	}
	d, err := icbfsdrive.New(ctx, icbfsdrive.Config{Store: store, Filesystem: cfg.Filesystem, Root: cfg.Root,
		ID: "history:" + cfg.Filesystem + cfg.Root, History: hist})
	if err != nil {
		hist.Close()
		return nil, err
	}
	h := &History{cfg: cfg, hist: hist, drive: d, live: d.Filesystem()}
	if h.created, err = h.creation(ctx); err != nil {
		h.Close()
		return nil, err
	}
	if _, err := h.CheckPast(ctx); err != nil {
		h.Close()
		return nil, err
	}
	// Measure the store's clock now, not in the first listing.
	if _, err := hist.StoreNow(ctx); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Close closes the history.
func (h *History) Close() error {
	h.drive.Close()
	return h.hist.Close()
}

// creation is when the filesystem was made: its master block entry says,
// or, for one made before that was recorded, its root's first version.
func (h *History) creation(ctx context.Context) (time.Time, error) {
	infos, err := icbfs.ListFilesystems(ctx, h.live.Store())
	if err != nil {
		return time.Time{}, err
	}
	for _, info := range infos {
		if info.Name == h.cfg.Filesystem && !info.Created.IsZero() {
			return info.Created, nil
		}
	}
	versions, err := h.live.Store().History(ctx, h.live.RootKey())
	if err != nil || len(versions) == 0 {
		return time.Time{}, fmt.Errorf("when filesystem %q was made: %v", h.cfg.Filesystem, err)
	}
	return versions[len(versions)-1].Time(), nil
}

// Span is the moments there are to see: from the filesystem's creation
// to now, by the store's clock, which is what its times are in (this
// machine's could be off).
func (h *History) Span(ctx context.Context) (from, to time.Time, err error) {
	now, err := h.hist.StoreNow(ctx)
	return h.created, now, err
}

// CheckPast drops from the cache what it kept of the past if the past
// has changed since the last look (at most every 30 seconds): a purge
// erased something, or a mirror wrote what happened earlier. It reports
// whether it dropped anything: listings kept from before may then be
// wrong. List calls it.
func (h *History) CheckPast(ctx context.Context) (bool, error) {
	h.mu.Lock()
	due := time.Since(h.pastSeen) >= pastCheck
	h.mu.Unlock()
	if !due {
		return false, nil
	}
	forgot, err := h.drive.CheckPast(ctx)
	if err != nil {
		return false, err
	}
	h.mu.Lock()
	h.pastSeen = time.Now()
	if forgot {
		h.pastChanges++
	}
	h.mu.Unlock()
	return forgot, nil
}

// PastChanges counts the changes to the past this history has dropped
// from the cache: a caller keeping listings of its own drops them when
// it moves.
func (h *History) PastChanges() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pastChanges
}

// List lists directory dir (drive-relative, "/" for the top) as it was at
// at (now, if at is zero), as ctx's profile sees it.
func (h *History) List(ctx context.Context, dir string, at time.Time) ([]drive.Item, error) {
	if _, err := h.CheckPast(ctx); err != nil {
		return nil, err
	}
	opts := drive.ListOptions{At: drive.At{Time: at}}
	var items []drive.Item
	for {
		page, err := h.drive.List(ctx, drive.ByPath(dir), opts)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Next == "" {
			break
		}
		opts.Cursor = page.Next
	}
	return items, nil
}

// Versions are a file's versions, newest first: by its id (an item's ID,
// even one in the trash), or else the one at its path now.
func (h *History) Versions(ctx context.Context, item drive.ItemRef) ([]drive.Version, error) {
	return h.drive.Versions(ctx, item)
}

// ErrNotFound is what List, Versions and a search hit's moment answer for
// what isn't there (then): drive.ErrNotFound.
var ErrNotFound = drive.ErrNotFound

// IsNotFound reports whether err is ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
