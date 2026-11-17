package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// Settling a directory: its states from its earliest late change on are
// written once, with every change in place, instead of the whole history
// once per change (ReplaceDirHistory).
//
//   - Build: the state just before the earliest change and every later
//     state are read (SettleInput.StateAt, through the history cache, late
//     and unlisted versions left out), one at a time, into the working
//     table as entry lifetimes (objstore.SettleTable). The requests are the
//     later states' blocks', whatever the directory's whole history.
//   - Play: each change, oldest first, is an insert or an end of a
//     lifetime in the table. No request.
//   - Write-out: the states from the first one that differs from what the
//     store has (known from the sums of the states read, not by reading
//     blocks back) are generated one at a time and written: every block
//     below a root first, under new keys no reader looks at; then each
//     state's .metadata and root block back to back, oldest first, two
//     requests a state and nothing between (the window in which a reader
//     of the present sees the directory as of an earlier state); then the
//     directory's .version written once (the commit: a version not in it
//     is an unfinished write); then what the new states supersede erased,
//     and every block below a root that the directory now doesn't reach
//     gets a deletion on top.
//
// From the window on, the settle doesn't stop with its caller's context,
// and a store error is tried again (settleTries) before it gives up.
//
// What a version the .version doesn't list is: in a directory whose
// .version lists any version, every version a write finished is listed
// (objstore.DateAll; the first record lists those from before), so an
// unlisted one is a write that never finished (a cut write-out's root and
// .metadata, after every listed one) or one a finished settle superseded
// and didn't get to erase (before the newest listed one). Neither is a
// state: a settle reads past them and erases them. So the directory's
// present, as a settle sees it, is its newest listed root version (with
// the blocks below as they are) and its newest listed .metadata: what it
// was before a cut write-out started, whatever the cut left on top.
//
// A settle cut short is finished by settling again with its changes (or
// any that include them) and its plan (SettleInput.Cut): the present is
// the one from before the cut, each change goes in at its time, and what
// the cut settle wrote that nothing reaches (its Create, before its
// commit) or superseded and didn't erase (its Erase, after) goes.

// settleTries is how many times a write-out request is made before the
// settle gives up.
const settleTries = 3

// settleRetryWait is the wait before a write-out request's next try,
// times the tries so far (a variable for the tests).
var settleRetryWait = 200 * time.Millisecond

// maxSettleAhead is how far past this machine's clock a change may be.
const maxSettleAhead = time.Second

// SettleInput is how SettleDir reads and where it keeps its work.
type SettleInput struct {
	// StateAt is the directory's entries as a view of t sees them, its
	// late and unlisted versions left out, and the keys of the blocks below
	// its root the view read; an error satisfying ErrNotFound if it didn't
	// exist then.
	StateAt func(ctx context.Context, t time.Time) ([]block.Entry, []string, error)
	// History is a key's history now, newest first (the history cache's,
	// kept for the views that StateAt reads through); nil for the store's.
	History func(ctx context.Context, key string) ([]objstore.Object, error)
	// Table is the settle's working table.
	Table *objstore.SettleTable
	// Plan, if set, is told what the settle will do before its first
	// write; an error from it stops the settle with nothing written.
	Plan func(SettlePlan) error
	// Cut, if set, is the plan of a settle of this directory that was cut
	// short (as Plan was told it): what it created or superseded that
	// nothing reaches now goes too.
	Cut *SettlePlan
	// Forget, if set, is given the keys whose history the settle changed,
	// once it's done: a cache's to drop.
	Forget func(ctx context.Context, keys []string) error
	// Keep, if set, is given each block and .metadata version the settle
	// wrote, with its content, after Forget: a cache's to keep.
	Keep func(ctx context.Context, o *objstore.Object, content []byte) error
}

