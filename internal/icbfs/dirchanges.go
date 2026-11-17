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

// checkLate refuses an entry change to dirKey under ctx's declared time,
// if ctx refuses late changes (RefuseLateChanges) and the directory has a
// state dated after it: one read of the directory's .version record.
// The rule is RecordDates': a directory changed behind its latest state
// is one whose table dates a version after the change.
func (f *Filesystem) checkLate(ctx context.Context, dirKey string) error {
	if refuse, _ := ctx.Value(refuseLateKey{}).(bool); !refuse || !f.sourceTime.Load() {
		return nil
	}
	at, ok := objstore.DeclaredTime(ctx)
	if !ok {
		return nil
	}
	rec := objstore.RecorderFrom(ctx)
	if rec != nil && rec.IsNew(dirKey) {
		return nil // made by this change: no state before it
	}
	ours := map[string]bool{}
	if rec != nil {
		for _, w := range rec.Written() {
			ours[w.VersionID] = true
		}
	}
	uctx := undeclared(ctx)
	dates, err := objstore.ReadDates(uctx, f.store, dirKey)
	if err != nil {
		return err
	}
	var newest time.Time
	for id, t := range dates {
		if !ours[id] && t.After(newest) {
			newest = t
		}
	}
	if len(dates) == 0 {
		// No record yet: the first one lists the versions the directory
		// has, at the times the store wrote them (objstore.MergeDates), and
		// RecordDates finds the change late against those.
		for _, key := range []string{dirKey, metadataObjectKey(dirKey)} {
			h, err := f.store.History(uctx, key)
			if err != nil {
				return err
			}
			for _, o := range h {
				if !o.Deleted && !ours[o.VersionID] && o.LastModified.After(newest) {
					newest = o.LastModified
				}
			}
		}
	}
	if newest.After(at) {
		return &LateChangeError{Dir: dirKey, At: at, Newest: newest}
	}
	return nil
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
