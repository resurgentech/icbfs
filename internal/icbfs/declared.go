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
	"github.com/resurgentech/icbfs/internal/pb"
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
//
// restored names the keys whose current version was put back on top, and
// the .metadata of every directory changed behind its latest state.
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
	// A directory changed behind its latest state is late however its
	// .metadata write went: in a filesystem whose items list every
	// version, the directory's table says so.
	if f.sourceTime.Load() {
		ours := map[string]bool{}
		for _, w := range written {
			ours[w.VersionID] = true
		}
		for _, w := range written {
			meta := metadataObjectKey(w.Dir)
			if w.Dir == "" || restored[meta] {
				continue
			}
			dates, ok := tables[w.Dir]
			if !ok {
				if dates, err = objstore.ReadDates(ctx, f.store, w.Dir); err != nil {
					return restored, err
				}
				tables[w.Dir] = dates
			}
			for id, t := range dates {
				if !ours[id] && t.After(w.At) {
					restored[meta] = true // the drive rewrites its history (repairLateFolders)
					break
				}
			}
		}
	}
	return restored, nil
}

// latestDated reports whether w is dated no earlier than every version in
// its item's table (any of its objects': one of them later is reason
// enough to look).
func latestDated(dates objstore.Dates, w objstore.Written) bool {
	if _, ok := dates[w.VersionID]; !ok {
		return false
	}
	for id, t := range dates {
		if id != w.VersionID && t.After(w.At) {
			return false
		}
	}
	return true
}

// unfinishedAfter is how old a version an item's complete table doesn't
// list must be to count as a write that never finished: any write is
// dated well within it.
var unfinishedAfter = 5 * time.Minute // a variable for the tests

