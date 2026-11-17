package icbfs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// The kinds of DirChange.
const (
	ChangeAdd    = "add"    // Name now names Item, of Type
	ChangeRemove = "remove" // Name named Item, and is gone
	ChangeTouch  = "touch"  // the directory changed with no entry changed
)

// DirChange is one change an operation made to a directory's entries,
// at its declared time.
type DirChange struct {
	Dir  string // the directory's key
	At   time.Time
	Op   string // ChangeAdd, ChangeRemove or ChangeTouch
	Name string
	Item string
	Type EntryType
	// Until, for a change At behind the directory's newest state, is that
	// state's time: the change was written into the directory's present,
	// dated Until (WithPresentDating), so the states from Until on have it
	// and those in [At, Until) don't yet (SettleDir puts it there).
	Until time.Time
}

// DirChanges collects the entry changes the operations under a context
// make under a declared time (WithDirChanges), in the order made: what a
// directory changed behind its newest state needs put into its past
// (SettleDir).
type DirChanges struct {
	mu   sync.Mutex
	list []DirChange
}

type dirChangesKey struct{}

// WithDirChanges has the entry changes made under ctx with a declared
// time collected in c.
func WithDirChanges(ctx context.Context, c *DirChanges) context.Context {
	return context.WithValue(ctx, dirChangesKey{}, c)
}

// For is the changes made to directory dir, in order.
func (c *DirChanges) For(dir string) []DirChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []DirChange
	for _, ch := range c.list {
		if ch.Dir == dir {
			out = append(out, ch)
		}
	}
	return out
}

// LateChangeError is a change to directory Dir's entries dated At, behind
// the directory's newest state (dated Newest), refused before anything
// of it was written to the directory (RefuseLateChanges).
type LateChangeError struct {
	Dir        string
	At, Newest time.Time
}

func (e *LateChangeError) Error() string {
	return fmt.Sprintf("the change is dated %s, behind the newest state of folder %s (dated %s)",
		e.At.UTC().Format(time.RFC3339Nano), e.Dir, e.Newest.UTC().Format(time.RFC3339Nano))
}

type refuseLateKey struct{}

// RefuseLateChanges has an entry change made under ctx with a declared
// time refused (a *LateChangeError) when the directory has a state dated
// after that time, before the directory is written: a caller that can't
// put the change into the directory's later states (SettleDir) refuses
// it rather than leave them without it.
func RefuseLateChanges(ctx context.Context) context.Context {
	return context.WithValue(ctx, refuseLateKey{}, true)
}

// A directory is never written behind its newest state. An entry change
// under a declared time D that is behind the directory's newest state T
// (the latest date in the directory's .version) is, under ctx:
//
//   - refused before the directory is written (RefuseLateChanges): a
//     caller that can't put the change into the directory's states before
//     T has it fail rather than leave them without it;
//   - or written into the directory's present, every block, root and
//     .metadata version it writes dated T, not D (WithPresentDating): the
//     item's own objects keep D. The change is recorded with D and
//     Until = T (DirChanges), for SettleDir to put into the states in
//     [D, T).
//
// Either way a directory's versions, in every one of its objects, are
// stored in the order of their dates, and a view of any time, which
// resolves each object on its own (objstore.AsOf), sees a state the
// directory really had.

// presentDates is a WithPresentDating context's: the date each directory
// written behind its newest state was written at.
type presentDates struct {
	mu sync.Mutex
	m  map[string]time.Time
}

type presentDatesKey struct{}

// WithPresentDating has an entry change made under ctx with a declared
// time behind its directory's newest state written into the directory's
// present, dated at that state (see above), and noted (PresentDated).
func WithPresentDating(ctx context.Context) context.Context {
	return context.WithValue(ctx, presentDatesKey{}, &presentDates{m: map[string]time.Time{}})
}

// PresentDated is the directories the changes under ctx wrote dated at
// their newest state, and that state's time.
func PresentDated(ctx context.Context) map[string]time.Time {
	p, _ := ctx.Value(presentDatesKey{}).(*presentDates)
	out := map[string]time.Time{}
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.m {
		out[k] = v
	}
	return out
}

