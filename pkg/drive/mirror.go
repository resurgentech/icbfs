package drive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"time"
)

// A Mirror keeps a drive matching one source of documents (a WebDAV
// share, say): a sync daemon says what each source item is
// now, and the mirror works out what that means on the drive, keyed by
// the item's origin (the source drive and the item's id there).
//
// The trash is the mirror's undo. A source item deleted and then brought
// back with the same id (from a recycle bin, a folder shared again, a
// flaky API that came back short) is restored, history and all, not
// copied again; and a whole pass's removals can be undone at once. So a
// daemon needn't cap how much a pass may remove to limit the damage of a
// bad listing.
type Mirror struct {
	d           Drive
	source      string
	sourceTimes bool
}

// NewMirror is a mirror of the source drive source (an Origin.Drive) on d.
// d needs the Origin and Trash capabilities. A backend that implements
// OriginFinder finds items by origin in a lookup; any other is searched
// (a listing of the whole drive and its trash per lookup).
func NewMirror(d Drive, source string) *Mirror {
	return &Mirror{d: d, source: source}
}

// InSourceTime makes the mirror write the source's history in the
// source's time (WithSourceTime), so the drive's versions, trash and
// views of the past show when things happened there: an item is created
// at its Created time (else its ModTime), a new version written at its
// ModTime, a move made at its Changed time; Remove deletes at ctx's
// source time, which the daemon sets to when the source deleted it. A
// time the source item doesn't give is ctx's source time, if it has one,
// else now. Changes come in any order, though oldest first costs least.
// The drive's filesystem has to start before the source's history does
// (Settings.Created).
func (m *Mirror) InSourceTime() *Mirror {
	m.sourceTimes = true
	return m
}

// at is ctx for a change that happened at t at the source, when the
// mirror writes in source time and t is known.
func (m *Mirror) at(ctx context.Context, t time.Time) context.Context {
	if !m.sourceTimes || t.IsZero() {
		return ctx
	}
	return WithSourceTime(ctx, t)
}

// born is when src came to be at the source, as far as it says.
func born(src SourceItem) time.Time {
	if !src.Created.IsZero() {
		return src.Created
	}
	return src.ModTime
}

// OriginFinder is what a backend implements to find an item by its origin
// quickly: the live item copied from the source item, or, if it was
// deleted, its trash entry. Neither: ErrNotFound.
type OriginFinder interface {
	FindByOrigin(ctx context.Context, originDrive, originID string) (Item, *TrashEntry, error)
}