// eraseUnfinished erases from key's history, newest first, the versions
// its item's complete table doesn't list that are older than
// unfinishedAfter: written, never dated, the change that wrote them
// failed. A key's only version stays: erasing it would leave the name of
// an item with nothing behind it. It returns history without them.
func (f *Filesystem) eraseUnfinished(ctx context.Context, key string, history []objstore.Object, dates objstore.Dates) ([]objstore.Object, error) {
	cutoff := time.Now().Add(-unfinishedAfter)
	kept := history[:0:0]
	live := 0
	for _, o := range history {
		if !o.Deleted {
			live++
		}
	}
	for _, o := range history {
		_, dated := dates[o.VersionID]
		if o.Deleted || dated || !o.LastModified.Before(cutoff) || live <= 1 {
			kept = append(kept, o)
			continue
		}
		if err := f.store.Erase(ctx, key, o.VersionID); err != nil && !objstore.IsNotFound(err) {
			return nil, err
		}
		live--
	}
	return kept, nil
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
//
// In a filesystem written in a source's time the table lists every
// version (Filesystem.dated), so a write dated no earlier than all of
// them is current with nothing more to read. Only one behind lists the
// key's versions, and then erases any the table doesn't list that are
// older than unfinishedAfter: writes that never finished.
func (f *Filesystem) restoreCurrent(ctx context.Context, w objstore.Written, tables map[string]objstore.Dates, rekeys map[string][2]string, pending map[string]bool) (bool, error) {
	if w.Dir != "" {
		return false, nil
	}
	dates := tables[objstore.BaseKey(w.Key)]
	complete := f.dated(w.Key)
	if complete && latestDated(dates, w) {
		return false, nil // what was written is current, as it should be
	}
	history, err := f.store.History(ctx, w.Key)
	if err != nil {
		return false, err
	}
	if len(history) == 0 || history[0].VersionID != w.VersionID {
		return false, nil // something newer since, or gone: not ours to put right
	}
	if complete {
		if history, err = f.eraseUnfinished(ctx, w.Key, history, dates); err != nil {
			return false, err
		}
	}
	want, ok := dates.Pick(history, time.Now().Add(time.Hour))
	if !ok || want.VersionID == w.VersionID {
		return false, nil // what was written is current, as it should be
	}
	var obj *objstore.Object
	base := objstore.BaseKey(w.Key)
	cctx := objstore.DatesItself(ctx) // the copy is dated below, as want was
	if strings.HasSuffix(w.Key, ".metadata") {
		// The attributes as they were, plus the bookkeeping the late
		// write added about its own version (its checksums).
		obj, err = f.mergeMetadataOnTop(cctx, w.Key, want, w.VersionID, rekeys[base])
	} else {
		obj, err = f.copyVersionOnTop(cctx, w.Key, want)
		if err == nil && obj.ETag != want.ETag {
			rekeys[base] = [2]string{want.ETag, obj.ETag}
			// Content: its .metadata follows the new ETag, now, unless a
			// .metadata write of this operation is still to be put right,
			// which does it (a write here would be the newer version, and
			// leave that one as it is).
			if base == w.Key && !pending[metadataObjectKey(base)] {
				err = f.writeMetadataIf(objstore.KeepDate(ctx), base, false, func(a *Attr) bool {
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
	// The copy is the version now; the one it was copied from is the
	// same bytes at the same date, so it goes, and its line with it.
	if tables[base], err = objstore.UpdateDates(ctx, f.store, base, objstore.Dates{obj.VersionID: dates.Time(&want)}, want.VersionID); err != nil {
		// Undated, the copy would be taken for an unfinished write; it
		// goes, and the late write is on top until a try that works.
		_ = f.store.Erase(context.WithoutCancel(ctx), w.Key, obj.VersionID)
		return false, err
	}
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
	data, err := f.encodeMetadata(ctx, attr)
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

// SourceTime reports whether the filesystem is written in a source's
// time (its master entry's source_time), as of attaching or the last
// Refresh.
func (f *Filesystem) SourceTime() bool { return f.sourceTime.Load() }

// MarkSourceTime records that the filesystem is written in a source's
// time, before its first change made under one: from then on every
// version of its items is dated (dated).
func (f *Filesystem) MarkSourceTime(ctx context.Context) error {
	if f.sourceTime.Load() {
		return nil
	}
	err := updateFilesystemEntry(undeclared(ctx), f.store, f.fsName, func(e *pb.FilesystemEntry) {
		e.SourceTime = true
	})
	if err == nil {
		f.sourceTime.Store(true)
	}
	return err
}

// dated reports whether key is an object every version of which is to
// be dated (objstore.DateAll): in a filesystem written in a source's
// time, an item's content or .metadata, a directory's .metadata, or an
// origin record. Not a lock, a stream, or anything outside the tree.
func (f *Filesystem) dated(key string) bool {
	if !f.sourceTime.Load() {
		return false
	}
	base := objstore.BaseKey(key)
	if key != base && key != metadataObjectKey(base) {
		return false
	}
	rest, ok := strings.CutPrefix(base, f.id+"-")
	if !ok {
		return false
	}
	if _, err := uuid.Parse(rest); err == nil {
		return true
	}
	return rest == "root-"+f.fsName || strings.HasPrefix(rest, "origin-")
}

// SetPastChanged has fn told whenever this filesystem's own operations
// (a history replaced, a purge) change its past, instead of their
// sending the mark themselves: for a caller that sends marks of its own
// and spaces them out (the drive's, at most one a second).
func (f *Filesystem) SetPastChanged(fn func(context.Context)) { f.pastChanged = fn }

// markPast marks the past changed, or tells SetPastChanged's fn to.
func (f *Filesystem) markPast(ctx context.Context) error {
	if f.pastChanged != nil {
		f.pastChanged(ctx)
		return nil
	}
	return f.MarkPastChanged(ctx)
}

// ForgetPast makes cache drop everything it kept of live's filesystem,
// mark or no mark: for the filesystem's own writer, which knows it has
// just changed the past before its mark is sent.
func ForgetPast(ctx context.Context, live *Filesystem, cache *objstore.HistoryCache) error {
	return cache.ForgetUnder(ctx, live.prefix())
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
