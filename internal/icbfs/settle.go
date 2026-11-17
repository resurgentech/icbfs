package icbfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// Settling a directory: its states from its earliest late change on are
// written once, with every change in place, instead of the whole history
// once per change (ReplaceDirHistory).
//
// A late change never wrote the directory behind its newest state: it was
// written into the directory's present, dated at that state, Until
// (lateDating), so every one of the directory's objects holds its
// versions in the order of their dates and every view of the past is a
// state the directory really had. What a settle has left to do is put
// each change into the states in [At, Until), and only those.
//
//   - Build: the state just before the earliest change and every later
//     state are read (SettleInput.StateAt, through the history cache),
//     one at a time, into the working table as entry lifetimes
//     (objstore.SettleTable), a copy kept as read. The requests are the
//     later states' blocks', whatever the directory's whole history.
//     Nothing is left out but what a cut settle wrote (its record) and
//     versions shadowed by a newer one dated alike (as every view does);
//     nothing is judged late by its date. A root block stored behind a
//     later-dated one (a directory written outside this rule) is refused.
//   - Play: each change, by At and then in the order given, is an insert
//     or an end of a lifetime in the table, within [At, Until). No
//     request.
//   - Proof, before anything is written: every state from the earliest
//     change on must be the state read there with exactly the changes
//     whose [At, Until) holds it, played in order; and the last state
//     must be the directory's present. Anything else is refused
//     (ErrSettleRefused) with nothing written: a settle never puts in what
//     no change says, and never backdates.
//   - Record: before anything is written, the caller is handed the
//     settle's record (SettleInput.Record): the directory's present (the
//     versions of its root block and .metadata, V0), the keys of the new
//     blocks, and the old blocks below the root it may clear. Nothing of
//     it is written to the store.
//   - Write-out: the states from the first one that differs from what the
//     store has (known from the sums of the states read, not by reading
//     blocks back) are generated one at a time and written: every block
//     below a root first, under new keys no reader looks at; then each
//     state's .metadata and root block back to back, oldest first, two
//     requests a state and nothing between (the window in which a reader
//     of the present sees the directory as of an earlier state), each put
//     conditional on the version before it (the first on the version the
//     settle read); then the new versions dated in the directory's
//     .version (the commit). A conditional put that finds another
//     writer's version stops the settle (ErrSettleStopped): nothing is
//     erased.
//   - Erase, in this order: any version of the root or .metadata stored
//     after V0 that is not one of the new states (a cut settle's, or one
//     a lost answer left); V0 itself (its .metadata, then its root); the
//     blocks below the roots that no state reaches any more; the other
//     versions the new states supersede; the .version's lines of what is
//     gone. Then the caller is told the record can go (SettleInput.Done).
//
// A store error in the write-out is tried again (settleTries) before the
// settle gives up. The caller's context is an immediate stop at every
// step: canceled, the request in flight ends, no other begins (not a try
// again, not the next write, not an erase), and the record stays; the
// settle is finished by a rerun with it, as after a kill. A graceful stop
// is the caller's: it lets a settle under way run to its end.
//
// Exclusion is the caller's: from the record to Done nothing but the
// settle writes the directory (a daemon's lock on the folder, held for
// its every write and for a settle). A settle cut short is finished by
// settling again with its changes and its record (SettleInput.Cut), and
// what the rerun takes for the cut settle's own leftovers follows from
// that rule and from the one above:
//
//   - V0 was the directory's newest stored root and .metadata version
//     when the record was handed out, and, versions being stored in date
//     order, its newest dated too: the present.
//   - While V0 is in the store, nothing the cut settle erases from the old
//     states has been erased (V0 goes first after the commit), so the old
//     states are whole; and every root and .metadata version stored
//     after V0 was stored between the record and the cut, by the only
//     writer of the directory then: the cut settle's window (its puts, a
//     put whose answer was lost). Those are its leftovers; the present is
//     V0, and the settle runs again from it.
//   - Once V0 is gone, the cut settle had committed (only its erase phase,
//     after the commit, erases V0), and the states in the store are its:
//     the settle reads them as they are, writes nothing, and does the
//     erases left.
//
// Without a record every stored version is a state. A writer that breaks
// the exclusion is undefined behaviour; what the conditional window does
// about one is in SettleInput.
// settleTries is how many times a write-out request is made before the
// settle gives up.
const settleTries = 3

// settleRetryWait is the wait before a write-out request's next try,
// times the tries so far (a variable for the tests).
var settleRetryWait = 200 * time.Millisecond

// ErrSettleRefused is a settle that found it could not put the changes
// into the directory's past without writing a state the directory never
// had: the states it read plus the changes don't give the present, a
// state it would write isn't the state read there with exactly the
// changes that cover it, a state would hold a name twice, or the
// directory's root block has versions stored behind later-dated ones.
// Nothing was written.
var ErrSettleRefused = errors.New("the settle is refused, and nothing was written: the states read and the changes don't give the folder's states")

// ErrSettleStopped is a settle whose conditional window found a version
// of the directory's root block or .metadata that it didn't write (a
// writer the caller's exclusion didn't hold off): it stopped there and
// erased nothing.
var ErrSettleStopped = errors.New("the settle stopped: the folder was written by another writer during its write-out; nothing was erased")

// maxSettleAhead is how far past this machine's clock a change may be.
const maxSettleAhead = time.Second

