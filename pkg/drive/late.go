package drive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A change dated behind a folder's newest state (WithSourceTime) is a late
// change: the folder's later states should have had it, and don't. Only
// a caller that queues late changes and settles each folder once, with
// all of them (a mirror daemon: LateSettler.SettleFolder), has them put
// into the folder's past: its drive defers late folders (for an icbfs
// drive, its Config.DeferLateFolders) and reports each write's late
// changes to the write's context (WithLateChanges). On any other drive,
// and on a deferring drive under a context with no LateChanges, a write
// dated behind its folder's newest state is refused before anything is
// written (ErrLateChange).
//
// The intended caller keeps each change as a row late(dir, at, seq, op,
// name, item, type) in its own database, written in the transaction that
// records the item as applied, so a change is never lost between the
// write and the settle; it settles a folder from its rows when it has
// applied the folder's items, and deletes the rows once SettleFolder
// returns nil. A settle of changes already settled writes nothing.
//
// The record of a settle under way is the caller's too
// (SettleOptions.Record): nothing about it is kept by the drive or in the
// store. After a crash the caller settles the folder again with its rows
// and the record (SettleOptions.Cut) before it writes into the folder.

// The kinds of LateChange.
const (
	LateAdd    = "add"    // Name now names Item, of Type
	LateRemove = "remove" // Name named Item, and is now gone
	LateTouch  = "touch"  // the folder changed at At with no entry changed
)

// ErrLateChange is what a change dated behind its folder's newest state
// gets from a drive that doesn't settle late changes: nothing of it was
// written. Its message says the dates and what to do instead. It is an
// ErrInvalid.
var ErrLateChange = fmt.Errorf("this drive does not rewrite a folder's past, so nothing was written; "+
	"write it with no source time, or one at or after the folder's newest state, "+
	"or through a mirror that defers and settles late changes: %w", ErrInvalid)

// ErrFolderGone is what SettleFolder returns for a folder that is no
// longer under the drive's root (trashed or purged): nothing was written,
// and nothing is held; the caller drops the folder's rows. It is an
// ErrNotFound.
var ErrFolderGone = fmt.Errorf("the folder is gone (trashed or purged), and its late changes can't be settled: %w", ErrNotFound)

// IsLateChange reports whether err is a refused late change.
func IsLateChange(err error) bool { return errors.Is(err, ErrLateChange) }

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
// to c instead of being refused: the caller settles them later
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
	//
	// Each At is checked: a zero time, one before 1970 or one in the
	// future is refused (ErrInvalid). The folder must be a directory under
	// the drive's root, whoever asks; one that is gone (trashed or
	// purged) gets ErrFolderGone, and nothing is written.
	//
	// The caller's rule, which the drive does not enforce: nothing but
	// the settling caller writes into the folder from the time it is given
	// the record (SettleOptions.Record) to the time it is told the record
	// can go (SettleOptions.Done), and none of its own writes either (a
	// daemon's one worker a folder; its folders' queues settled before it
	// removes a folder). A write into the folder by anyone else in that
	// time (a person using a command-line client by hand in the folder
	// while a settle there was cut) is undone by the settle run again with
	// the record: every root and .metadata version of the folder newer
	// than the recorded present is taken for the cut settle's.
	//
	// From its window on (its first write of the folder's .metadata or
	// root block), a settle goes to its end whatever ctx does, trying a
	// failed request again before it gives up. A settle that gives up
	// after it handed out its record is finished by settling again with
	// the same changes and that record (SettleOptions.Cut), in this
	// process or another, before anything else is written into the
	// folder.
	SettleFolder(ctx context.Context, changes []LateChange, opts SettleOptions) (Settled, error)
}

// SettleOptions are SettleFolder's options.
type SettleOptions struct {
	// Record, if set, is handed the settle's record before the settle
	// writes anything, and the settle waits for it: the caller stores the
	// record where it survives the process (with the folder's rows). An
	// error stops the settle with nothing written. A settle that writes
	// nothing hands out no record.
	Record func(SettleRecord) error
	// Done, if set, is told when a settle that handed out a record has
	// finished (its states written and committed, what they supersede
	// erased): the record can go. An error from it is SettleFolder's.
	Done func(SettleRecord) error
	// Cut, if set, is the record of a settle of this folder that was cut
	// short (as Record was handed it), passed back: the folder's present
	// is taken to be the recorded one, every root and .metadata version
	// newer than it is the cut settle's (whatever the folder's dates
	// table lists), and the recorded new blocks nothing reaches go. Its
	// changes must be among this settle's. Without Cut every stored
	// version is a state.
	Cut *SettleRecord
	// Path, if set, is the folder's path in the drive, where it is looked
	// for first. Without it, a folder the drive hasn't found this run is
	// searched for through the drive's tree.
	Path string
}

// SettleRecord is what a settle is about to change, for its caller to
// keep until the settle has finished: enough to finish it after a cut,
// in any process.
type SettleRecord struct {
	Folder string // the folder's item id (its key)
	// Root and Meta are the versions of the folder's root block and of
	// its .metadata that are its present when the settle starts (V0):
	// every version stored after them is the settle's.
	Root, Meta string
	// Create is the keys of the new blocks the settle writes below the
	// folder's root (and any a cut settle it finishes wrote).
	Create []string
	// Old is the keys of the blocks below the root that the folder's
	// states from the settle's first on reach before it: once the settle
	// has committed, what of them no state reaches any more goes.
	Old []string
}

// Settled is what a settle did: the states it read, the states it wrote,
// the versions it erased.
type Settled struct{ Read, Written, Erased int }
