package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
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

// asOfVersion picks, from one key's history (newest first), the version
// current at at, by the times the history says: ok is false if the key
// didn't exist then. An entry with MetadataUnknown counts as at its
// LastModified, which may be later than a declared time it has.
func asOfVersion(history []Object, at time.Time) (Object, bool) {
	for _, v := range history {
		if !v.Time().After(at) {
			return v, !v.Deleted
		}
	}
	return Object{}, false
}

// resolve returns the version of key current at a.at.
func (a *AsOf) resolve(ctx context.Context, key string) (Object, bool, error) {
	history, err := a.cache.history(ctx, key, a.at)
	if err != nil {
		return Object{}, false, err
	}
	i, err := a.firstAtOrBefore(ctx, history)
	if err != nil || i == len(history) {
		return Object{}, false, err
	}
	return history[i], !history[i].Deleted, nil
}

// firstAtOrBefore is the index of the first entry of history (newest
// first) whose time is at or before a.at, or len(history). A key's times
// only fall going back through its history (icbfs refuses a write
// declared earlier than the version it replaces), so it's a binary
// search, which reads the metadata of only a few entries the listing
// didn't give it for.
func (a *AsOf) firstAtOrBefore(ctx context.Context, history []Object) (int, error) {
	var ferr error
	i := sort.Search(len(history), func(i int) bool {
		t, err := a.timeOf(ctx, &history[i])
		if err != nil && ferr == nil {
			ferr = err
		}
		return !t.After(a.at)
	})
	return i, ferr
}

// timeOf is o's time, as far as deciding whether it's after a.at needs:
// an entry the listing gave no metadata for is read (HeadVersion, kept
// by the cache) only if its LastModified is after a.at, since a declared
// time is never later than that.
func (a *AsOf) timeOf(ctx context.Context, o *Object) (time.Time, error) {
	if !o.MetadataUnknown || o.Deleted || !o.LastModified.After(a.at) {
		return o.Time(), nil
	}
	head, err := a.cache.headVersion(ctx, o.Key, o.VersionID)
	if IsNotFound(err) {
		return o.LastModified, nil // erased since: a purge's, which resolve's caller retries
	}
	if err != nil {
		return o.LastModified, err
	}
	o.Metadata, o.MetadataUnknown = head.Metadata, false
	return o.Time(), nil
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

// History is key's history up to a.at.
func (a *AsOf) History(ctx context.Context, key string) ([]Object, error) {
	all, err := a.cache.history(ctx, key, a.at)
	if err != nil {
		return nil, err
	}
	i, err := a.firstAtOrBefore(ctx, all)
	if err != nil {
		return nil, err
	}
	return all[i:], nil
}

// HistoryUnder is the history under prefix up to a.at, by the times the
// listing gives (see asOfVersion).
func (a *AsOf) HistoryUnder(ctx context.Context, prefix string) ([]Object, error) {
	all, err := a.cache.historyUnder(ctx, prefix, a.at)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, v := range all {
		if !v.Time().After(a.at) {
			out = append(out, v)
		}
	}
	return out, nil
}

// List lists every key under prefix that existed at a.at, with its size
// then, by the times the listing gives (see asOfVersion: on S3, a key
// written under a declared time counts from when it was stored) — every
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
		if v, ok := asOfVersion(all[i:j], a.at); ok {
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