// SettleInput is how SettleDir reads and where it keeps its work.
type SettleInput struct {
	// StateAt is the directory's entries as a view of t sees them (what a
	// cut settle wrote after upTo's versions left out:
	// objstore.HistoryCache.UpTo), the keys of the blocks below its root
	// the view read, and its .metadata as the view reads it (the
	// directory's own record then); an error satisfying ErrNotFound if it
	// didn't exist then. upTo is the same for every call of one settle.
	StateAt func(ctx context.Context, t time.Time, upTo map[string]string) ([]block.Entry, []string, []byte, error)
	// History is a key's history now, newest first (the history cache's,
	// kept for the views that StateAt reads through); nil for the store's.
	History func(ctx context.Context, key string) ([]objstore.Object, error)
	// Table is the settle's working table.
	Table *objstore.SettleTable
	// Record, if set, is handed the settle's record before its first
	// write; an error from it stops the settle with nothing written.
	Record func(SettleRecord) error
	// Done, if set, is told when a settle that handed out its record has
	// finished: the record can go.
	Done func(SettleRecord) error
	// Cut, if set, is the record of a settle of this directory that was
	// cut short, as Record was handed it. The directory is settled by its
	// key whether or not anything leads to it. If none of its root block
	// and .metadata is stored (purged), the record's new blocks go and Done
	// is told.
	//
	// A writer outside the exclusion: one whose version lands before the
	// window's first put stops the settle there (ErrSettleStopped) with
	// nothing of the window written; the settle then erases its own new
	// blocks and, if it was not given Cut, tells Done, so no rerun takes
	// the other writer's version for a leftover. One that lands later
	// stops it with its record held; a rerun with that record takes every
	// version after V0 for the cut settle's, the other writer's too.
	Cut *SettleRecord
	// Forget, if set, is given the keys whose history the settle changed,
	// once it's done: a cache's to drop.
	Forget func(ctx context.Context, keys []string) error
	// Keep, if set, is given each block and .metadata version the settle
	// wrote, with its content, after Forget: a cache's to keep.
	Keep func(ctx context.Context, o *objstore.Object, content []byte) error
	// ProveOnly stops the settle after its proof: it reads, plays and
	// proves, and writes nothing, hands out no record and tells Done
	// nothing. nil: the settle would be done; ErrSettleRefused: it would be
	// refused. Not with Cut.
	ProveOnly bool
}

// SettleRecord is what a settle hands its caller before it writes
// (SettleInput.Record).
type SettleRecord struct {
	// Root and Meta are the versions of the directory's root block and of
	// its .metadata that are its present (V0).
	Root, Meta string
	Create     []string // the keys of the new blocks below the root
	Old        []string // the old blocks below the root it may clear once committed
	// Times is the times of the states the settle writes, oldest first:
	// its window puts a root block and a .metadata version at each, and
	// its commit dates them there. A rerun takes a version after V0 that
	// the directory's .version dates otherwise for another writer's.
	Times []time.Time
}

// Settled is what a settle did.
type Settled struct{ Read, Written, Erased int }

// settleState is a state the write-out writes.
type settleState struct {
	at   time.Time
	keys int // blocks below its root
}

// lowerErase is what a settle does to one block below a root.
type lowerErase struct {
	key     string
	ids     []string
	whole   bool // every version goes, and the key's .version with it
	cover   bool // a deletion goes on top: no state from now on reaches it
	current bool // its newest stored version is among ids
}

// ValidSettleTime reports whether t can be a change's time: not zero,
// not before the Unix epoch (new keys are minted at it), not in the
// future.
func ValidSettleTime(t time.Time) bool { return SettleTimeFault(t) == "" }

// SettleTimeFault is what is wrong with t as a change's time, in a few
// words ("is not set", "is before 1970", "is in the future"), or "".
func SettleTimeFault(t time.Time) string {
	switch {
	case t.IsZero():
		return "is not set"
	case t.Before(time.Unix(0, 0)):
		return "is before 1970"
	case t.After(time.Now().Add(maxSettleAhead)):
		return "is in the future"
	}
	return ""
}

