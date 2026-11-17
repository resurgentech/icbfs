package drive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A change dated behind a folder's newest state (WithSourceTime) is a late
// change: the folder's states from the change's date to its newest state
// should have had it, and don't. A drive never writes a folder behind its
// newest state. Only a caller that queues late changes and settles each
// folder with all of them (a mirror daemon: LateSettler.SettleFolder) may
// make one: its drive defers late folders (for an icbfs drive, its
// Config.DeferLateFolders) and reports each write's late changes to the
// write's context (WithLateChanges). There, a late change to a folder's
// entries dated D, behind the folder's newest state dated T, is written
// into the folder's present, dated T (the new item's own content and
// record keep D), and reported with At = D and Until = T. Until the folder
// is settled, a view of the folder's past shows it as it was without the
// change up to T, and with it from T on; every view is a state the folder
// really had. The settle puts the change into the states in [D, T).
//
// On any other drive, and on a deferring drive under a context with no
// LateChanges, a write dated behind its folder's newest state is refused
// before anything is written (ErrLateChange).
//
// The intended caller keeps each change as a row late(dir, at, until,
// seq, op, name, item, type) in its own database, written in the
// transaction that records the item as applied, so a change is never lost
// between the write and the settle; it settles a folder from its rows
// when it chooses (at the end of a pass), and deletes the rows once
// SettleFolder returns nil. A settle of changes already settled writes
// nothing.
//
// Exclusion is the caller's, and the drive does not enforce it: from the
// time a settle hands out its record (SettleOptions.Record) to the time
// it is told the record can go (SettleOptions.Done), nothing but the
// settle writes the folder (a daemon holds an exclusive lock per folder
// for every write and for a settle). The record of a settle under way is
// the caller's too: nothing about it is kept by the drive or in the
// store. After a crash the caller settles the folder again with its rows
// and the record (SettleOptions.Cut) before it writes into the folder.

// The kinds of LateChange.
const (
	LateAdd    = "add"    // Name now names Item, of Type
	LateRemove = "remove" // Name named Item, and is now gone
	LateTouch  = "touch"  // the folder changed at At with no entry changed: a settle puts nothing anywhere for it
)

// ErrLateChange is what a change dated behind its folder's newest state
// gets from a drive that doesn't settle late changes: nothing of it was
// written. Its message says the dates and what to do instead. It is an
// ErrInvalid.
var ErrLateChange = fmt.Errorf("this drive does not rewrite a folder's past, so nothing was written; "+
	"write it with no source time, or one at or after the folder's newest state, "+
	"or through a mirror that defers and settles late changes: %w", ErrInvalid)

// ErrFolderNotAtPath is what SettleFolder returns when the folder is not
// at the path it was given (SettleOptions.Path): nothing is there, or
// another item is. Nothing was read beyond that path and nothing was
// written. It does not say the folder is gone: the caller looks the
// folder up as it looks up any item, and settles again with the path it
// finds, or drops the folder's changes if it is in no folder. It is not
// an ErrNotFound.
var ErrFolderNotAtPath = errors.New("the folder is not at the path given (nothing is there, or another item is), and nothing was written: look the folder up, and settle again with its path")

// ErrSettleRefused is what SettleFolder returns when the folder's states
// as read and the changes given don't give the folder's states without
// writing one the folder never had: the states read plus the changes are
// not the folder's present (a change missing from the rows, or a write
// into the folder not by these rules), a state would hold a name twice,
// or the folder was written behind its newest state by a writer not
// following them. Nothing was written; the rows and any record stay as
// they were. Settling the same rows again gives the same answer until the
// rows or the folder change: a caller counts these.
var ErrSettleRefused = errors.New("the settle is refused, and nothing was written: the folder's states as read and its late changes don't give its present")

