package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// A change made under objstore.WithDeclaredTime happened at that time, as
// far as the filesystem's history goes: every version it writes says so,
// a new item's key (and so its creation time) is minted at it, a
// directory's mtime becomes it, and a trash entry is deleted at it. Views
// of the past go by those times. ctime stays icbfs's own clock.

// ErrOutOfOrder is a change declared earlier than the version of an
// object it would replace: history is written in order, and fixing it
// afterwards is the caller's (a folder's, with ReplaceDirHistory).
var ErrOutOfOrder = errors.New("declared time is before the current version's")

// CheckOrder fails with ErrOutOfOrder if ctx declares a time before
// cur's: for a write of cur's key made outside this package.
func CheckOrder(ctx context.Context, cur *objstore.Object) error { return checkOrder(ctx, cur) }

// checkOrder fails with ErrOutOfOrder if ctx declares a time before cur's.
func checkOrder(ctx context.Context, cur *objstore.Object) error {
	t, ok := objstore.DeclaredTime(ctx)
	if !ok || cur == nil || !cur.Time().After(t) {
		return nil
	}
	return fmt.Errorf("%s at %s, already at %s: %w", cur.Key, t.Format(time.RFC3339Nano), cur.Time().Format(time.RFC3339Nano), ErrOutOfOrder)
}

// changeTime is when a change made under ctx happened: its declared time,
// or now.
func changeTime(ctx context.Context) time.Time {
	if t, ok := objstore.DeclaredTime(ctx); ok {
		return t
	}
	return time.Now()
}

// newUUIDv7At is a new UUIDv7 whose time is t's millisecond.
func newUUIDv7At(t time.Time) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	ms := t.UnixMilli()
	for i := 5; i >= 0; i-- {
		id[i] = byte(ms)
		ms >>= 8
	}
	return id.String(), nil
}

// pastKey is an empty object written again whenever this filesystem's
// past changes: a purge erasing, or a change declared at an earlier time.
// History caches, which take the past as fixed, drop what they kept of
// the filesystem when it moves (ForgetChangedPast).
func (f *Filesystem) pastKey() string { return f.id + "-past" }

// MarkPastChanged tells history caches that this filesystem's past has
// changed (see pastKey). The library calls it after a purge; a caller
// that writes under a declared time calls it after doing so.
func (f *Filesystem) MarkPastChanged(ctx context.Context) error {
	// Not under the declared time: the mark is news now.
	_, err := f.store.Put(context.WithoutCancel(undeclared(ctx)), f.pastKey(), bytes.NewReader(nil), nil, "")
	return err
}

// undeclared is ctx without a declared time.
func undeclared(ctx context.Context) context.Context {
	if _, ok := objstore.DeclaredTime(ctx); !ok {
		return ctx
	}
	return objstore.WithoutDeclaredTime(ctx)
}

// ForgetChangedPast makes cache drop everything it kept of live's
// filesystem if its past has changed (MarkPastChanged) since the cache
// last looked, and reports whether it did. live is the filesystem
// attached to the live store, not a past view.
func ForgetChangedPast(ctx context.Context, live *Filesystem, cache *objstore.HistoryCache) (bool, error) {
	mark := ""
	obj, err := live.store.Head(ctx, live.pastKey())
	switch {
	case err == nil:
		mark = obj.VersionID
	case !objstore.IsNotFound(err):
		return false, err
	}
	note := "past:" + live.id
	seen, err := cache.Note(ctx, note)
	if err != nil || seen == mark {
		return false, err
	}
	if err := cache.ForgetUnder(ctx, live.prefix()); err != nil {
		return false, err
	}
	return true, cache.SetNote(ctx, note, mark)
}