// SettleDir writes directory dirKey's states from the earliest of changes
// on, with each change in place: an add or a removal in the states in
// [At, Until) (an add where its name is free, the occupant keeping it; a
// removal of its name and item), a touch nowhere. Changes are played by
// time, and those of one time in the order given. The directory as it is
// now isn't changed: the last state written is it. Changes already
// settled write nothing. The caller makes no other change to the
// directory while it runs, nor, if it keeps the record, until it is told
// the record can go.
func (f *Filesystem) SettleDir(ctx context.Context, dirKey string, changes []DirChange, in SettleInput) (_ Settled, err error) {
	if err := f.checkWritable(); err != nil {
		return Settled{}, err
	}
	if len(changes) == 0 {
		return Settled{}, nil
	}
	ctx = undeclared(ctx)
	changes = append([]DirChange(nil), changes...)
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].At.Before(changes[j].At) })
	for _, ch := range changes {
		if ch.Dir != dirKey {
			return Settled{}, fmt.Errorf("a change to %s settling %s: %w", ch.Dir, dirKey, ErrInvalidArgument)
		}
		if f := SettleTimeFault(ch.At); f != "" {
			return Settled{}, fmt.Errorf("a change at %s: its time (At) %s: %w", ch.At.Format(time.RFC3339Nano), f, ErrInvalidArgument)
		}
		switch ch.Op {
		case ChangeAdd, ChangeRemove:
			if f := SettleTimeFault(ch.Until); f != "" {
				return Settled{}, fmt.Errorf("a change at %s until %s: its Until %s: %w",
					ch.At.Format(time.RFC3339Nano), ch.Until.Format(time.RFC3339Nano), f, ErrInvalidArgument)
			}
			if !ch.Until.After(ch.At) {
				return Settled{}, fmt.Errorf("a change at %s until %s: its Until is not after its time (At): %w",
					ch.At.Format(time.RFC3339Nano), ch.Until.Format(time.RFC3339Nano), ErrInvalidArgument)
			}
		case ChangeTouch:
		default:
			return Settled{}, fmt.Errorf("change %q: %w", ch.Op, ErrInvalidArgument)
		}
	}
	if in.ProveOnly && in.Cut != nil {
		return Settled{}, fmt.Errorf("a settle that only proves, given the record of a cut one: %w", ErrInvalidArgument)
	}
	history := in.History
	if history == nil {
		history = f.store.History
	}
	tab := in.Table
	defer func() {
		if cerr := tab.Clear(context.WithoutCancel(ctx)); cerr != nil && err == nil {
			err = cerr
		}
	}()
	t0 := changes[0].At
	meta := metadataObjectKey(dirKey)

	// --- Build ---
	dates, err := objstore.ReadDates(ctx, f.store, dirKey)
	if err != nil {
		return Settled{}, err
	}
	rootHist, err := history(ctx, dirKey)
	if err != nil {
		return Settled{}, err
	}
	metaHist, err := history(ctx, meta)
	if err != nil {
		return Settled{}, err
	}
	// The .version record's history too, so views read the dates just read
	// (a cache shared with another drive may hold an older one).
	if _, err := history(ctx, dirKey+objstore.VersionSuffix); err != nil {
		return Settled{}, err
	}
	if in.Cut != nil && len(liveOf(rootHist))+len(liveOf(metaHist)) == 0 {
		// Purged while its settle was cut: nothing of it to finish but the
		// cut settle's new blocks.
		return f.settlePurged(ctx, *in.Cut, in.Done)
	}
	// The present (V0), and what of the store's versions are the
	// directory's states.
	var rec SettleRecord
	rootH, metaH := rootHist, metaHist
	var leftovers []objstore.Object // stored after V0: a cut settle's
	var upTo map[string]string
	if cut := in.Cut; cut != nil {
		rec.Root, rec.Meta = cut.Root, cut.Meta
		ri, mi := objstore.VersionIndex(rootHist, cut.Root), objstore.VersionIndex(metaHist, cut.Meta)
		if ri >= 0 && mi >= 0 {
			// V0 is whole: nothing of the old states is erased yet.
			rootH, metaH = rootHist[ri:], metaHist[mi:]
			leftovers = append(liveOf(rootHist[:ri]), liveOf(metaHist[:mi])...)
			upTo = map[string]string{dirKey: cut.Root, meta: cut.Meta}
			if err := cutLeftoversOurs(dirKey, rootHist[:ri], metaHist[:mi], dates, cut.Times); err != nil {
				return Settled{}, err
			}
		}
		// Else V0 is gone: the cut settle committed and began its erases,
		// and the states stored are its own.
	} else {
		if len(rootHist) > 0 {
			rec.Root = rootHist[0].VersionID
		}
		if len(metaHist) > 0 {
			rec.Meta = metaHist[0].VersionID
		}
	}
	// Of versions dated alike the newest stored is the state (as every view
	// takes it); nothing else is left out. A root block version stored
	// after one dated later was written behind the directory's newest
	// state, which no writer of this rule does: its views are no state the
	// directory had, and a settle can't tell which is right.
	shadowed, late := objstore.NotStates(rootH, dates)
	if len(late) > 0 {
		var ids []string
		for id := range late {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return Settled{}, fmt.Errorf("directory %s: its root block has %d version(s) stored behind a later-dated one (%v): %w",
			dirKey, len(ids), ids, ErrSettleRefused)
	}
	ms, _ := objstore.NotStates(metaH, dates)
	for id := range ms {
		shadowed[id] = true
	}
	times := map[time.Time]bool{}
	var tb time.Time
	for _, h := range [][]objstore.Object{rootH, metaH} {
		for i := range h {
			o := &h[i]
			if o.Deleted || shadowed[o.VersionID] {
				continue
			}
			t := dates.Time(o).UTC()
			if !t.Before(t0) {
				times[t] = true
			} else if t.After(tb) {
				tb = t
			}
		}
	}
	read := 0
	readState := func(t time.Time) error {
		entries, keys, recData, err := in.StateAt(ctx, t, upTo)
		if errors.Is(err, ErrNotFound) || objstore.IsNotFound(err) {
			return nil // the directory didn't exist then
		}
		if err != nil {
			return fmt.Errorf("the directory as of %s: %w", t.Format(time.RFC3339Nano), err)
		}
		read++
		if err := tab.AddBlocks(ctx, t, keys...); err != nil {
			return err
		}
		recAttr, err := decodeFileMetadata(recData)
		if err != nil {
			return fmt.Errorf("the directory's record as of %s: %w", t.Format(time.RFC3339Nano), err)
		}
		recSum, err := recordSum(recAttr)
		if err != nil {
			return err
		}
		_, err = tab.ReadState(ctx, t, toSettleEntries(entries), recData, recSum)
		return err
	}
	if !tb.IsZero() {
		if err := readState(tb); err != nil {
			return Settled{}, err
		}
	}
	for _, t := range sortedTimes(times) {
		if err := readState(t); err != nil {
			return Settled{}, err
		}
	}
	// The present, and its blocks.
	present, presentKeys, attr, err := f.settlePresentNow(ctx, dirKey, firstLive(rootH), firstLive(metaH),
		len(leftovers) == 0)
	if err != nil {
		return Settled{}, err
	}
	if err := tab.AddBlocks(ctx, objstore.SettlePresent, presentKeys...); err != nil {
		return Settled{}, err
	}

	// --- Play ---
	for _, ch := range changes {
		switch ch.Op {
		case ChangeAdd, ChangeRemove:
		default:
			continue // a touch names no entry: nothing to put anywhere
		}
		for _, at := range []time.Time{ch.At, ch.Until} {
			if err := tab.EnsureState(ctx, at); err != nil {
				return Settled{}, err
			}
		}
		if ch.Op == ChangeAdd {
			err = tab.Add(ctx, ch.At, ch.Until, objstore.SettleEntry{Name: ch.Name, Item: ch.Item, Type: int(ch.Type)})
		} else {
			err = tab.Remove(ctx, ch.At, ch.Until, ch.Name, ch.Item)
		}
		if err != nil {
			return Settled{}, err
		}
	}

	// --- Proof ---
	// A table emptied under the settle (another process that took its run
	// for dead) is no proof of anything, refused or not.
	perr := f.settleProof(ctx, tab, changes, t0, present, attr)
	if err := tab.Held(ctx); err != nil {
		return Settled{}, err
	}
	if perr != nil {
		return Settled{}, perr
	}
	if in.ProveOnly {
		return Settled{Read: read}, nil
	}

	// --- Write-out: what to write ---
	gen, first, err := f.settleStates(ctx, tab, t0, tb)
	if err != nil {
		return Settled{}, err
	}
	var written []settleState
	var tw time.Time
	if first >= 0 {
		written = gen[first:]
		tw = written[0].at
	}
	writes := len(written) > 0

	// What goes of the root and .metadata: the leftovers, then V0 (the
	// mark that the old states are no longer whole), then every other
	// version the new states supersede or that is shadowed.
	var v0, superseded []objstore.Object
	for _, h := range [][]objstore.Object{rootH, metaH} {
		for i := range h {
			o := &h[i]
			if o.Deleted {
				continue
			}
			if !(shadowed[o.VersionID] || writes && !dates.Time(o).Before(tw)) {
				continue
			}
			if o.VersionID == rec.Root || o.VersionID == rec.Meta {
				v0 = append(v0, *o)
			} else {
				superseded = append(superseded, *o)
			}
		}
	}
	// .metadata first: V0's root goes last of the two.
	sort.SliceStable(v0, func(i, j int) bool { return v0[i].Key == meta && v0[j].Key != meta })
	var cutKeys []string
	if in.Cut != nil {
		cutKeys = append(append(cutKeys, in.Cut.Create...), in.Cut.Old...)
	}
	lower, err := f.lowerErasures(ctx, dirKey, tab, cutKeys, writes, tb, tw)
	if err != nil {
		return Settled{}, err
	}
	// What to write and to erase came from the table: whole still, or the
	// settle stops here, before its record.
	if err := tab.Held(ctx); err != nil {
		return Settled{}, err
	}
	// The store has it all already. A cut settle's rerun goes on all the
	// same: the cut may have come after its erases, in the .version's
	// lines or the past-changed mark, and its record is released (Done).
	if in.Cut == nil && !writes && len(leftovers)+len(v0)+len(superseded) == 0 && len(lower) == 0 {
		return Settled{Read: read}, nil
	}

	// New keys for the blocks below each written state's root, minted at
	// its time as writeDirTree's are.
	recorder := &objstore.Recorder{}
	var create []string
	for _, s := range written {
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.at), recorder)
		keys := make([]string, s.keys)
		for i := range keys {
			if keys[i], err = f.newKey(sctx); err != nil {
				return Settled{}, err
			}
		}
		if err := tab.SetNewKeys(ctx, s.at, keys); err != nil {
			return Settled{}, err
		}
		create = append(create, keys...)
	}
	// --- The record, before anything is written ---
	if in.Cut != nil {
		rec.Create = append(rec.Create, in.Cut.Create...)
		rec.Old = append(rec.Old, in.Cut.Old...)
	}
	rec.Create = append(rec.Create, create...)
	blocks, err := tab.Blocks(ctx)
	if err != nil {
		return Settled{}, err
	}
	for _, b := range blocks {
		rec.Old = append(rec.Old, b.Key)
	}
	rec.Create, rec.Old = uniqueStrings(rec.Create), uniqueStrings(rec.Old)
	rec.Times = nil
	for _, s := range written {
		rec.Times = append(rec.Times, s.at)
	}
	if in.Record != nil {
		if err := in.Record(rec); err != nil {
			return Settled{}, err
		}
	}

	// --- Write-out: the blocks below the roots, and each state's root
	// block and .metadata made, before the window ---
	keysOf := map[time.Time][]string{}
	if writes {
		all, err := tab.States(ctx, tw)
		if err != nil {
			return Settled{}, err
		}
		for _, s := range all {
			keysOf[s.At] = s.NewKeys
		}
	}
	type put struct {
		obj  *objstore.Object
		data []byte
	}
	// Only the last state's versions are kept for the cache (the present):
	// the write-out holds one state's blocks at a time.
	var kept []put
	for si, s := range written {
		last := si == len(written)-1
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.at), recorder)
		entries, err := tab.EntriesAt(ctx, s.at)
		if err != nil {
			return Settled{}, err
		}
		sorted, err := f.sortedEntries(fromSettleEntries(entries))
		if err != nil {
			return Settled{}, err
		}
		keys := keysOf[s.at]
		root, levels, err := f.dirTree(sorted, func() (string, error) {
			if len(keys) == 0 {
				return "", fmt.Errorf("the state at %s needs more blocks than were minted", s.at.Format(time.RFC3339Nano))
			}
			k := keys[0]
			keys = keys[1:]
			return k, nil
		})
		if err != nil {
			return Settled{}, err
		}
		for _, level := range levels {
			for i := len(level) - 1; i >= 0; i-- { // right to left, as writeDirTree
				data, err := level[i].blk.Encode()
				if err != nil {
					return Settled{}, err
				}
				obj, err := f.store.Put(objstore.AsBlock(sctx, dirKey), level[i].key, bytes.NewReader(data), nil, "")
				if err != nil {
					return Settled{}, err
				}
				if last {
					kept = append(kept, put{obj, data})
				}
			}
		}
		rootData, err := root.Encode()
		if err != nil {
			return Settled{}, err
		}
		// The state's own record: the .metadata the state read there (or
		// the one before it, for a state a change made) had, at the
		// state's time.
		recData, _, err := tab.RecordAt(ctx, s.at)
		if err != nil {
			return Settled{}, err
		}
		ma := attr
		if recData != nil {
			if ma, err = decodeFileMetadata(recData); err != nil {
				return Settled{}, err
			}
		}
		ma.Mtime = s.at
		if ma.Ctime.IsZero() || ma.Ctime.Before(s.at) {
			ma.Ctime = s.at
		}
		metaData, err := f.encodeMetadata(sctx, ma)
		if err != nil {
			return Settled{}, err
		}
		if err := tab.SetEncoded(ctx, s.at, rootData, metaData); err != nil {
			return Settled{}, err
		}
	}
	for base, d := range recorder.Dates() {
		if _, err := objstore.MergeDates(ctx, f.store, base, d, recorder.Fresh(base)); err != nil {
			return Settled{}, err
		}
	}
	// The states the window puts were made from the table: whole still, or
	// the settle stops before its window (the record stays: a rerun).
	if err := tab.Held(ctx); err != nil {
		return Settled{}, err
	}

	// The window, the commit and the erases run under the caller's
	// context: an immediate stop cuts them where they stand (the request
	// in flight ends, no other begins), the record stays, and a rerun with
	// it finishes them. Until then a reader of the present may see an
	// earlier state (the window) or what is superseded (the erases): what a
	// kill there leaves too.
	wctx := ctx

	// --- The window: each state's .metadata and root block, oldest
	// first, two requests a state and nothing else (a request tried again
	// is the same request), each conditional on the version before it: the
	// first on the version the settle read, each next on the one it just
	// wrote ---
	win := &objstore.Recorder{}
	retried := false
	winDates := objstore.Dates{}
	ours := map[string]bool{}
	etag := map[string]string{}
	for key, h := range map[string][]objstore.Object{dirKey: rootHist, meta: metaHist} {
		if len(h) > 0 && !h[0].Deleted {
			etag[key] = h[0].ETag
		}
	}
	for i, s := range written {
		rootData, metaData, err := tab.Encoded(wctx, s.at)
		if err != nil {
			return Settled{}, err
		}
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(wctx, s.at), win)
		for j, p := range []struct {
			key  string
			pctx context.Context
			data []byte
		}{{meta, sctx, metaData}, {dirKey, objstore.AsBlock(sctx, dirKey), rootData}} {
			obj, err := f.settlePut(p.pctx, p.key, p.data, etag[p.key], &retried)
			if errors.Is(err, ErrSettleStopped) && i == 0 && j == 0 && in.Cut == nil {
				// Nothing of the window is written: the settle's own new
				// blocks go, and the record with them, so the next settle
				// starts from the present the other writer left.
				return Settled{Read: read}, f.settleStoppedFirst(wctx, create, rec, in.Done, err)
			}
			if err != nil {
				return Settled{Read: read}, err
			}
			etag[p.key] = obj.ETag
			winDates[obj.VersionID] = s.at
			ours[obj.VersionID] = true
			if i == len(written)-1 {
				kept = append(kept, put{obj, p.data})
			}
		}
	}

	// --- The commit ---
	if writes {
		if err := settleRetry(wctx, nil, func() error {
			_, err := objstore.UpdateDates(wctx, f.store, dirKey, winDates)
			return err
		}); err != nil {
			return Settled{}, err
		}
	}

	// --- What goes ---
	// A put tried again may have been made the first time too (its answer
	// lost): a version after V0 that is none of the new states'.
	if retried {
		for _, key := range []string{dirKey, meta} {
			h, err := f.store.History(wctx, key)
			if err != nil {
				return Settled{}, err
			}
			v := rec.Root
			if key == meta {
				v = rec.Meta
			}
			i := objstore.VersionIndex(h, v)
			if i < 0 {
				i = len(h)
			}
			for _, o := range liveOf(h[:i]) {
				if !ours[o.VersionID] && objstore.VersionIndex(leftovers, o.VersionID) < 0 {
					leftovers = append(leftovers, o)
				}
			}
		}
	}
	erased := 0
	gone := map[string]bool{}
	eraseRoot := func(list []objstore.Object) error {
		for _, o := range list {
			if err := settleRetry(wctx, nil, func() error { return eraseGone(wctx, f.store, o.Key, o.VersionID) }); err != nil {
				return err
			}
			gone[o.VersionID] = true
			erased++
		}
		return nil
	}
	if err := eraseRoot(leftovers); err != nil {
		return Settled{}, err
	}
	if err := eraseRoot(v0); err != nil {
		return Settled{}, err
	}
	for _, le := range lower {
		n, err := f.eraseLower(wctx, le)
		erased += n
		if err != nil {
			return Settled{}, err
		}
	}
	// Each written state's blocks but the last's: the present doesn't
	// reach them, its state does (past its deletion), as ReplaceDirHistory
	// leaves them.
	for i := 0; i+1 < len(written); i++ {
		for _, k := range keysOf[written[i].at] {
			if err := settleRetry(wctx, nil, func() error {
				err := f.store.Delete(wctx, k)
				if objstore.IsNotFound(err) {
					return nil
				}
				return err
			}); err != nil {
				return Settled{}, err
			}
		}
	}
	if err := eraseRoot(superseded); err != nil {
		return Settled{}, err
	}
	// The .version's lines of versions no longer stored.
	var drop []string
	for id := range dates {
		stored := false
		for _, h := range [][]objstore.Object{rootHist, metaHist} {
			stored = stored || objstore.VersionIndex(h, id) >= 0
		}
		if !stored || gone[id] {
			drop = append(drop, id)
		}
	}
	if len(drop) > 0 {
		sort.Strings(drop)
		if err := settleRetry(wctx, nil, func() error {
			_, err := objstore.UpdateDates(wctx, f.store, dirKey, nil, drop...)
			return err
		}); err != nil {
			return Settled{}, err
		}
	}
	// The past mark is put whatever the context says (markPast): an
	// immediate stop before it leaves it to the rerun.
	if err := wctx.Err(); err != nil {
		return Settled{}, err
	}
	if err := f.markPast(wctx); err != nil {
		return Settled{}, err
	}

	// --- The cache: what changed goes, what was written is kept ---
	if in.Forget != nil {
		forget := []string{dirKey, meta, dirKey + objstore.VersionSuffix}
		for _, le := range lower {
			forget = append(forget, le.key, le.key+objstore.VersionSuffix)
		}
		for _, b := range blocks {
			forget = append(forget, b.Key, b.Key+objstore.VersionSuffix)
		}
		// The cache is this process's, not the store: a stop arriving after
		// the erases leaves no needless rerun.
		if err := in.Forget(context.WithoutCancel(ctx), forget); err != nil {
			return Settled{}, err
		}
	}
	if in.Keep != nil {
		for _, p := range kept {
			if err := in.Keep(context.WithoutCancel(ctx), p.obj, p.data); err != nil {
				return Settled{}, err
			}
		}
	}
	if in.Done != nil {
		if err := in.Done(rec); err != nil {
			return Settled{Read: read, Written: len(written), Erased: erased}, err
		}
	}
	return Settled{Read: read, Written: len(written), Erased: erased}, nil
}