// SourceItem is what a sync daemon knows about one source item, now.
type SourceItem struct {
	// Origin is the source item: its id, the version to hold, its path
	// there. Origin.Drive is the mirror's source if left empty; Copied
	// is now if zero.
	Origin  Origin
	Path    string    // where it belongs on the drive
	Type    string    // TypeFile or TypeDir
	ModTime time.Time // its time at the source, if known
	Created time.Time // its creation time at the source, if known
	// Changed is when it was last renamed or moved at the source, if
	// known: what a move is dated with in source time (InSourceTime).
	Changed     time.Time
	ContentType string
	// App, Mode and WindowsAttrs are recorded on the item as ItemInfo
	// says, in the same write as its times and origin; nil leaves the
	// item's as they are. An item is Unchanged only when none differ.
	App          map[string][]byte
	Mode         *uint32
	WindowsAttrs *uint32
	// Open gives the content; it's called only when the bytes are needed,
	// never for a move, a restore of the version held, or an item already
	// up to date.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// Action is one thing Apply did.
type Action string

const (
	Created   Action = "created"
	Restored  Action = "restored" // from the trash, with its history
	Moved     Action = "moved"
	Updated   Action = "updated" // a new version
	Retimed   Action = "retimed" // its times set as the source's, no new version
	Unchanged Action = "unchanged"
)

// Applied is what Apply did, in order, and the item now.
type Applied struct {
	Item    Item
	Actions []Action
}

// ErrNotMirrored is Apply's answer when the path is taken by an item that
// wasn't copied from that source item (one made on the drive, say): a
// mirror doesn't write over what it didn't make. A directory is adopted
// instead.
var ErrNotMirrored = errors.New("the path holds an item not copied from that source item")

// Known is what the drive holds for a source item: the live item, or its
// trash entry, or neither (found false).
func (m *Mirror) Known(ctx context.Context, sourceID string) (item Item, trashed *TrashEntry, found bool, err error) {
	item, trashed, err = m.find(ctx, sourceID)
	if errors.Is(err, ErrNotFound) {
		return Item{}, nil, false, nil
	}
	if err != nil {
		return Item{}, nil, false, err
	}
	return item, trashed, true, nil
}

// Apply makes the drive match the source item: it creates the item if the
// drive has none for it; restores it if it's in the trash; moves it if
// its path changed; writes a new version if the version held differs from
// src's (the times and the origin in the same write); sets its times if
// only they differ from src's (on a drive that's a TimeSetter: a
// directory's creation time, a file's creation and modified times); or
// does nothing.
func (m *Mirror) Apply(ctx context.Context, src SourceItem) (Applied, error) {
	o := src.Origin
	if o.Drive == "" {
		o.Drive = m.source
	}
	if o.Drive != m.source {
		return Applied{}, fmt.Errorf("source item from %q on a mirror of %q: %w", o.Drive, m.source, ErrInvalid)
	}
	if o.ID == "" || src.Path == "" {
		return Applied{}, fmt.Errorf("a source item needs an id and a path: %w", ErrInvalid)
	}
	if o.Copied.IsZero() {
		o.Copied = time.Now().UTC()
	}
	if src.Type == "" {
		src.Type = TypeFile
	}

	var done []Action
	item, trashed, err := m.find(ctx, o.ID)
	if errors.Is(err, ErrNotFound) {
		// A file is written if its path is free, which is the write's own
		// condition: the path is looked at only if it's taken.
		if src.Type != TypeDir {
			item, err := m.write(m.at(ctx, born(src)), src, o, Condition{IfNoneMatch: true})
			var pe *PreconditionError
			if err == nil {
				return Applied{Item: item, Actions: []Action{Created}}, nil
			}
			if !errors.As(err, &pe) && !errors.Is(err, ErrIsDir) {
				return Applied{}, err
			}
		}
		// An item copied before the drive kept an index of origins, still
		// where it was copied to, is adopted: indexed now, then brought up
		// to date like any other.
		at, serr := m.d.Stat(ctx, ByPath(src.Path), At{})
		if serr != nil && !errors.Is(serr, ErrNotFound) {
			return Applied{}, serr
		}
		if serr != nil || at.Origin == nil || at.Origin.Drive != m.source || at.Origin.ID != o.ID {
			return m.create(ctx, src, o, at, serr == nil)
		}
		if err := m.d.SetOrigin(ctx, ItemRef{ID: at.ID, Path: at.Path}, *at.Origin); err != nil {
			return Applied{}, err
		}
		item, err = at, nil
	}
	switch {
	case err != nil:
		return Applied{}, err
	case trashed != nil:
		item, err = m.d.Restore(m.at(ctx, src.Changed), trashed.ID, RestoreOptions{To: src.Path, Parents: true})
		if err != nil {
			return Applied{}, fmt.Errorf("restore %s: %w", src.Path, err)
		}
		done = append(done, Restored)
	case item.Path != src.Path:
		mctx := m.at(ctx, src.Changed)
		if err := m.parentOf(mctx, src.Path); err != nil {
			return Applied{}, err
		}
		moved, err := m.d.Move(mctx, ItemRef{ID: item.ID, Path: item.Path}, src.Path, false)
		if err != nil {
			return Applied{}, fmt.Errorf("move %s to %s: %w", item.Path, src.Path, err)
		}
		item = moved
		done = append(done, Moved)
	}

	if src.Type == TypeDir || item.Origin != nil && item.Origin.Version == o.Version {
		// Nothing to write but what the item's record lacks: its times,
		// app data, mode, Windows attributes and origin (the source item's
		// path there, say), in one write and no new version. A
		// directory's modified time is its entries', here as anywhere, so
		// only its creation time follows the source after it's made.
		modified := src.ModTime
		if src.Type == TypeDir {
			modified = time.Time{}
		}
		item, changed, err := m.reinfo(ctx, item, modified, src, o)
		if err != nil {
			return Applied{}, err
		}
		if changed {
			done = append(done, Retimed)
		}
		if len(done) == 0 {
			done = append(done, Unchanged)
		}
		return Applied{Item: item, Actions: done}, nil
	}
	cond := Condition{}
	if item.Version != nil {
		cond.IfMatch = item.Version.ID
	}
	item, err = m.write(m.at(ctx, src.ModTime), src, o, cond)
	if err != nil {
		return Applied{}, err
	}
	return Applied{Item: item, Actions: append(done, Updated)}, nil
}

// parentOf makes p's parent directory if it doesn't exist yet, as a
// write with Parents would: Move doesn't.
func (m *Mirror) parentOf(ctx context.Context, p string) error {
	dir := path.Dir(p)
	if dir == "/" || dir == "." {
		return nil
	}
	if _, err := m.d.Stat(ctx, ByPath(dir), At{}); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := m.d.Mkdir(ctx, dir, true)
	return err
}

// sameOrigin reports whether a says what o does, when it was copied aside.
func sameOrigin(a *Origin, o Origin) bool {
	return a != nil && a.Drive == o.Drive && a.ID == o.ID && a.Version == o.Version && a.Path == o.Path
}

// create makes a source item the drive has never held; existing is
// what's at its path, if taken.
func (m *Mirror) create(ctx context.Context, src SourceItem, o Origin, existing Item, taken bool) (Applied, error) {
	ctx = m.at(ctx, born(src))
	var err error
	if src.Type == TypeDir {
		item := existing
		modified := time.Time{} // an adopted directory's is its entries'
		if !taken {
			if item, err = m.d.Mkdir(ctx, src.Path, true); err != nil {
				return Applied{}, err
			}
			modified = src.ModTime
		} else if existing.Type != TypeDir {
			return Applied{}, fmt.Errorf("%s: %w", src.Path, ErrNotMirrored)
		}
		if item, _, err = m.reinfo(ctx, item, modified, src, o); err != nil {
			return Applied{}, err
		}
		return Applied{Item: item, Actions: []Action{Created}}, nil
	}
	if taken {
		return Applied{}, fmt.Errorf("%s: %w", src.Path, ErrNotMirrored)
	}
	item, err := m.write(ctx, src, o, Condition{IfNoneMatch: true})
	if err != nil {
		return Applied{}, err
	}
	return Applied{Item: item, Actions: []Action{Created}}, nil
}

// write stores src's content at its path, its time and origin with it.
// The source is opened when the drive first reads the content: not at
// all if cond refuses the write.
func (m *Mirror) write(ctx context.Context, src SourceItem, o Origin, cond Condition) (Item, error) {
	if src.Open == nil {
		return Item{}, fmt.Errorf("%s: the content is needed and there's no Open: %w", src.Path, ErrInvalid)
	}
	body := &openOnRead{ctx: ctx, src: src}
	defer body.Close()
	return m.d.Write(ctx, src.Path, body, cond, WriteOptions{
		Parents: true, ContentType: src.ContentType, ModTime: src.ModTime, Created: src.Created, Origin: &o,
		App: src.App, Mode: src.Mode, WindowsAttrs: src.WindowsAttrs,
	})
}

// openOnRead is a source item's content, opened at the first Read.
type openOnRead struct {
	ctx  context.Context
	src  SourceItem
	body io.ReadCloser
	err  error
}

func (r *openOnRead) Read(p []byte) (int, error) {
	if r.body == nil && r.err == nil {
		if r.body, r.err = r.src.Open(r.ctx); r.err != nil {
			r.err = fmt.Errorf("open %s at the source: %w", r.src.Path, r.err)
		}
	}
	if r.err != nil {
		return 0, r.err
	}
	return r.body.Read(p)
}

func (r *openOnRead) Close() error {
	if r.body == nil {
		return nil
	}
	return r.body.Close()
}

// reinfo records on item what src says and item's record doesn't yet
// have: modified (zero to leave it) and src.Created where they differ,
// app keys whose values differ, a differing mode or Windows attributes,
// and origin o if it isn't item's; in one write on a drive that's an
// InfoSetter, else times (TimeSetter) and origin separately, and app
// keys, mode and attributes not at all (ErrNotSupported if given). It
// reports whether anything but the origin was written.
func (m *Mirror) reinfo(ctx context.Context, item Item, modified time.Time, src SourceItem, o Origin) (Item, bool, error) {
	info := ItemInfo{}
	if !modified.IsZero() && !modified.Equal(item.Modified) {
		info.Modified = modified
	}
	if !src.Created.IsZero() && !src.Created.Equal(item.Created) {
		info.Created = src.Created
	}
	for k, v := range src.App {
		cur, has := item.App[k]
		if v == nil && has || v != nil && (!has || !bytes.Equal(cur, v)) {
			if info.App == nil {
				info.App = map[string][]byte{}
			}
			info.App[k] = v
		}
	}
	if src.Mode != nil && *src.Mode != item.Mode {
		info.Mode = src.Mode
	}
	if src.WindowsAttrs != nil && (item.WindowsAttrs == nil || *item.WindowsAttrs != *src.WindowsAttrs) {
		info.WindowsAttrs = src.WindowsAttrs
	}
	changed := !info.Modified.IsZero() || !info.Created.IsZero() || len(info.App) > 0 || info.Mode != nil || info.WindowsAttrs != nil
	if !sameOrigin(item.Origin, o) {
		info.Origin = &o
	} else if !changed {
		return item, false, nil
	}
	ref := ItemRef{ID: item.ID, Path: item.Path}
	if is, ok := m.d.(InfoSetter); ok {
		set, err := is.SetInfo(ctx, ref, info)
		if err != nil {
			return item, false, fmt.Errorf("record %s: %w", item.Path, err)
		}
		if set.Origin == nil {
			set.Origin = &o
		}
		return set, changed, nil
	}
	if len(info.App) > 0 || info.Mode != nil || info.WindowsAttrs != nil {
		return item, false, fmt.Errorf("app data, mode or attributes on %s: %w", item.Path, ErrNotSupported)
	}
	if info.Origin != nil {
		if err := m.d.SetOrigin(ctx, ref, o); err != nil {
			return item, false, err
		}
		item.Origin = &o
	}
	if !changed {
		return item, false, nil
	}
	ts, ok := m.d.(TimeSetter)
	if !ok {
		return item, false, nil
	}
	retimed, err := ts.SetTimes(ctx, ref, info.Modified, info.Created)
	if err != nil {
		return item, false, fmt.Errorf("set the times of %s: %w", item.Path, err)
	}
	if retimed.Origin == nil {
		retimed.Origin = item.Origin
	}
	return retimed, true, nil
}

// Remove sends the item copied from the source item to the trash (a
// directory with everything in it), recorded as ctx's pass if it has one
// (WithPass). It reports whether there was a live item to remove; one
// already in the trash, or never held, is nothing to do. In source time
// (InSourceTime), the deletion is dated with ctx's source time: when the
// source deleted it.
func (m *Mirror) Remove(ctx context.Context, sourceID string) (bool, error) {
	item, trashed, err := m.find(ctx, sourceID)
	if errors.Is(err, ErrNotFound) || err == nil && trashed != nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := m.d.Delete(ctx, ItemRef{ID: item.ID, Path: item.Path}, item.Type == TypeDir, Condition{}); err != nil {
		return false, err
	}
	return true, nil
}

// Undo restores everything the pass removed, each where it was: what a
// daemon does when it finds a pass ran on a bad listing. It returns how
// many items it brought back (a directory counts once, with its contents).
func (m *Mirror) Undo(ctx context.Context, pass string) (int, error) {
	if pass == "" {
		return 0, fmt.Errorf("undo needs a pass: %w", ErrInvalid)
	}
	var entries []TrashEntry
	for cursor := ""; ; {
		page, err := m.d.Trash(ctx, TrashOptions{Pass: pass, Cursor: cursor})
		if err != nil {
			return 0, err
		}
		entries = append(entries, page.Entries...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	n := 0
	// Oldest first, so a directory removed before something put back in it
	// is back first; inside a recursive delete only the directory that
	// started it, which brings back the rest.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Batch != "" && e.Batch != e.ID {
			continue
		}
		if _, err := m.d.Restore(ctx, e.ID, RestoreOptions{Parents: true}); err != nil {
			return n, fmt.Errorf("restore %s: %w", e.Path, err)
		}
		n++
	}
	return n, nil
}

// find is the drive's item, or trash entry, for the source item.
func (m *Mirror) find(ctx context.Context, sourceID string) (Item, *TrashEntry, error) {
	if f, ok := m.d.(OriginFinder); ok {
		return f.FindByOrigin(ctx, m.source, sourceID)
	}
	return m.search(ctx, sourceID)
}

// search finds by origin on a backend that can't: a listing of the whole
// drive, then of its trash.
func (m *Mirror) search(ctx context.Context, sourceID string) (Item, *TrashEntry, error) {
	var found *Item
	for cursor := ""; ; {
		page, err := m.d.List(ctx, ByPath("/"), ListOptions{Depth: 1 << 30, Cursor: cursor})
		if err != nil {
			return Item{}, nil, err
		}
		for i := range page.Items {
			it := page.Items[i]
			if it.Origin != nil && it.Origin.Drive == m.source && it.Origin.ID == sourceID {
				if found == nil || it.Origin.Copied.After(found.Origin.Copied) {
					found = &it // the newest claim wins
				}
			}
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if found != nil {
		return *found, nil, nil
	}
	trash, err := m.d.Trash(ctx, TrashOptions{OriginDrive: m.source, OriginID: sourceID, Limit: 1})
	if err != nil {
		return Item{}, nil, err
	}
	if len(trash.Entries) > 0 {
		return Item{}, &trash.Entries[0], nil
	}
	return Item{}, nil, fmt.Errorf("source item %s: %w", sourceID, ErrNotFound)
}
