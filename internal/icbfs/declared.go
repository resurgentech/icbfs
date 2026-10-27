package icbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// A change made under objstore.WithDeclaredTime happened at that time, as
// far as the filesystem's history goes: a new item's key (and so its
// creation time) is minted at it, a directory's mtime becomes it, a trash
// entry is deleted at it, and every version it writes is dated at it in
// the item's .version record (RecordDates), which views of the past go
// by. ctime stays icbfs's own clock.
//
// Changes come in any order. One dated behind the item's current state
// is written as given, and then what should be current is put back on
// top (restoreCurrent), so the present stays right and the past gains
// the change at its date.

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

// undeclared is ctx without a declared time.
func undeclared(ctx context.Context) context.Context {
	if _, ok := objstore.DeclaredTime(ctx); !ok {
		return ctx
	}
	return objstore.WithoutDeclaredTime(ctx)
}

// RecordDates dates what rec recorded (the versions written under a
// declared time by an operation just done) in each item's .version
// record, and puts back on top whatever a version written behind its
// item's current state displaced. The operation's writes went through
// this filesystem's store, which records them (objstore.Recording).
func (f *Filesystem) RecordDates(ctx context.Context, rec *objstore.Recorder) (restored map[string]bool, err error) {
	ctx = undeclared(ctx)
	// Each item's table as written, for restoreCurrent; an item the
	// operation made has none to read first, and nothing to put right.
	tables := map[string]objstore.Dates{}
	for base, dates := range rec.Dates() {
		if tables[base], err = objstore.MergeDates(ctx, f.store, base, dates, rec.Fresh(base)); err != nil {
			return nil, err
		}
	}
	restored = map[string]bool{}
	// Content before .metadata: a content version put back on top is a
	// copy with an ETag of its own, and .metadata's references to the old
	// one (the mtime override, the checksums) follow it (rekeys).
	written := rec.Written()
	sort.SliceStable(written, func(i, j int) bool {
		return !strings.HasSuffix(written[i].Key, ".metadata") && strings.HasSuffix(written[j].Key, ".metadata")
	})
	rekeys := map[string][2]string{}
	pending := map[string]bool{}
	for _, w := range written {
		pending[w.Key] = true
	}
	for _, w := range written {
		if rec.IsNew(objstore.BaseKey(w.Key)) {
			continue // all its versions are this operation's, dated alike: the newest is current
		}
		did, err := f.restoreCurrent(ctx, w, tables, rekeys, pending)
		if err != nil {
			return restored, err
		}
		if did {
			restored[w.Key] = true
		}
	}
	return restored, nil
}

// rekeyETag moves what a references by content ETag old (the mtime
// override, the checksums) to new: for a content version copied, whose
// copy has an ETag of its own.
func rekeyETag(a *Attr, old, new string) {
	if old == "" || new == "" || old == new {
		return
	}
	if a.mtimeOverrideETag == old {
		a.mtimeOverrideETag = new
	}
	if c, ok := a.Checksums[old]; ok {
		a.Checksums[new] = c
	}
}

// restoreCurrent, after version w was written at w.At, makes the version
// that should be current current again: if the key's newest version by
// effective date isn't its newest stored one, that version is copied on
// top (server-side for content, read and written back for the small
// objects), dated as it was, and reported. tables holds each item's
// .version table as RecordDates wrote it, kept up to date here. A
// directory's blocks are the directory's to put right (the drive
// rewrites the copies from w.At on); they're left.
func (f *Filesystem) restoreCurrent(ctx context.Context, w objstore.Written, tables map[string]objstore.Dates, rekeys map[string][2]string, pending map[string]bool) (bool, error) {
	if w.Dir != "" {
		return false, nil
	}
	history, err := f.store.History(ctx, w.Key)
	if err != nil {
		return false, err
	}
	if len(history) == 0 || history[0].VersionID != w.VersionID {
		return false, nil // something newer since, or gone: not ours to put right
	}
	dates := tables[objstore.BaseKey(w.Key)]
	want, ok := dates.Pick(history, time.Now().Add(time.Hour))
	if !ok || want.VersionID == w.VersionID {
		return false, nil // what was written is current, as it should be
	}
	var obj *objstore.Object
	base := objstore.BaseKey(w.Key)
	if strings.HasSuffix(w.Key, ".metadata") {
		// The attributes as they were, plus the bookkeeping the late
		// write added about its own version (its checksums).
		obj, err = f.mergeMetadataOnTop(ctx, w.Key, want, w.VersionID, rekeys[base])
	} else {
		obj, err = f.copyVersionOnTop(ctx, w.Key, want)
		if err == nil && obj.ETag != want.ETag {
			rekeys[base] = [2]string{want.ETag, obj.ETag}
			// Content: its .metadata follows the new ETag, now, unless a
			// .metadata write of this operation is still to be put right,
			// which does it (a write here would be the newer version, and
			// leave that one as it is).
			if base == w.Key && !pending[metadataObjectKey(base)] {
				err = f.writeMetadataIf(ctx, base, false, func(a *Attr) bool {
					rekeyETag(a, want.ETag, obj.ETag)
					return true
				})
				if errors.Is(err, ErrNotFound) || objstore.IsNotFound(err) {
					err = nil
				}
			}
		}
	}
	if err != nil {
		return false, err
	}
	if tables[base], err = objstore.MergeDates(ctx, f.store, base, objstore.Dates{obj.VersionID: dates.Time(&want)}, false); err != nil {
		return false, err
	}
	// The copy is the version now; the one it was copied from is the
	// same bytes at the same date, so it goes.
	return true, f.store.Erase(ctx, w.Key, want.VersionID)
}

// mergeMetadataOnTop writes version want of .metadata key as its newest
// version, with the checksums the late version lateID recorded merged in.
func (f *Filesystem) mergeMetadataOnTop(ctx context.Context, key string, want objstore.Object, lateID string, rekey [2]string) (*objstore.Object, error) {
	read := func(versionID string) (Attr, error) {
		body, _, err := f.store.GetVersion(ctx, key, versionID)
		if err != nil {
			return Attr{}, err
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			return Attr{}, err
		}
		return decodeFileMetadata(data)
	}
	attr, err := read(want.VersionID)
	if err != nil {
		return nil, err
	}
	late, err := read(lateID)
	if err != nil {
		return nil, err
	}
	for etag, c := range late.Checksums {
		if _, ok := attr.Checksums[etag]; !ok {
			if attr.Checksums == nil {
				attr.Checksums = map[string]Checksums{}
			}
			attr.Checksums[etag] = c
		}
	}
	rekeyETag(&attr, rekey[0], rekey[1])
	attr.dated = attr.dated || late.dated
	data, err := encodeFileMetadata(ctx, attr)
	if err != nil {
		return nil, err
	}
	return f.store.Put(ctx, key, bytes.NewReader(data), nil, "")
}

// copyVersionOnTop writes version want of key as key's newest version, a
// server-side copy (one part up to the part size, several past it; a
// single part may be any size): nothing is downloaded. An empty version
// is written empty.
func (f *Filesystem) copyVersionOnTop(ctx context.Context, key string, want objstore.Object) (*objstore.Object, error) {
	if want.Size == 0 {
		return f.store.Put(ctx, key, bytes.NewReader(nil), want.Metadata, "")
	}
	partSize := f.partSize()
	var parts []objstore.Part
	for off := int64(0); off < want.Size; off += partSize {
		parts = append(parts, objstore.Part{Size: min(partSize, want.Size-off), CopyVersion: want.VersionID, CopyOffset: off})
	}
	return f.store.PutParts(ctx, key, parts, want.Metadata, "")
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