// liveOf is list without its deletions.
func liveOf(list []objstore.Object) []objstore.Object {
	var out []objstore.Object
	for _, o := range list {
		if !o.Deleted {
			out = append(out, o)
		}
	}
	return out
}

// firstLive is list's newest stored version, if it isn't a deletion.
func firstLive(list []objstore.Object) *objstore.Object {
	if len(list) == 0 || list[0].Deleted {
		return nil
	}
	return &list[0]
}

func uniqueStrings(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// settleStates is the states from from on that the table generates, with
// the index of the first that differs from what the store has (-1 for
// none): a state read whose sum is not its sum as read, or one a change
// made. A state a change made that is the one before it is no state.
// prevAt, if set, is the state before from.
func (f *Filesystem) settleStates(ctx context.Context, tab *objstore.SettleTable, from, prevAt time.Time) ([]settleState, int, error) {
	states, err := tab.States(ctx, from)
	if err != nil {
		return nil, -1, err
	}
	prev := []objstore.SettleEntry{}
	if !prevAt.IsZero() {
		if prev, err = tab.EntriesAt(ctx, prevAt); err != nil {
			return nil, -1, err
		}
	}
	prevSum := objstore.SettleSum(prev)
	var gen []settleState
	first := -1
	for _, s := range states {
		entries, err := tab.EntriesAt(ctx, s.At)
		if err != nil {
			return nil, -1, err
		}
		sorted, err := f.sortedEntries(fromSettleEntries(entries))
		if err != nil {
			return nil, -1, fmt.Errorf("the state at %s: %v: %w", s.At.Format(time.RFC3339Nano), err, ErrSettleRefused)
		}
		sum := objstore.SettleSum(entries)
		if !s.Read && sum == prevSum {
			continue // a change that changed nothing here: no state of its own
		}
		if first < 0 && (!s.Read || sum != s.Sum) {
			first = len(gen)
		}
		n := 0
		if _, _, err := f.dirTree(sorted, func() (string, error) { n++; return "", nil }); err != nil {
			return nil, -1, err
		}
		gen = append(gen, settleState{at: s.At, keys: n})
		prevSum = sum
	}
	return gen, first, nil
}

// settlePresentNow is the directory as a settle takes it to be now:
// versions root and mobj of its root block and .metadata, with the
// blocks below as they are. live says they are the newest stored, read
// as the directory is read.
func (f *Filesystem) settlePresentNow(ctx context.Context, dirKey string, root, mobj *objstore.Object, live bool) ([]block.Entry, []string, Attr, error) {
	if root == nil || mobj == nil {
		return nil, nil, Attr{}, fmt.Errorf("directory %s has no version: %w", dirKey, ErrNotFound)
	}
	if live {
		entries, keys, err := f.ReadDirKeys(ctx, dirKey)
		if err != nil {
			return nil, nil, Attr{}, err
		}
		attr, err := f.statDir(ctx, dirKey)
		if err != nil {
			return nil, nil, Attr{}, err
		}
		// Read last, the present must be the root the settle listed as its
		// present (V0): another writer's version since (during the reads)
		// makes what was read no proof of anything (ST-22). One after this
		// read is the window's first put's to find.
		if cur, ok := attr.ContentVersion(); ok && cur.VersionID != root.VersionID {
			return nil, nil, Attr{}, fmt.Errorf("directory %s was written while the settle read it (its root block is version %s, not %s): %w",
				dirKey, cur.VersionID, root.VersionID, ErrSettleRefused)
		}
		return entries, keys, attr, nil
	}
	getVersion := func(key, id string) ([]byte, *objstore.Object, error) {
		body, obj, err := f.store.GetVersion(ctx, key, id)
		if err != nil {
			return nil, nil, mapNotFound(err)
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		return data, obj, err
	}
	rootData, rootObj, err := getVersion(dirKey, root.VersionID)
	if err != nil {
		return nil, nil, Attr{}, err
	}
	blk, err := block.Decode(rootData)
	if err != nil {
		return nil, nil, Attr{}, err
	}
	blk.Collation = f.collation()
	metaData, metaObj, err := getVersion(metadataObjectKey(dirKey), mobj.VersionID)
	if err != nil {
		return nil, nil, Attr{}, err
	}
	attr, err := combineDirAttr(dirKey, metaData, metaObj.LastModified, rootObj)
	if err != nil {
		return nil, nil, Attr{}, err
	}
	held := context.WithValue(ctx, heldBlockKey{}, &heldBlock{key: dirKey, blk: blk, obj: rootObj})
	entries, keys, err := f.ReadDirKeys(held, dirKey)
	return entries, keys, attr, err
}

// lowerErasures is what a settle does to the blocks below the roots: the
// blocks its states and the present reached (tab), and those a cut
// settle's record named (cut). A version goes if no state reaches it
// from now on: none read at or after its date did (states before tb
// aren't read, and don't reach a version dated after tb), or, when the
// settle writes (from tw), it is dated from tw on; and a version
// shadowed by a newer one dated alike, always. A block no state reaches any version of goes whole; one that
// the directory now doesn't reach gets a deletion on top (after writing,
// the present reaches only new keys).
func (f *Filesystem) lowerErasures(ctx context.Context, dirKey string, tab *objstore.SettleTable, cut []string, writes bool, tb, tw time.Time) ([]lowerErase, error) {
	blocks, err := tab.Blocks(ctx)
	if err != nil {
		return nil, err
	}
	reach := map[string]objstore.SettleBlock{}
	// Writing, every block a state or the present reached is superseded
	// from tw on; not writing, only what a cut settle named is looked at.
	var keys []string
	inTable := map[string]bool{}
	seen := map[string]bool{}
	for _, b := range blocks {
		reach[b.Key] = b
		inTable[b.Key] = true
		if writes {
			seen[b.Key] = true
			keys = append(keys, b.Key)
		}
	}
	for _, k := range cut {
		if !seen[k] && objstore.BaseKey(k) == k && k != dirKey {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	var out []lowerErase
	for _, k := range keys {
		b := reach[k]
		if b.Present && !writes {
			continue // the directory now reaches it, and stays as it is
		}
		h, err := f.store.History(ctx, k)
		if err != nil {
			return nil, err
		}
		kd, err := objstore.ReadDates(ctx, f.store, k)
		if err != nil {
			return nil, err
		}
		shadowed, _ := objstore.NotStates(h, kd)
		le := lowerErase{key: k}
		live := 0
		for i := range h {
			o := &h[i]
			if o.Deleted {
				continue
			}
			live++
			t := kd.Time(o)
			gone := shadowed[o.VersionID] ||
				t.After(tb) && (b.Last.IsZero() || t.After(b.Last)) ||
				writes && inTable[k] && !t.Before(tw)
			if gone {
				le.ids = append(le.ids, o.VersionID)
				if i == 0 {
					le.current = true
				}
			}
		}
		if live == 0 {
			continue // gone already, or a deletion on top
		}
		le.whole = len(le.ids) == live
		le.cover = !le.whole && (le.current || !h[0].Deleted)
		if len(le.ids) > 0 || le.cover {
			out = append(out, le)
		}
	}
	return out, nil
}

// eraseLower does le, and reports the versions it erased.
func (f *Filesystem) eraseLower(ctx context.Context, le lowerErase) (int, error) {
	if le.whole {
		if err := settleRetry(ctx, nil, func() error { return f.store.EraseAll(ctx, le.key) }); err != nil {
			return 0, err
		}
		return len(le.ids), nil
	}
	if le.cover {
		// No state from now on reaches the key: a deletion on top (before
		// erasing its newest version, which a store may refuse to erase
		// while it is current).
		if err := settleRetry(ctx, nil, func() error {
			err := f.store.Delete(ctx, le.key)
			if objstore.IsNotFound(err) {
				return nil
			}
			return err
		}); err != nil {
			return 0, err
		}
	}
	n := 0
	for _, id := range le.ids {
		if err := settleRetry(ctx, nil, func() error { return eraseGone(ctx, f.store, le.key, id) }); err != nil {
			return n, err
		}
		n++
	}
	if len(le.ids) > 0 {
		if err := settleRetry(ctx, nil, func() error {
			_, err := objstore.UpdateDates(ctx, f.store, le.key, nil, le.ids...)
			return err
		}); err != nil {
			return n, err
		}
	}
	return n, nil
}

// eraseGone erases a version, which may be gone already.
func eraseGone(ctx context.Context, store objstore.Store, key, id string) error {
	if err := store.Erase(ctx, key, id); err != nil && !objstore.IsNotFound(err) {
		return err
	}
	return nil
}

// settleRetry makes a write-out request, and again on an error, up to
// settleTries in all; retried, if set, is set when it tries again. Once
// ctx is done (an immediate stop) it begins no request and tries none
// again: its error is ctx's, with the request's.
func settleRetry(ctx context.Context, retried *bool, do func() error) error {
	var err error
	for try := 1; try <= settleTries; try++ {
		if cerr := ctx.Err(); cerr != nil {
			if err == nil {
				return cerr
			}
			return fmt.Errorf("%w (the request before: %v)", cerr, err)
		}
		if err = do(); err == nil || errors.Is(err, ErrSettleStopped) {
			return err
		}
		if retried != nil {
			*retried = true
		}
		if try < settleTries {
			t := time.NewTimer(time.Duration(try) * settleRetryWait)
			select {
			case <-ctx.Done():
			case <-t.C:
			}
			t.Stop()
		}
	}
	return err
}

// WrittenBehind reports whether directory dirKey's root block has a
// version stored after one dated later (objstore.NotStates' late): one
// a write behind the directory's newest state left, which SettleDir
// refuses. One listing of the root block's history, one read of the
// directory's .version.
func (f *Filesystem) WrittenBehind(ctx context.Context, dirKey string) (bool, error) {
	ctx = undeclared(ctx)
	dates, err := objstore.ReadDates(ctx, f.store, dirKey)
	if err != nil {
		return false, err
	}
	h, err := f.store.History(ctx, dirKey)
	if err != nil {
		return false, err
	}
	_, late := objstore.NotStates(h, dates)
	return len(late) > 0, nil
}

// cutLeftoversOurs checks that the root block and .metadata versions
// stored after a cut settle's V0 (root and meta, each newest first) are
// that settle's own, as far as the directory's .version tells: before its
// commit none of its window's versions is dated there; after it, each
// key's are dated at exactly its states' times (times). A version dated
// otherwise is another writer's (a write made between the cut and this
// rerun, which the caller's exclusion forbids): ErrSettleStopped, and
// nothing is erased. An undated version is the settle's (its window
// before the commit, or a put whose answer was lost).
func cutLeftoversOurs(dirKey string, root, meta []objstore.Object, dates objstore.Dates, times []time.Time) error {
	for _, h := range [][]objstore.Object{root, meta} {
		var dated []time.Time
		for _, o := range h {
			if o.Deleted {
				continue
			}
			if t, ok := dates[o.VersionID]; ok {
				dated = append(dated, t)
			}
		}
		if len(dated) == 0 {
			continue
		}
		want := map[int64]int{}
		for _, t := range times {
			want[t.UnixNano()]++
		}
		ok := len(dated) == len(times)
		for _, t := range dated {
			if want[t.UnixNano()] == 0 {
				ok = false
				break
			}
			want[t.UnixNano()]--
		}
		if !ok {
			return fmt.Errorf("directory %s: a version stored after the cut settle's present is dated as none of its states (%d dated, %d states): written by another writer since: %w",
				dirKey, len(dated), len(times), ErrSettleStopped)
		}
	}
	return nil
}

// recordSum is a sum of what of a directory's .metadata is its own record
// (mode, owner, app data and origin, extended attributes, ...), its
// times and bookkeeping left out.
func recordSum(a Attr) (string, error) {
	a.Mtime, a.Ctime, a.Atime, a.Btime = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	a.Size, a.content, a.mtimeOverrideETag = 0, objstore.Object{}, ""
	a.Checksums, a.Refs, a.dated, a.Nlink = nil, nil, false, 0 // nlink follows the entries
	data, err := encodeFileMetadata(context.Background(), a)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:16]), nil
}

// settleProof checks, before anything is written, that the table holds
// what the settle may write and nothing else: every state from t0 on is
// the state as read there (the latest read at or before it) with exactly
// the changes whose [At, Until) holds it played in order on it, an add
// where its name is free, a removal of its name and item; and the last
// state is the directory's present. It is the table's arithmetic checked
// by other means, and the rule that a settle never puts into the past
// what no change says: a difference between the states read plus the
// changes and the present (a change not queued, a write not by this
// rule) is ErrSettleRefused.
func (f *Filesystem) settleProof(ctx context.Context, tab *objstore.SettleTable, changes []DirChange, t0 time.Time, present []block.Entry, presentAttr Attr) error {
	states, err := tab.States(ctx, t0)
	if err != nil {
		return err
	}
	for _, s := range states {
		base, err := tab.ReadEntriesAt(ctx, s.At)
		if err != nil {
			return err
		}
		want := map[string]objstore.SettleEntry{}
		for _, e := range base {
			want[e.Name] = e
		}
		for _, ch := range changes {
			if ch.At.After(s.At) || !ch.Until.After(s.At) {
				continue // a touch's Until is zero: never
			}
			switch ch.Op {
			case ChangeAdd:
				if _, taken := want[ch.Name]; !taken {
					want[ch.Name] = objstore.SettleEntry{Name: ch.Name, Item: ch.Item, Type: int(ch.Type)}
				}
			case ChangeRemove:
				if e, ok := want[ch.Name]; ok && e.Item == ch.Item {
					delete(want, ch.Name)
				}
			}
		}
		got, err := tab.EntriesAt(ctx, s.At)
		if err != nil {
			return err
		}
		if d := settleDiff(got, want); d != "" {
			return fmt.Errorf("the state at %s would be %s: %w", s.At.Format(time.RFC3339Nano), d, ErrSettleRefused)
		}
		// Its record: the one the state read there had (none read: a
		// directory a change made, which has no record before it).
		recData, recSum, err := tab.RecordAt(ctx, s.At)
		if err != nil {
			return err
		}
		if recData != nil {
			a, err := decodeFileMetadata(recData)
			if err != nil {
				return err
			}
			if sum, err := recordSum(a); err != nil || sum != recSum {
				return fmt.Errorf("the state at %s: its record is not the one read: %w", s.At.Format(time.RFC3339Nano), ErrSettleRefused)
			}
		}
	}
	if len(states) == 0 {
		return nil
	}
	last, err := tab.EntriesAt(ctx, states[len(states)-1].At)
	if err != nil {
		return err
	}
	now := map[string]objstore.SettleEntry{}
	for _, e := range toSettleEntries(present) {
		now[e.Name] = e
	}
	if d := settleDiff(last, now); d != "" {
		return fmt.Errorf("the states read and the changes give, as of %s, not the folder's present (it would be %s): %w",
			states[len(states)-1].At.Format(time.RFC3339Nano), d, ErrSettleRefused)
	}
	_, lastRec, err := tab.RecordAt(ctx, states[len(states)-1].At)
	if err != nil {
		return err
	}
	presentRec, err := recordSum(presentAttr)
	if err != nil {
		return err
	}
	if lastRec != "" && lastRec != presentRec {
		return fmt.Errorf("the states read give, as of %s, not the folder's own record as it is now: %w",
			states[len(states)-1].At.Format(time.RFC3339Nano), ErrSettleRefused)
	}
	return nil
}

// settleDiff says how got differs from want ("" for not at all), in a
// few names.
func settleDiff(got []objstore.SettleEntry, want map[string]objstore.SettleEntry) string {
	var diff []string
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.Name] {
			diff = append(diff, fmt.Sprintf("%q twice", e.Name))
		}
		seen[e.Name] = true
		if w, ok := want[e.Name]; !ok {
			diff = append(diff, fmt.Sprintf("with %q", e.Name))
		} else if w != e {
			diff = append(diff, fmt.Sprintf("with %q as item %s, not %s", e.Name, e.Item, w.Item))
		}
	}
	for name := range want {
		if !seen[name] {
			diff = append(diff, fmt.Sprintf("without %q", name))
		}
	}
	if len(diff) == 0 {
		return ""
	}
	sort.Strings(diff)
	if len(diff) > 5 {
		diff = append(diff[:5], fmt.Sprintf("and %d more", len(diff)-5))
	}
	return strings.Join(diff, ", ")
}

