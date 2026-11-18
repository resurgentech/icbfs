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
	"github.com/resurgentech/icbfs/internal/block"
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
	// A version the operation wrote on top of (or built on: a directory's
	// root above the block it changed) that no table lists is one a write
	// left and never dated (killed between its put and its dating), or,
	// for a directory's block, an undated write's. It is listed now: at the
	// date its own write carried (objstore.PutAt), or, carrying none, at
	// its store's second or the operation's date if that is earlier; so it
	// never outranks what was written after it. The blocks a block left so
	// are found from it (its children, its right sibling), and dated so too.
	ours := map[string]bool{}
	for _, w := range rec.Written() {
		ours[w.VersionID] = true
	}
	over := map[string]objstore.Dates{}
	links := map[string][]string{} // version id -> what it points to
	for _, w := range rec.Overs() {
		o := w.Over
		if o == nil || o.Deleted || o.VersionID == "" || ours[o.VersionID] {
			continue
		}
		base := objstore.BaseKey(w.Key)
		// The table, as the dating below reads it anyway (one read per item
		// an operation: objstore.DatesRead).
		cur, err := objstore.ReadDates(ctx, f.store, base)
		if err != nil {
			return nil, err
		}
		if _, listed := cur[o.VersionID]; listed {
			continue
		}
		if _, ok := objstore.PutAt(o); !ok {
			// What the write read may not carry its store metadata (a
			// cache's copy): ask for this one version's.
			if h, err := f.store.HeadVersion(ctx, w.Key, o.VersionID); err == nil {
				o = h
			}
		}
		if over[base] == nil {
			over[base] = objstore.Dates{}
		}
		at := leftoverDate(o, w.At)
		if t, ok := over[base][o.VersionID]; !ok || at.Before(t) {
			over[base][o.VersionID] = at
		}
		links[o.VersionID] = append(links[o.VersionID], w.Links...)
	}
	add := rec.Dates()
	for base := range over {
		if add[base] == nil {
			add[base] = objstore.Dates{}
		}
	}
	var follow []string
	var followAt []time.Time
	// A directory's own record (its root block and .metadata) is dated
	// last: a write killed while it dates leaves its root undated, and the
	// next write, which reads that root first, finds from it whatever else
	// it left undated (dateLeftoverBlocks).
	roots := map[string]bool{}
	for _, w := range rec.Written() {
		if w.Dir != "" {
			roots[w.Dir] = true
		}
	}
	bases := make([]string, 0, len(add))
	for base := range add {
		bases = append(bases, base)
	}
	sort.SliceStable(bases, func(i, j int) bool {
		if roots[bases[i]] != roots[bases[j]] {
			return !roots[bases[i]]
		}
		return bases[i] < bases[j]
	})
	for _, base := range bases {
		dates := add[base]
		var listed []string
		if tables[base], listed, err = objstore.MergeDatesOver(ctx, f.store, base, dates, over[base], rec.Fresh(base)); err != nil {
			return nil, err
		}
		for _, id := range listed {
			for _, k := range links[id] {
				follow = append(follow, k)
				followAt = append(followAt, over[base][id])
			}
		}
	}
	if err := f.dateLeftoverBlocks(ctx, follow, followAt); err != nil {
		return nil, err
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

// leftoverDate is the date a version no table lists is listed at by a
// write dated at made on top of it: the date its own write carried, or
// its store's second, or at if that is earlier.
func leftoverDate(o *objstore.Object, at time.Time) time.Time {
	if t, ok := objstore.PutAt(o); ok {
		return t
	}
	t := objstore.StoreSecond(o.LastModified)
	if at.Before(t) {
		return at
	}
	return t
}

// dateLeftoverBlocks lists the current versions of blocks keys (with the
// dates of the leftovers that point to them, ats), and of the blocks
// they point to in turn, where no table lists them: the rest of what a
// killed write left. Only what such a leftover reaches is read.
func (f *Filesystem) dateLeftoverBlocks(ctx context.Context, keys []string, ats []time.Time) error {
	seen := map[string]bool{}
	for len(keys) > 0 {
		k, at := keys[0], ats[0]
		keys, ats = keys[1:], ats[1:]
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		head, err := f.store.Head(ctx, k)
		if objstore.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if _, ok := objstore.PutAt(head); !ok {
			if h, err := f.store.HeadVersion(ctx, k, head.VersionID); err == nil {
				head = h
			}
		}
		dates, err := objstore.ReadDates(ctx, f.store, k)
		if err != nil {
			return err
		}
		if _, ok := dates[head.VersionID]; ok {
			continue
		}
		t := leftoverDate(head, at)
		if _, _, err := objstore.MergeDatesOver(ctx, f.store, k, objstore.Dates{}, objstore.Dates{head.VersionID: t}, false); err != nil {
			return err
		}
		blk, _, err := f.readBlock(ctx, k)
		if err != nil {
			return err
		}
		for _, l := range blockLinks(blk) {
			keys, ats = append(keys, l), append(ats, t)
		}
	}
	return nil
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
// override, the checksums, a stub's Ref) to new: for a content version
// copied, whose copy has an ETag of its own.
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
	if r, ok := a.Refs[old]; ok {
		a.Refs[new] = r
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
		obj, _, err = f.copyVersionOnTop(cctx, w.Key, want)
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
	for etag, r := range late.Refs { // a late stub's, likewise
		if _, ok := attr.Refs[etag]; !ok {
			if attr.Refs == nil {
				attr.Refs = map[string]Ref{}
			}
			attr.Refs[etag] = r
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
//
// A stub stays a stub: it's copied with its metadata (a history listing,
// where want usually comes from, has none, so a version small enough to
// be one is looked at first), and the copy is a referrer of its target
// as the original was: the target's .refs gains the copy's entry before
// it is written (pending, then finalized), and the original's entry goes
// (dropRef). It returns the stub's Ref, for a copy of one.
func (f *Filesystem) copyVersionOnTop(ctx context.Context, key string, want objstore.Object) (*objstore.Object, *Ref, error) {
	if want.Metadata == nil && want.Size > 0 && want.Size <= maxRefRecordSize {
		head, err := f.store.HeadVersion(ctx, key, want.VersionID)
		if err != nil {
			return nil, nil, err
		}
		want.Metadata = head.Metadata
	}
	ref, isRef, err := RefOf(&want)
	if err != nil {
		return nil, nil, err
	}
	if !isRef {
		obj, err := f.copyBytesOnTop(ctx, key, want)
		return obj, nil, err
	}
	if err := f.copyReferrer(ctx, ref, key, want.VersionID); err != nil {
		return nil, nil, err
	}
	obj, err := f.copyBytesOnTop(ctx, key, want)
	if err != nil {
		return nil, nil, err // the pending entry stays, for fsck
	}
	if err := f.copiedReferrer(ctx, ref, key, want.VersionID, obj.VersionID); err != nil {
		return nil, nil, err
	}
	return obj, &ref, nil
}

// copyBytesOnTop is copyVersionOnTop's copy itself.
func (f *Filesystem) copyBytesOnTop(ctx context.Context, key string, want objstore.Object) (*objstore.Object, error) {
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

// MarkPastChangedSeen is MarkPastChanged for the filesystem's own writer,
// whose history cache already dropped what its change changed: if cache
// had seen the mark before this one (ForgetChangedPast), it is told it
// has seen this one too, so it doesn't drop everything when it next
// looks. It reports the mark before (as a HEAD saw it, "" for none) and
// the one it sent, for the writer's own views (which hold more than the
// cache: each item's dates) to say the same. Every drive and program
// sharing cache drops what it changes from it, so the note stays right
// for all of them; each one's views look at the mark for themselves. A
// mark another writer sends between this one's look at the mark (a HEAD)
// and its write is taken for seen as well: that writer's change is
// forgotten by this cache only when the next mark after it comes, or the
// cache's TTL ends.
func MarkPastChangedSeen(ctx context.Context, live *Filesystem, cache *objstore.HistoryCache) (before, after string, err error) {
	ctx = context.WithoutCancel(undeclared(ctx))
	switch obj, err := live.store.Head(ctx, live.pastKey()); {
	case err == nil:
		before = obj.VersionID
	case !objstore.IsNotFound(err):
		return "", "", err
	}
	note := "past:" + live.id
	seen, err := cache.Note(ctx, note)
	if err != nil {
		return "", "", err
	}
	obj, err := live.store.Put(ctx, live.pastKey(), bytes.NewReader(nil), nil, "")
	if err != nil {
		return "", "", err
	}
	if seen != before {
		return before, obj.VersionID, nil // the cache hadn't seen the mark before: it looks for itself
	}
	return before, obj.VersionID, cache.SetNote(ctx, note, obj.VersionID)
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
	_, forgot, err := CheckPastMark(ctx, live, cache)
	return forgot, err
}

// CheckPastMark is ForgetChangedPast that also reports the mark it
// found ("" for none): a cache shared by several drives says whether
// the cache is up to date, and each drive's own views compare the mark
// with the one they last saw.
func CheckPastMark(ctx context.Context, live *Filesystem, cache *objstore.HistoryCache) (mark string, forgot bool, err error) {
	obj, err := live.store.Head(ctx, live.pastKey())
	switch {
	case err == nil:
		mark = obj.VersionID
	case !objstore.IsNotFound(err):
		return "", false, err
	}
	note := "past:" + live.id
	seen, err := cache.Note(ctx, note)
	if err != nil || seen == mark {
		return mark, false, err
	}
	if err := cache.ForgetUnder(ctx, live.prefix()); err != nil {
		return mark, false, err
	}
	return mark, true, cache.SetNote(ctx, note, mark)
}

// LeftoverItem is one item DateLeftovers looks at: its key and its type.
type LeftoverItem struct {
	Key  string
	Type EntryType
}

// Leftovers is what DateLeftovers found: the versions it listed, and
// those it found unlisted and could not date (they carry no date of
// their own: written before writes carried one, or by a writer that
// stores none).
type Leftovers struct {
	Dated, Undatable int
}

// DateLeftovers finishes the dating of a dated write that was killed
// after its puts and before its dating (RecordDates): every current
// version of the items' objects that no .version lists and that carries
// the date its own write declared (objstore.PutAt) is listed at that
// date, and, for an item's content or record written behind its current
// state, what should be current is put back on top, as the write's
// dating would have done (restoreCurrent). For a directory: its
// .metadata and every block of its tree (the root, and the blocks it
// links to: one read of each). For a file: its content and .metadata; a
// content version that carries no date of its own takes the date of its
// .metadata's, when that is the same write's leftover too.
//
// The views of the past go by the .version records: a version a killed
// write left unlisted is taken at its store's time (Dates.Time), which,
// in a filesystem written in a source's time, is not the change's: from
// the write's own date until the store's time those views lack it. This
// puts them right (interloc docs/STATES.md IT-28). A version with no
// date of its own is left as it is, and counted: its store's time is
// what views take.
//
// It writes nothing when every current version is listed. Run twice, the
// second finds nothing. Killed part way, a run again finishes it. In a
// filesystem not written in a source's time it does nothing: no write
// there is dated other than by its store, so a version is in the views
// from the time it was stored, which is its write's own.
func (f *Filesystem) DateLeftovers(ctx context.Context, items []LeftoverItem) (Leftovers, error) {
	ctx = undeclared(ctx)
	var out Leftovers
	if !f.sourceTime.Load() {
		return out, nil
	}
	rec := &objstore.Recorder{}
	// unlisted is key's current version if no .version lists it, with
	// the date its write declared (ok false: none).
	unlisted := func(key string) (*objstore.Object, time.Time, bool, error) {
		head, err := f.store.Head(ctx, key)
		if objstore.IsNotFound(err) {
			return nil, time.Time{}, false, nil
		}
		if err != nil {
			return nil, time.Time{}, false, err
		}
		dates, err := objstore.ReadDates(ctx, f.store, objstore.BaseKey(key))
		if err != nil {
			return nil, time.Time{}, false, err
		}
		if _, listed := dates[head.VersionID]; listed {
			return nil, time.Time{}, false, nil
		}
		if _, ok := objstore.PutAt(head); !ok {
			if h, err := f.store.HeadVersion(ctx, key, head.VersionID); err == nil {
				head = h
			}
		}
		t, ok := objstore.PutAt(head)
		return head, t, ok, nil
	}
	seen := map[string]bool{}
	for len(items) > 0 {
		it := items[0]
		items = items[1:]
		if it.Key == "" || seen[it.Key] {
			continue
		}
		seen[it.Key] = true
		meta := metadataObjectKey(it.Key)
		mo, mt, mok, err := unlisted(meta)
		if err != nil {
			return out, err
		}
		if mo != nil {
			if mok {
				rec.Note(objstore.Written{Key: meta, VersionID: mo.VersionID, At: mt})
			} else {
				out.Undatable++
			}
		}
		if it.Type == TypeDir {
			n, u, added, err := f.dateLeftoverTree(ctx, it.Key)
			out.Dated += n
			out.Undatable += u
			if err != nil {
				return out, err
			}
			// The entries the killed write added (a new item's own objects,
			// written with it and dated with it): looked at too.
			items = append(items, added...)
			continue
		}
		co, ct, cok, err := unlisted(it.Key)
		if err != nil {
			return out, err
		}
		if co == nil {
			continue
		}
		if !cok && mo != nil && mok {
			ct, cok = mt, true // the same write's content: its record's date
		}
		if !cok {
			out.Undatable++
			continue
		}
		rec.Note(objstore.Written{Key: it.Key, VersionID: co.VersionID, At: ct})
	}
	if len(rec.Written()) == 0 {
		return out, nil
	}
	out.Dated += len(rec.Written())
	if _, err := f.RecordDates(ctx, rec); err != nil {
		return out, err
	}
	return out, nil
}

// dateLeftoverTree lists, at the date each carries, every current block
// of directory key's tree that no .version lists: the root and the blocks
// it links to, each read once. A block with no date of its own is
// counted, and left. added is the entries a block so dated has that its
// state before had not (blockBefore: one read): the items the killed
// write added, whose own objects it wrote and did not date either.
func (f *Filesystem) dateLeftoverTree(ctx context.Context, key string) (dated, undatable int, added []LeftoverItem, err error) {
	seen := map[string]bool{}
	keys := []string{key}
	for len(keys) > 0 {
		k := keys[0]
		keys = keys[1:]
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		body, obj, err := f.store.Get(ctx, k)
		if objstore.IsNotFound(err) {
			continue
		}
		if err != nil {
			return dated, undatable, added, err
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			return dated, undatable, added, err
		}
		blk, err := block.Decode(data)
		if err != nil {
			return dated, undatable, added, err
		}
		keys = append(keys, blockLinks(blk)...)
		dates, err := objstore.ReadDates(ctx, f.store, k)
		if err != nil {
			return dated, undatable, added, err
		}
		if _, listed := dates[obj.VersionID]; listed {
			continue
		}
		if _, ok := objstore.PutAt(obj); !ok {
			if h, err := f.store.HeadVersion(ctx, k, obj.VersionID); err == nil {
				obj = h
			}
		}
		t, ok := objstore.PutAt(obj)
		if !ok {
			undatable++
			continue
		}
		if _, _, err := objstore.MergeDatesOver(ctx, f.store, k, objstore.Dates{}, objstore.Dates{obj.VersionID: t}, false); err != nil {
			return dated, undatable, added, err
		}
		dated++
		if blk.Kind != block.Leaf {
			continue
		}
		before, err := f.blockBefore(ctx, k, obj.VersionID, dates)
		if err != nil {
			return dated, undatable, added, err
		}
		for _, e := range blk.Entries {
			if !before[e.UUID] {
				added = append(added, LeftoverItem{Key: e.UUID, Type: e.Type})
			}
		}
	}
	return dated, undatable, added, nil
}

// blockBefore is the items block k named in its state before the
// unlisted version: the version its .version (dates) dates latest, other
// than versionID, read once; none for a block with no dated version (a
// new block). No listing of the block's versions: the cost is one read,
// whatever the block's history.
func (f *Filesystem) blockBefore(ctx context.Context, k, versionID string, dates objstore.Dates) (map[string]bool, error) {
	out := map[string]bool{}
	prev, prevAt := "", time.Time{}
	for id, t := range dates {
		if id != versionID && (prev == "" || t.After(prevAt) || t.Equal(prevAt) && id > prev) {
			prev, prevAt = id, t
		}
	}
	if prev == "" {
		return out, nil
	}
	body, _, err := f.store.GetVersion(ctx, k, prev)
	if objstore.IsNotFound(err) {
		return out, nil // erased since: every entry is taken for the write's
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return nil, err
	}
	blk, err := block.Decode(data)
	if err != nil {
		return nil, err
	}
	for _, e := range blk.Entries {
		out[e.UUID] = true
	}
	return out, nil
}
