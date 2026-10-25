package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrReadOnly is what every write to an AsOf view returns.
var ErrReadOnly = errors.New("read-only: a view of the store as of a past time")

// AsOf is a read-only view of a Store as it was at one moment: Get and
// Head return the newest version of each key whose Time is at or before
// that moment (not found if that was a delete, or if the key didn't exist
// yet), List lists what existed then, and every write fails with
// ErrReadOnly. icbfs relies on the bucket's versioning for this: every
// change to every object is a new version, with the store's time or the
// one declared for it (WithDeclaredTime).
//
// Resolving a key means its history, which comes from a HistoryCache
// shared by views of every time: a view is only a time, and costs
// nothing to make.
type AsOf struct {
	cache *HistoryCache
	store Store
	at    time.Time

	// dates memoizes each item's override table (its .version object as
	// it is now), by base key, for this view.
	dates sync.Map
}

var _ Store = (*AsOf)(nil)

// asOfClockSlack is how far ahead of the store's clock an as-of time
// may be and still be waited for.
const asOfClockSlack = 2 * time.Second

// NewAsOf returns cache's store as it was at at, refusing a time after
// the store's own current time (whose view would still be changing),
// unless it's within asOfClockSlack, which it waits out.
func NewAsOf(ctx context.Context, cache *HistoryCache, at time.Time) (*AsOf, error) {
	store := cache.Store()
	// A time clearly past, by the cache's reckoning of the store's clock,
	// needs no request: a program browsing history makes a view per moment.
	if now, err := cache.StoreNow(ctx); err == nil && at.Before(now.Add(-asOfClockSlack)) {
		return &AsOf{cache: cache, store: store, at: at}, nil
	}
	// The store's clock reads in whole seconds (an HTTP Date), so a time
	// just made — a version name made now — can look up to a second
	// ahead: wait that out rather than refuse it.
	deadline := time.Now().Add(asOfClockSlack)
	for {
		now, err := store.ServerTime(ctx)
		if err != nil {
			return nil, fmt.Errorf("as-of: read the store's clock: %w", err)
		}
		if !at.After(now) {
			break
		}
		if at.Sub(now) > asOfClockSlack || time.Now().After(deadline) {
			return nil, fmt.Errorf("as-of %s is after the object store's current time %s", at.Format(time.RFC3339), now.Format(time.RFC3339))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return &AsOf{cache: cache, store: store, at: at}, nil
}

// At is the moment this view shows.
func (a *AsOf) At() time.Time { return a.at }

// Live is the store this is a view of, as it is now.
func (a *AsOf) Live() Store { return a.store }

// Dates is the override table of the item key belongs to, as its
// .version object has it now (the newest version, whatever a.at: the
// record is the authority on the item's timeline), from the history
// cache; empty for an item with none. Read once per item per view.
func (a *AsOf) Dates(ctx context.Context, key string) (Dates, error) {
	base := BaseKey(key)
	if d, ok := a.dates.Load(base); ok {
		return d.(Dates), nil
	}
	vkey := base + VersionSuffix
	history, err := a.cache.history(ctx, vkey, a.at)
	if err != nil {
		return nil, err
	}
	d := Dates{}
	if len(history) > 0 && !history[0].Deleted {
		body, _, err := a.cache.getVersion(CacheContent(ctx), vkey, history[0].VersionID)
		if err != nil && !IsNotFound(err) {
			return nil, err
		}
		if err == nil {
			data, rerr := io.ReadAll(body)
			body.Close()
			if rerr != nil {
				return nil, rerr
			}
			if d, err = decodeDates(data); err != nil {
				return nil, err
			}
		}
	}
	a.dates.Store(base, d)
	return d, nil
}

// resolve returns the version of key current at a.at, by effective date
// (Dates.Pick).
func (a *AsOf) resolve(ctx context.Context, key string) (Object, bool, error) {
	history, err := a.cache.history(ctx, key, a.at)
	if err != nil {
		return Object{}, false, err
	}
	dates, err := a.Dates(ctx, key)
	if err != nil {
		return Object{}, false, err
	}
	v, ok := dates.Pick(history, a.at)
	return v, ok, nil
}

// notAt is the not-found error (satisfying IsNotFound) for a key that
// didn't exist at a.at.
func (a *AsOf) notAt(key string) error {
	return fmt.Errorf("%s as of %s: %w (it didn't exist then)", key, a.at.Format(time.RFC3339), ErrNotFound)
}

// withVersion calls fn with the version of key current at a.at. If the
// store no longer has the version the cache named (a purge erased it),
// the key is forgotten and resolved again, once.
func (a *AsOf) withVersion(ctx context.Context, key string, fn func(versionID string) error) error {
	for retried := false; ; retried = true {
		v, ok, err := a.resolve(ctx, key)
		if err != nil {
			return err
		}
		if !ok {
			return a.notAt(key)
		}
		err = fn(v.VersionID)
		if retried || !IsNotFound(err) {
			return err
		}
		if err := a.cache.forget(ctx, key); err != nil {
			return err
		}
	}
}

func (a *AsOf) Get(ctx context.Context, key string) (io.ReadCloser, *Object, error) {
	var body io.ReadCloser
	var obj *Object
	err := a.withVersion(ctx, key, func(versionID string) (err error) {
		body, obj, err = a.cache.getVersion(ctx, key, versionID)
		return err
	})
	return body, obj, err
}

func (a *AsOf) Head(ctx context.Context, key string) (*Object, error) {
	var obj *Object
	err := a.withVersion(ctx, key, func(versionID string) (err error) {
		obj, err = a.cache.headVersion(ctx, key, versionID)
		return err
	})
	return obj, err
}

func (a *AsOf) GetVersion(ctx context.Context, key, versionID string) (io.ReadCloser, *Object, error) {
	return a.cache.getVersion(ctx, key, versionID)
}

func (a *AsOf) HeadVersion(ctx context.Context, key, versionID string) (*Object, error) {
	return a.cache.headVersion(ctx, key, versionID)
}

// History is key's history up to a.at, by effective date.
func (a *AsOf) History(ctx context.Context, key string) ([]Object, error) {
	all, err := a.cache.history(ctx, key, a.at)
	if err != nil {
		return nil, err
	}
	dates, err := a.Dates(ctx, key)
	if err != nil {
		return nil, err
	}
	return dates.UpTo(all, a.at), nil
}

// HistoryUnder is the history under prefix up to a.at, by the store's
// dates: a listing of many items doesn't consult each one's .version.
func (a *AsOf) HistoryUnder(ctx context.Context, prefix string) ([]Object, error) {
	all, err := a.cache.historyUnder(ctx, prefix, a.at)
	if err != nil {
		return nil, err
	}
	return Dates{}.UpTo(all, a.at), nil
}

// List lists every key under prefix that existed at a.at, with its size
// then, by the store's dates (a listing of many items doesn't consult each
// one's .version) — every
// version under the prefix has to be listed for that, so
// the first time it's as slow as the prefix's whole history is long.
// After that it's the history cache's, for a view of any time.
func (a *AsOf) List(ctx context.Context, prefix string) ([]Object, error) {
	all, err := a.cache.historyUnder(ctx, prefix, a.at)
	if err != nil {
		return nil, err
	}
	var out []Object
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].Key == all[i].Key {
			j++
		}
		if v, ok := (Dates{}).Pick(all[i:j], a.at); ok {
			out = append(out, Object{Key: v.Key, Size: v.Size})
		}
		i = j
	}
	return out, nil
}

func (a *AsOf) ServerTime(ctx context.Context) (time.Time, error) {
	return a.store.ServerTime(ctx)
}

func (a *AsOf) Put(context.Context, string, io.Reader, map[string]string, string) (*Object, error) {
	return nil, ErrReadOnly
}

func (a *AsOf) PutIfAbsent(context.Context, string, io.Reader, map[string]string) (*Object, error) {
	return nil, ErrReadOnly
}

func (a *AsOf) PutParts(context.Context, string, []Part, map[string]string, string) (*Object, error) {
	return nil, ErrReadOnly
}

// PartLimits are the store's, though an AsOf view writes nothing.
func (a *AsOf) PartLimits() PartLimits { return a.store.PartLimits() }

func (a *AsOf) Delete(context.Context, string) error { return ErrReadOnly }

func (a *AsOf) Undelete(context.Context, string) error { return ErrReadOnly }

func (a *AsOf) Erase(context.Context, string, string) error { return ErrReadOnly }

func (a *AsOf) EraseAll(context.Context, string) error { return ErrReadOnly }