// ErrSettleStopped is what SettleFolder returns when, while it wrote the
// folder's states, it found a version of the folder that it did not
// write (a writer outside the caller's exclusion): it stopped there and
// erased nothing. If it stopped before it had written any of the folder's
// states it released its record (SettleOptions.Done was told) and the
// rows stay to be settled again; otherwise the record stays, and the
// folder's present is one of its past states until a rerun with the
// record, which takes every version after the recorded present for its
// own (the other writer's too: undefined behaviour).
var ErrSettleStopped = errors.New("the settle stopped: the folder was written by another writer during the settle; nothing was erased")

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
	At     time.Time // the write's source time (D)
	// Until is the date of the folder's newest state when the change was
	// written (T): the change was written into the folder's present,
	// dated Until, so the states from Until on have it, and a settle puts
	// it into the states in [At, Until) and no others. After At; required
	// for LateAdd and LateRemove, none for LateTouch.
	Until time.Time
	Seq   int    // its place among the changes that write made to that folder, from 0
	Op    string // LateAdd, LateRemove or LateTouch
	Name  string // the entry's name (none for LateTouch)
	Item  string // the item id the entry names (none for LateTouch)
	Type  string // the item's type: TypeFile, TypeDir or TypeSymlink (none for LateTouch)
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
	// SettleFolder puts each change into the folder's states in [At,
	// Until), and only those, writing the folder's states from its earliest
	// change on once. All changes name one folder (else ErrInvalid); an
	// empty list does nothing. The folder as it is now is not changed.
	// Changes are played by At, those of one At in the order given.
	//
	// Every change is checked before anything is read: a time (At) not
	// set, before 1970 or in the future, and for an add or a removal an
	// Until not set, before 1970, in the future or not after its At, a
	// kind, type, name or item missing, are each refused with an error
	// that names the change (its index) and that fault (ErrInvalid).
	//
	// The folder is the directory at SettleOptions.Path with the folder's
	// id: required, and the drive never searches its tree for the folder.
	// Nothing there, or another item there, is ErrFolderNotAtPath, with
	// nothing written: the caller looks the folder up as it looks up any
	// item and settles again with the path it finds, or drops the rows of
	// a folder that is nowhere. A failure to read the way to the path is
	// that failure. With Cut, a folder not at Path (or no Path) is settled
	// by its id wherever it is, by the library's own housekeeping only.
	//
	// Before it writes anything it proves what it will write: each state is
	// the state it read there plus exactly the changes that cover it, and
	// the last is the folder's present; anything else is ErrSettleRefused,
	// with nothing written. A settle runs only when its caller calls it.
	//
	// The caller's rule, which the drive does not enforce: nothing but the
	// settle writes the folder from Record to Done (see above). From its
	// window on (its first write of the folder's .metadata or root block),
	// each write is conditional on the version before it, and one that
	// finds a version the settle did not write stops it
	// (ErrSettleStopped); a failed request is tried again (three tries)
	// before the settle gives up.
	//
	// ctx is an immediate stop, at every step: canceled, the request in
	// flight ends and no other begins (no try again, no further write of
	// the window, its commit or its erases, no further part); the error
	// is ctx's (errors.Is(err, context.Canceled)), and a record handed out
	// stays the caller's. A graceful stop is the caller's: it does not
	// cancel ctx, and the settle runs to its end (Done).
	//
	// A settle that gives up or is stopped after it handed out its record
	// is finished by settling again with that record (SettleOptions.Cut)
	// and the rows of the part that was cut (or those and more), in this
	// process or another, before anything else is written into the
	// folder: exactly as after a kill.
	//
	// The caller need never hold all of a folder's rows: successive calls,
	// each with the next rows in their order (At, then the order stored),
	// a batch at a time, leave the folder's states exactly as one call
	// with all of them. Settled.Changes says how many of the changes given,
	// from the first, are settled when a part fails.
	SettleFolder(ctx context.Context, changes []LateChange, opts SettleOptions) (Settled, error)

	// FolderWrittenBehind reports whether the folder's root block has a
	// version stored after one dated later: written behind its newest
	// state, as an older build's per-file repair cut short leaves a
	// folder. SettleFolder refuses such a folder (ErrSettleRefused);
	// rewriting its history as its views read it (ReplaceFolderHistory)
	// puts it right. One listing of the root block's history and one read
	// of the folder's .version; it writes nothing and decides nothing.
	FolderWrittenBehind(ctx context.Context, folder ItemRef) (bool, error)
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
	// short (as Record was handed it), passed back: while the recorded
	// present (Root and Meta) is stored, the folder's present is taken to
	// be it and every root and .metadata version stored after it is the
	// cut settle's (under the caller's exclusion, nothing else wrote the
	// folder since the record); once it is gone the cut settle had
	// committed, and what it left to erase goes. The recorded new blocks
	// nothing reaches go. Its changes must be among this settle's; its
	// Folder must be the changes'. Without Cut every stored version is a
	// state.
	Cut *SettleRecord
	// Path is the folder's path in the drive, as the caller holds it:
	// required (but with Cut). The folder is looked for there and nowhere
	// else (ErrFolderNotAtPath).
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
	// Times is the times of the states the settle writes: a rerun with the
	// record stops (ErrSettleStopped, nothing erased) on a version of the
	// folder stored after Root and Meta that is dated as none of them,
	// another writer's since the cut.
	Times []time.Time
}

// Settled is what a settle did: the states it read, the states it wrote,
// the versions it erased, and Changes: how many of the changes it was
// given, from the first, are settled (all of them when it returns nil;
// on an error, those of the parts before the one that failed, whose
// records were Done: the caller deletes those rows and keeps the rest).
type Settled struct{ Read, Written, Erased, Changes int }
