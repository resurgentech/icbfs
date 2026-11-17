package icbfs

import (
	"context"
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