// settlePut is one put of the window: data as key's new version,
// conditional on ifMatch (the key's current version, as the settle last
// read or wrote it; none for a key whose newest is a deletion). A
// precondition that fails because the put was made and its answer lost
// finds the key's current version holding data, and takes that version
// for its own; one that finds anything else is another writer's version:
// ErrSettleStopped, and nothing more is written.
func (f *Filesystem) settlePut(ctx context.Context, key string, data []byte, ifMatch string, retried *bool) (*objstore.Object, error) {
	var obj *objstore.Object
	err := settleRetry(ctx, retried, func() error {
		o, err := f.store.Put(ctx, key, bytes.NewReader(data), nil, ifMatch)
		if err == nil {
			obj = o
			return nil
		}
		if !objstore.IsPreconditionFailed(err) {
			return err
		}
		body, cur, gerr := f.store.Get(undeclared(ctx), key)
		if gerr != nil {
			return gerr
		}
		have, gerr := io.ReadAll(body)
		body.Close()
		if gerr != nil {
			return gerr
		}
		if bytes.Equal(have, data) {
			obj = cur // ours, its answer lost
			return nil
		}
		return fmt.Errorf("%s's current version %s is not the one the settle read or wrote: %w", key, cur.VersionID, ErrSettleStopped)
	})
	return obj, err
}

