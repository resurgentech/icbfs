package drive

import (
	"context"
	"sync"
	"time"
)

// A change dated behind a folder's newest state (WithSourceTime) is a late
// change: the folder's later states should have had it, and don't. A
// drive puts that right by writing the folder's states from the change's
// date on again. Doing so once per change costs a write-out of the later
// states per change; a caller applying many late changes to one folder
// (a mirror catching up) can instead have the drive report them
// (WithLateChanges), keep them, and settle each folder once, with all of
// its changes (LateSettler.SettleFolder).
//
// The intended caller keeps each change as a row late(dir, at, seq, op,
// name, item, type) in its own database, written in the transaction that
// records the item as applied, so a change is never lost between the
// write and the settle; it settles a folder from its rows when it has
// applied the folder's items, and deletes the rows once SettleFolder
// returns. A settle of changes already settled writes nothing.

// The kinds of LateChange.
const (
	LateAdd    = "add"    // Name now names Item, of Type
	LateRemove = "remove" // Name named Item, and is now gone
	LateTouch  = "touch"  // the folder changed at At with no entry changed
)

// LateChange is one change a write made to a folder behind the folder's
// newest state.
//
// The order changes are played in is one: by At, and those of one At in
// the order given to SettleFolder. A write's changes come in its order
// (Seq 0, 1, ...), and a caller keeping them as rows gives them back by
// at, then in the order it stored them (an insertion number such as
// SQLite's rowid), not by (at, seq): two writes at one time each number
// their changes from 0, and (at, seq) would interleave them.
type LateChange struct {
	Folder string    // the folder's item id
	At     time.Time // the write's source time
	Seq    int       // its place among the changes that write made to that folder, from 0
	Op     string    // LateAdd, LateRemove or LateTouch
	Name   string    // the entry's name (none for LateTouch)
	Item   string    // the item id the entry names (none for LateTouch)
	Type   string    // the item's type: TypeFile, TypeDir or TypeSymlink (none for LateTouch)
}

// LateChanges collects the late changes of the writes made under a
// context (WithLateChanges). It is safe for concurrent use.
type LateChanges struct {
	mu   sync.Mutex
	list []LateChange
}

// Add appends changes.
func (c *LateChanges) Add(changes ...LateChange) {
	c.mu.Lock()
	c.list = append(c.list, changes...)
	c.mu.Unlock()
}

// Take returns what was collected, in the order collected, and empties it.
func (c *LateChanges) Take() []LateChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.list
	c.list = nil
	return out
}

type lateChangesKey struct{}

// WithLateChanges has the writes made under ctx, on a drive that defers
// late folders (a LateSettler configured to), report their late changes
// to c instead of settling them: the caller settles them later
// (SettleFolder).
func WithLateChanges(ctx context.Context, c *LateChanges) context.Context {
	return context.WithValue(ctx, lateChangesKey{}, c)
}

// LateChangesFrom is ctx's LateChanges, or nil.
func LateChangesFrom(ctx context.Context) *LateChanges {
	c, _ := ctx.Value(lateChangesKey{}).(*LateChanges)
	return c
}

// LateSettler is a drive that can leave a late write's folder history to
// be settled later.
type LateSettler interface {
	// SettleFolder writes the folder's states from its earliest change on
	// once, with every change in place: each in the state at its time and
	// in every later one. All changes name one folder (else ErrInvalid);
	// an empty list does nothing. The folder as it is now is not changed.
	// The caller makes no other change to the folder while it runs.
	//
	// Each At is checked: a zero time, one before 1970 or one in the
	// future is refused (ErrInvalid). The folder must be a directory under
	// the drive's root, whoever asks.
	//
	// From its window on (its first write of the folder's .metadata or
	// root block), a settle goes to its end whatever ctx does, trying a
	// failed request again before it gives up. A settle that gives up after it may have written (it
	// told its Plan) is kept by the drive and finished before the drive's
	// next change (which is refused if it can't be), so no write builds
	// on the folder as the cut left it. A caller that settles again
	// itself passes the cut settle's plan (SettleOptions.Cut).
	SettleFolder(ctx context.Context, changes []LateChange, opts SettleOptions) (Settled, error)
}

// SettleOptions are SettleFolder's options.
type SettleOptions struct {
	// Plan, if set, is told what the settle will create and erase, before
	// its first write; an error from it stops the settle with nothing
	// written.
	Plan func(SettlePlan) error
	// Cut, if set, is the plan of a settle of this folder that was cut
	// short (as Plan was told it; a crash's, kept by the caller): what it
	// created that nothing reaches (before its commit) and what it
	// superseded and didn't erase (after) are erased by this one. Its
	// changes must be among this settle's.
	Cut *SettlePlan
	// Path, if set, is the folder's path in the drive, where it is looked
	// for first. Without it, a folder the drive hasn't found this run is
	// searched for through the drive's tree.
	Path string
}

// SettlePlan is what a settle is about to do.
type SettlePlan struct {
	Folder string
	// From: states before From are not read, written or erased (the
	// state at From is read, as the one before the earliest change).
	From   time.Time
	States int            // the states it writes
	Create []string       // the block keys it will create (all new)
	Erase  []StoreVersion // the versions it supersedes, erased after its commit
	// RootBefore and MetaBefore are the folder root block's and its
	// .metadata's newest stored versions before it writes: a version
	// newer than these and not in the folder's dates is the settle's own,
	// unfinished.
	RootBefore, MetaBefore string
}

// StoreVersion names one version of one stored object; a Version ""
// names every version of Key (the .version record of a block erased
// whole).
type StoreVersion struct{ Key, Version string }

// Settled is what a settle did: the states it read, the states it wrote,
// the versions it erased.
type Settled struct{ Read, Written, Erased int }