// SettlePlan is what a settle will do (SettleInput.Plan).
type SettlePlan struct {
	From   time.Time // states before it are neither read, written nor erased
	States int       // states it writes
	Create []string  // the keys of the new blocks below the root
	// Erase is the versions it supersedes or finds unreachable, erased
	// after its commit; a VersionID "" is every version of Key (the
	// .version of a block erased whole).
	Erase                  []objstore.Object
	RootBefore, MetaBefore string // the root block's and .metadata's newest stored versions now
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
func ValidSettleTime(t time.Time) bool {
	return !t.IsZero() && !t.Before(time.Unix(0, 0)) && !t.After(time.Now().Add(maxSettleAhead))
}

// SettleDir writes directory dirKey's states from the earliest of changes
// on, with each change in place: in the state at its time and in every
// later one (an add where its name is free, the occupant keeping it; a
// removal of its name and item). Changes are played by time, and those
// of one time in the order given. The directory as it is now isn't
// changed: the last state written is it. Changes already settled write
// nothing. The caller makes no other change to the directory while it
// runs.
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
		if !ValidSettleTime(ch.At) {
			return Settled{}, fmt.Errorf("a change at %s: not a time a change can have: %w", ch.At.Format(time.RFC3339Nano), ErrInvalidArgument)
		}
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
	unlisted := objstore.Unlisted(rootHist, dates)
	for id := range objstore.Unlisted(metaHist, dates) {
		unlisted[id] = true
	}
	late := objstore.LateVersions(objstore.Listed(rootHist, dates), dates)
	for id := range objstore.LateVersions(objstore.Listed(metaHist, dates), dates) {
		late[id] = true
	}
	// Unfinished: unlisted and stored after the newest listed version (a
	// cut write-out's). The newest stored root must then be written again.
	unfinished := 0
	for _, h := range [][]objstore.Object{rootHist, metaHist} {
		for _, o := range h {
			if !unlisted[o.VersionID] {
				break
			}
			if !o.Deleted {
				unfinished++
			}
		}
	}
	times := map[time.Time]bool{}
	var tb time.Time
	for _, h := range [][]objstore.Object{rootHist, metaHist} {
		for i := range h {
			o := &h[i]
			if o.Deleted || late[o.VersionID] || unlisted[o.VersionID] {
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
		entries, keys, err := in.StateAt(ctx, t)
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
		_, err = tab.ReadState(ctx, t, toSettleEntries(entries))
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
	// The present, as it was before any cut write-out, and its blocks: a
	// late write may have split one into a key no dated state reaches.
	present, presentKeys, attr, err := f.settlePresentNow(ctx, dirKey, rootHist, metaHist, unlisted)
	if err != nil {
		return Settled{}, err
	}
	if err := tab.AddBlocks(ctx, objstore.SettlePresent, presentKeys...); err != nil {
		return Settled{}, err
	}

	// --- Play ---
	for _, ch := range changes {
		if err := tab.EnsureState(ctx, ch.At); err != nil {
			return Settled{}, err
		}
		switch ch.Op {
		case ChangeAdd:
			err = tab.Add(ctx, ch.At, objstore.SettleEntry{Name: ch.Name, Item: ch.Item, Type: int(ch.Type)})
		case ChangeRemove:
			err = tab.Remove(ctx, ch.At, ch.Name, ch.Item)
		case ChangeTouch:
		default:
			err = fmt.Errorf("change %q: %w", ch.Op, ErrInvalidArgument)
		}
		if err != nil {
			return Settled{}, err
		}
	}
	// The directory now is the last state. What differs between them that
	// no change says (a change not recorded) goes in from the earliest
	// change, as a late write's repair always did.
	if err := f.settlePresent(ctx, tab, t0, present); err != nil {
		return Settled{}, err
	}

	// --- Write-out: what to write ---
	gen, first, err := f.settleStates(ctx, tab, t0, tb)
	if err != nil {
		return Settled{}, err
	}
	if first < 0 && len(late)+unfinished > 0 {
		// Nothing differs, but the newest stored root isn't the last state
		// (a late one, or a cut write-out's): the last state goes on top.
		if len(gen) == 0 && !tb.IsZero() {
			if gen, _, err = f.settleStates(ctx, tab, tb, time.Time{}); err != nil {
				return Settled{}, err
			}
		}
		if len(gen) > 0 {
			first = len(gen) - 1
		}
	}
	var written []settleState
	var tw time.Time
	if first >= 0 {
		written = gen[first:]
		tw = written[0].at
	}
	writes := len(written) > 0

	// What goes: the root's and .metadata's versions from the first state
	// written on, every late or unlisted one; and of the blocks below the
	// roots, what no state reaches from now on (lowerErasures).
	var erase []objstore.Object
	var dropIDs []string
	for _, h := range [][]objstore.Object{rootHist, metaHist} {
		for i := range h {
			o := &h[i]
			if o.Deleted {
				continue
			}
			if late[o.VersionID] || unlisted[o.VersionID] || writes && !dates.Time(o).Before(tw) {
				erase = append(erase, objstore.Object{Key: o.Key, VersionID: o.VersionID})
				if !unlisted[o.VersionID] {
					dropIDs = append(dropIDs, o.VersionID)
				}
			}
		}
	}
	lower, err := f.lowerErasures(ctx, dirKey, tab, in.Cut, writes, tb, tw)
	if err != nil {
		return Settled{}, err
	}
	for _, le := range lower {
		for _, id := range le.ids {
			erase = append(erase, objstore.Object{Key: le.key, VersionID: id})
		}
		if le.whole {
			erase = append(erase, objstore.Object{Key: le.key + objstore.VersionSuffix})
		}
	}
	if !writes && len(erase) == 0 && !anyCover(lower) {
		return Settled{Read: read}, nil // the store has it all already
	}

	// New keys for the blocks below each written state's root, minted at
	// its time as writeDirTree's are.
	rec := &objstore.Recorder{}
	var create []string
	for _, s := range written {
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.at), rec)
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
	if in.Plan != nil {
		p := SettlePlan{From: t0, States: len(written), Create: create, Erase: erase}
		if !tb.IsZero() {
			p.From = tb
		}
		if len(rootHist) > 0 {
			p.RootBefore = rootHist[0].VersionID
		}
		if len(metaHist) > 0 {
			p.MetaBefore = metaHist[0].VersionID
		}
		if err := in.Plan(p); err != nil {
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
	var kept []put
	for _, s := range written {
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.at), rec)
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
				kept = append(kept, put{obj, data})
			}
		}
		rootData, err := root.Encode()
		if err != nil {
			return Settled{}, err
		}
		ma := attr
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
	for base, d := range rec.Dates() {
		if _, err := objstore.MergeDates(ctx, f.store, base, d, rec.Fresh(base)); err != nil {
			return Settled{}, err
		}
	}

	// From here on the settle goes to its end whether or not its caller
	// is still there: stopping inside the window leaves the present as of
	// an earlier state.
	wctx := context.WithoutCancel(ctx)

	// --- The window: each state's .metadata and root block, oldest
	// first, two requests a state and nothing else (a request tried again
	// is the same request) ---
	win := &objstore.Recorder{}
	for _, s := range written {
		rootData, metaData, err := tab.Encoded(wctx, s.at)
		if err != nil {
			return Settled{}, err
		}
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(wctx, s.at), win)
		var mobj, robj *objstore.Object
		if err := settleRetry(func() (err error) {
			mobj, err = f.store.Put(sctx, meta, bytes.NewReader(metaData), nil, "")
			return err
		}); err != nil {
			return Settled{}, err
		}
		if err := settleRetry(func() (err error) {
			robj, err = f.store.Put(objstore.AsBlock(sctx, dirKey), dirKey, bytes.NewReader(rootData), nil, "")
			return err
		}); err != nil {
			return Settled{}, err
		}
		kept = append(kept, put{mobj, metaData}, put{robj, rootData})
	}

	// --- The commit ---
	if writes {
		if err := settleRetry(func() error {
			_, err := objstore.UpdateDates(wctx, f.store, dirKey, win.Dates()[dirKey], dropIDs...)
			return err
		}); err != nil {
			return Settled{}, err
		}
	}

	// --- What the new states supersede, and what nothing reaches ---
	erased := 0
	for _, o := range erase {
		if o.Key != dirKey && o.Key != meta {
			continue
		}
		if err := settleRetry(func() error { return eraseGone(wctx, f.store, o.Key, o.VersionID) }); err != nil {
			return Settled{}, err
		}
		erased++
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
			if err := settleRetry(func() error {
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
	if err := f.markPast(wctx); err != nil {
		return Settled{}, err
	}

	// --- The cache: what changed goes, what was written is kept ---
	if in.Forget != nil {
		forget := []string{dirKey, meta, dirKey + objstore.VersionSuffix}
		for _, le := range lower {
			forget = append(forget, le.key, le.key+objstore.VersionSuffix)
		}
		blocks, err := tab.Blocks(wctx)
		if err != nil {
			return Settled{}, err
		}
		for _, b := range blocks {
			forget = append(forget, b.Key, b.Key+objstore.VersionSuffix)
		}
		if err := in.Forget(wctx, forget); err != nil {
			return Settled{}, err
		}
	}
	if in.Keep != nil {
		for _, p := range kept {
			if err := in.Keep(wctx, p.obj, p.data); err != nil {
				return Settled{}, err
			}
		}
	}
	return Settled{Read: read, Written: len(written), Erased: erased}, nil
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
			return nil, -1, fmt.Errorf("the state at %s: %w", s.At.Format(time.RFC3339Nano), err)
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

// settlePresentNow is the directory as a settle takes it to be now: its
// newest listed root version, the blocks below as they are, and its
// newest listed .metadata's attributes. Live, unless a cut write-out left
// unlisted versions on top.
func (f *Filesystem) settlePresentNow(ctx context.Context, dirKey string, rootHist, metaHist []objstore.Object, unlisted map[string]bool) ([]block.Entry, []string, Attr, error) {
	newest := func(h []objstore.Object) (*objstore.Object, bool) {
		for i := range h {
			if !unlisted[h[i].VersionID] {
				return &h[i], i == 0
			}
		}
		return nil, len(h) == 0
	}
	root, rootLive := newest(rootHist)
	mobj, metaLive := newest(metaHist)
	if rootLive && metaLive {
		entries, keys, err := f.ReadDirKeys(ctx, dirKey)
		if err != nil {
			return nil, nil, Attr{}, err
		}
		attr, err := f.statDir(ctx, dirKey)
		return entries, keys, attr, err
	}
	if root == nil || root.Deleted || mobj == nil || mobj.Deleted {
		return nil, nil, Attr{}, fmt.Errorf("directory %s has no finished version: %w", dirKey, ErrNotFound)
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
// settle's plan named. A version goes if no state reaches it from now
// on: none read at or after its date did (states before tb aren't read,
// and don't reach a version dated after tb), or, when the settle writes
// (from tw), it is dated from tw on or late; and an unlisted version,
// always. A block no state reaches any version of goes whole; one that
// the directory now doesn't reach gets a deletion on top (after writing,
// the present reaches only new keys).
func (f *Filesystem) lowerErasures(ctx context.Context, dirKey string, tab *objstore.SettleTable, cut *SettlePlan, writes bool, tb, tw time.Time) ([]lowerErase, error) {
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
	if cut != nil {
		add := func(k string) {
			if !seen[k] && objstore.BaseKey(k) == k {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		for _, k := range cut.Create {
			add(k)
		}
		for _, o := range cut.Erase {
			if o.Key != dirKey && o.Key != metadataObjectKey(dirKey) {
				add(o.Key)
			}
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
		unlisted := objstore.Unlisted(h, kd)
		late := objstore.LateVersions(objstore.Listed(h, kd), kd)
		le := lowerErase{key: k}
		live := 0
		for i := range h {
			o := &h[i]
			if o.Deleted {
				continue
			}
			live++
			t := kd.Time(o)
			gone := unlisted[o.VersionID] ||
				t.After(tb) && (b.Last.IsZero() || t.After(b.Last)) ||
				writes && inTable[k] && (late[o.VersionID] || !t.Before(tw))
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
		if err := settleRetry(func() error { return f.store.EraseAll(ctx, le.key) }); err != nil {
			return 0, err
		}
		return len(le.ids), nil
	}
	if le.cover {
		// No state from now on reaches the key: a deletion on top (before
		// erasing its newest version, which a store may refuse to erase
		// while it is current).
		if err := settleRetry(func() error {
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
		if err := settleRetry(func() error { return eraseGone(ctx, f.store, le.key, id) }); err != nil {
			return n, err
		}
		n++
	}
	if len(le.ids) > 0 {
		if err := settleRetry(func() error {
			_, err := objstore.UpdateDates(ctx, f.store, le.key, nil, le.ids...)
			return err
		}); err != nil {
			return n, err
		}
	}
	return n, nil
}

func anyCover(lower []lowerErase) bool {
	for _, le := range lower {
		if le.cover {
			return true
		}
	}
	return false
}

// eraseGone erases a version, which may be gone already.
func eraseGone(ctx context.Context, store objstore.Store, key, id string) error {
	if err := store.Erase(ctx, key, id); err != nil && !objstore.IsNotFound(err) {
		return err
	}
	return nil
}

// settleRetry makes a write-out request, and again on an error, up to
// settleTries in all.
func settleRetry(do func() error) error {
	var err error
	for try := 1; try <= settleTries; try++ {
		if err = do(); err == nil {
			return nil
		}
		if try < settleTries {
			time.Sleep(time.Duration(try) * settleRetryWait)
		}
	}
	return err
}

// settlePresent plays, at at, what the present has that the last state
// in tab doesn't, and the other way round: a removal of each entry it
// lacks or holds otherwise, an add of each entry it has.
func (f *Filesystem) settlePresent(ctx context.Context, tab *objstore.SettleTable, at time.Time, present []block.Entry) error {
	states, err := tab.States(ctx, at)
	if err != nil || len(states) == 0 {
		return err
	}
	last, err := tab.EntriesAt(ctx, states[len(states)-1].At)
	if err != nil {
		return err
	}
	now := toSettleEntries(present)
	if objstore.SettleSum(last) == objstore.SettleSum(now) {
		return nil
	}
	have := map[string]objstore.SettleEntry{}
	for _, e := range last {
		have[e.Name] = e
	}
	var added []objstore.SettleEntry
	nowNames := map[string]bool{}
	for _, e := range now {
		nowNames[e.Name] = true
		if h, ok := have[e.Name]; !ok || h != e {
			added = append(added, e)
		}
	}
	for name, e := range have {
		replaced := false
		for _, a := range added {
			replaced = replaced || a.Name == name && a != e
		}
		if !nowNames[name] || replaced {
			if err := tab.Remove(ctx, at, name, e.Item); err != nil {
				return err
			}
		}
	}
	for _, a := range added {
		if err := tab.Add(ctx, at, a); err != nil {
			return err
		}
	}
	return nil
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