// presentDate is the date ctx's changes write directory dirKey at, if
// they write it dated at its newest state.
func presentDate(ctx context.Context, dirKey string) (time.Time, bool) {
	p, _ := ctx.Value(presentDatesKey{}).(*presentDates)
	if p == nil {
		return time.Time{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.m[dirKey]
	return t, ok
}

// lateDating is ctx for an entry change to dirKey: if ctx has a declared
// time behind the directory's newest state, the change is refused
// (RefuseLateChanges) or ctx is dated at that state (WithPresentDating);
// otherwise ctx as it is. One read of the directory's .version record,
// made only under one of the two and a declared time.
func (f *Filesystem) lateDating(ctx context.Context, dirKey string, root *objstore.Object) (context.Context, error) {
	refuse, _ := ctx.Value(refuseLateKey{}).(bool)
	p, _ := ctx.Value(presentDatesKey{}).(*presentDates)
	if !refuse && p == nil || !f.sourceTime.Load() {
		return ctx, nil
	}
	at, ok := objstore.DeclaredTime(ctx)
	if !ok {
		return ctx, nil
	}
	if t, ok := presentDate(ctx, dirKey); ok {
		return objstore.WithDeclaredTime(ctx, t), nil // dated so by this operation already
	}
	rec := objstore.RecorderFrom(ctx)
	if rec != nil && rec.IsNew(dirKey) {
		return ctx, nil // made by this change: no state before it
	}
	newest, err := f.newestState(ctx, dirKey, rec, root)
	if err != nil {
		return ctx, err
	}
	if !newest.After(at) {
		return ctx, nil
	}
	if refuse {
		return ctx, &LateChangeError{Dir: dirKey, At: at, Newest: newest}
	}
	p.mu.Lock()
	p.m[dirKey] = newest
	p.mu.Unlock()
	return objstore.WithDeclaredTime(ctx, newest), nil
}

// recordDating is ctx for a change to directory dirKey's own record (its
// .metadata: its mode, its times, its app data, the origin a mirror
// keeps there). Under WithPresentDating, a declared time behind the
// directory's newest state is moved to that state: the change is written
// into the present, dated at that state, as a late entry change is, and
// so the present and every later state have it; it makes no DirChange
// (nothing for a settle to play: the states before keep the record they
// had, which a settle keeps too). Otherwise, or with no declared time,
// ctx as it is: under RefuseLateChanges a late record change is written
// at its own time and the record that should be current put back on top
// (RecordDates), as before.
func (f *Filesystem) recordDating(ctx context.Context, dirKey string) (context.Context, error) {
	p, _ := ctx.Value(presentDatesKey{}).(*presentDates)
	if p == nil || !f.sourceTime.Load() {
		return ctx, nil
	}
	at, ok := objstore.DeclaredTime(ctx)
	if !ok {
		return ctx, nil
	}
	if t, ok := presentDate(ctx, dirKey); ok {
		return objstore.WithDeclaredTime(ctx, t), nil // dated so by this operation already
	}
	rec := objstore.RecorderFrom(ctx)
	if rec != nil && rec.IsNew(dirKey) {
		return ctx, nil // made by this change: no state before it
	}
	newest, err := f.newestState(ctx, dirKey, rec, nil)
	if err != nil {
		return ctx, err
	}
	if !newest.After(at) {
		return ctx, nil
	}
	return objstore.WithDeclaredTime(ctx, newest), nil
}

// newestState is the time of directory dirKey's newest state: the latest
// date in its .version record, what rec (if set) recorded of this
// operation's own writes left out. The rule is RecordDates': a directory
// changed behind its latest state is one whose table dates a version
// after the change. The record is read through ctx's objstore.DatesRead,
// so the dating after the write (RecordDates) reads it no more: a dated
// entry change costs what it cost before the check. A directory with no
// record yet has its first one's dates (its versions at the store's
// times), which the dating would list anyway.
//
// A version no record lists (a block an undated write left) is dated at
// the store's second (objstore.Dates.Time), as its write's .metadata,
// which the record lists, is: the record's newest is never behind it.
//
// root, if set, is the root block's version the change read: one no record
// lists (an undated write's, or a write's killed before it was dated)
// counts at its store's second, so the change is never stored behind it.
func (f *Filesystem) newestState(ctx context.Context, dirKey string, rec *objstore.Recorder, root *objstore.Object) (time.Time, error) {
	ours := map[string]bool{}
	if rec != nil {
		for _, w := range rec.Written() {
			ours[w.VersionID] = true
		}
	}
	dates, _, err := objstore.StoreDatesRead(undeclared(ctx), f.store, dirKey)
	if err != nil {
		return time.Time{}, err
	}
	var newest time.Time
	for id, t := range dates {
		if !ours[id] && t.After(newest) {
			newest = t
		}
	}
	if root != nil && !root.Deleted && !ours[root.VersionID] {
		if _, listed := dates[root.VersionID]; !listed {
			// Not dated: a write killed before its dating says its own date
			// (objstore.PutAt); an undated write's is its store's second.
			t, ok := objstore.PutAt(root)
			if !ok {
				t = objstore.StoreSecond(root.LastModified)
			}
			if t.After(newest) {
				newest = t
			}
		}
	}
	return newest, nil
}

// noteDirChange records an entry change made under ctx, if it has a
// declared time and a DirChanges.
func noteDirChange(ctx context.Context, dir, op, name, item string, typ EntryType) {
	c, _ := ctx.Value(dirChangesKey{}).(*DirChanges)
	if c == nil {
		return
	}
	at, ok := objstore.DeclaredTime(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	c.list = append(c.list, DirChange{Dir: dir, At: at, Op: op, Name: name, Item: item, Type: typ})
	c.mu.Unlock()
}