// settleStoppedFirst ends a settle stopped at its window's first put,
// with nothing of the window written: its new blocks go (keys only it
// minted), and its caller is told the record can go, so that no rerun
// takes the other writer's version for the settle's own. stop is the
// settle's error.
func (f *Filesystem) settleStoppedFirst(ctx context.Context, create []string, rec SettleRecord, done func(SettleRecord) error, stop error) error {
	for _, k := range create {
		if err := settleRetry(ctx, nil, func() error { return f.store.EraseAll(ctx, k) }); err != nil {
			return errors.Join(stop, err)
		}
	}
	if done != nil {
		if err := done(rec); err != nil {
			return errors.Join(stop, err)
		}
	}
	return stop
}

// settlePurged ends a cut settle of a directory none of whose root block
// and .metadata is stored any more: the cut settle's new blocks go, and
// the caller is told the record can go.
func (f *Filesystem) settlePurged(ctx context.Context, cut SettleRecord, done func(SettleRecord) error) (Settled, error) {
	wctx := ctx
	for _, k := range cut.Create {
		if err := settleRetry(wctx, nil, func() error { return f.store.EraseAll(wctx, k) }); err != nil {
			return Settled{}, err
		}
	}
	if done != nil {
		if err := done(cut); err != nil {
			return Settled{}, err
		}
	}
	return Settled{}, nil
}

func toSettleEntries(entries []block.Entry) []objstore.SettleEntry {
	out := make([]objstore.SettleEntry, len(entries))
	for i, e := range entries {
		out[i] = objstore.SettleEntry{Name: e.Name, Item: e.UUID, Type: int(e.Type)}
	}
	return out
}

func fromSettleEntries(entries []objstore.SettleEntry) []block.Entry {
	out := make([]block.Entry, len(entries))
	for i, e := range entries {
		out[i] = block.Entry{Name: e.Name, UUID: e.Item, Type: EntryType(e.Type)}
	}
	return out
}

func sortedTimes(set map[time.Time]bool) []time.Time {
	out := make([]time.Time, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
