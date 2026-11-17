package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.LateSettler = (*Drive)(nil)

// MaxSettleChanges is how many late changes one settle plays at most:
// SettleFolder settles more in parts of this size. A settle's heap is a
// state's blocks and bookkeeping that grows with its states, not with the
// changes; the parts bound what one write-out writes, and so how long a
// window and a rerun take. A variable for the tests.
var MaxSettleChanges = 256

// onSettle, if set, is told of every settle the drive makes: for the
// tests.
var onSettle func(dir string, s drive.Settled)

// SettleFolder puts each late change into the folder's states in [At,
// Until), writing the folder's states from its earliest change on once
// (drive.LateSettler).
func (d *Drive) SettleFolder(ctx context.Context, changes []drive.LateChange, opts drive.SettleOptions) (drive.Settled, error) {
	if len(changes) == 0 {
		return drive.Settled{}, nil
	}
	// Every row is checked before anything is read: each refusal names its
	// row and its own fault.
	folder := changes[0].Folder
	dirChanges := make([]icbfs.DirChange, len(changes))
	for i, c := range changes {
		if c.Folder != folder {
			return drive.Settled{}, fmt.Errorf("changes of folders %s and %s in one settle: %w", folder, c.Folder, drive.ErrInvalid)
		}
		row := fmt.Sprintf("late change %d (%s %q at %s, until %s)", i, c.Op, c.Name, c.At.Format(time.RFC3339Nano), c.Until.Format(time.RFC3339Nano))
		if f := icbfs.SettleTimeFault(c.At); f != "" {
			return drive.Settled{}, fmt.Errorf("%s: its time (At) %s: %w", row, f, drive.ErrInvalid)
		}
		dc := icbfs.DirChange{Dir: folder, At: c.At, Name: c.Name, Item: c.Item}
		switch c.Op {
		case drive.LateAdd, drive.LateRemove:
			if f := icbfs.SettleTimeFault(c.Until); f != "" {
				return drive.Settled{}, fmt.Errorf("%s: its Until %s: %w", row, f, drive.ErrInvalid)
			}
			if !c.Until.After(c.At) {
				return drive.Settled{}, fmt.Errorf("%s: its Until is not after its time (At): %w", row, drive.ErrInvalid)
			}
			typ, ok := entryType(c.Type)
			switch {
			case !ok:
				return drive.Settled{}, fmt.Errorf("%s: its type %q is not a file, folder or link: %w", row, c.Type, drive.ErrInvalid)
			case c.Name == "":
				return drive.Settled{}, fmt.Errorf("%s: it names no entry: %w", row, drive.ErrInvalid)
			case c.Item == "":
				return drive.Settled{}, fmt.Errorf("%s: it names no item: %w", row, drive.ErrInvalid)
			}
			dc.Type, dc.Until = typ, c.Until
			dc.Op = icbfs.ChangeAdd
			if c.Op == drive.LateRemove {
				dc.Op = icbfs.ChangeRemove
			}
		case drive.LateTouch:
			dc.Op = icbfs.ChangeTouch
		default:
			return drive.Settled{}, fmt.Errorf("%s: its kind %q is none of add, remove, touch: %w", row, c.Op, drive.ErrInvalid)
		}
		dirChanges[i] = dc
	}
	if c := opts.Cut; c != nil && c.Folder != folder {
		return drive.Settled{}, fmt.Errorf("the record of a settle of folder %q settling %s: %w", c.Folder, folder, drive.ErrInvalid)
	}
	if opts.ProveOnly && opts.Cut != nil {
		return drive.Settled{}, fmt.Errorf("folder %s: a settle that only proves is not given the record of a cut one (SettleOptions.Cut): %w", folder, drive.ErrInvalid)
	}
	if opts.Path == "" && opts.Cut == nil {
		return drive.Settled{}, fmt.Errorf("folder %s: a settle is given the folder's path (SettleOptions.Path): %w", folder, drive.ErrInvalid)
	}
	if d.cfg.ReadOnly {
		return drive.Settled{}, errDriveReadOnly
	}
	if err := d.fs.Refresh(ctx); err != nil {
		return drive.Settled{}, mapErr(err)
	}
	if d.fs.Archived() {
		return drive.Settled{}, fmt.Errorf("the filesystem is archived: %w", drive.ErrReadOnly)
	}
	p, err := profile(ctx)
	if err != nil {
		return drive.Settled{}, err
	}
	// The folder is the directory at opts.Path with the folder's id; the
	// drive never searches for it (its caller holds where it is, and looks
	// for it as for any item when it isn't there). Anyone but the
	// library's own housekeeping needs its rights to write its history, as
	// ReplaceFolderHistory does. A settle with a record is finished by the
	// folder's key wherever it is, or isn't: a folder trashed or moved
	// while its settle was cut still holds the cut settle's writes, and is
	// finished like any other (only by the library's own housekeeping,
	// which holds such records).
	n, err := d.resolveToSettle(ctx, folder, opts.Path)
	switch {
	case err == nil:
		if n.typ != icbfs.TypeDir {
			return drive.Settled{}, drive.ErrNotDir
		}
		if !p.System {
			if err := d.check(ctx, n, needAddFile); err != nil {
				return drive.Settled{}, err
			}
			if err := d.check(ctx, n, needAddDir); err != nil {
				return drive.Settled{}, err
			}
		}
	case errors.Is(err, drive.ErrFolderNotAtPath) && opts.Cut != nil:
		if !p.System {
			return drive.Settled{}, fmt.Errorf("folder %s is not at %q, and only the library's own housekeeping finishes its settle by its key: %w", folder, opts.Path, drive.ErrForbidden)
		}
	default:
		return drive.Settled{}, err
	}
	var cut *icbfs.SettleRecord
	if c := opts.Cut; c != nil {
		cut = &icbfs.SettleRecord{Root: c.Root, Meta: c.Meta, Create: c.Create, Old: c.Old, Times: c.Times}
	}
	// Played by At, those of one At in the order given; settled in parts of
	// at most MaxSettleChanges, oldest first, each a settle of its own:
	// record, write-out, Done. A part settled already plays again as
	// nothing, so a rerun after a cut in any part is given every change
	// and Cut, which the first part takes. An immediate stop between parts
	// starts no other.
	order := make([]int, len(dirChanges))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return dirChanges[order[i]].At.Before(dirChanges[order[j]].At) })
	sorted := make([]icbfs.DirChange, len(order))
	for i, o := range order {
		sorted[i] = dirChanges[o]
	}
	var s drive.Settled
	settled := 0 // of sorted, the parts that finished
	for start := 0; start < len(sorted); start += MaxSettleChanges {
		if err = ctx.Err(); err != nil {
			break
		}
		end := min(start+MaxSettleChanges, len(sorted))
		ps, perr := d.settleDir(ctx, folder, sorted[start:end], opts, cut)
		s.Read, s.Written, s.Erased = s.Read+ps.Read, s.Written+ps.Written, s.Erased+ps.Erased
		cut = nil
		if err = perr; err != nil {
			break
		}
		settled = end
	}
	// Changes: the longest run of the changes as given, from the first,
	// that are all settled (all of them, given in order by At).
	done := make([]bool, len(changes))
	for _, o := range order[:settled] {
		done[o] = true
	}
	for s.Changes < len(done) && done[s.Changes] {
		s.Changes++
	}
	switch {
	case err == nil:
		return s, nil
	case errors.Is(err, icbfs.ErrSettleRefused):
		return s, fmt.Errorf("%s: %w", strings.TrimSuffix(err.Error(), ": "+icbfs.ErrSettleRefused.Error()), drive.ErrSettleRefused)
	case errors.Is(err, icbfs.ErrSettleStopped):
		stopped := fmt.Errorf("%s: %w", strings.TrimSuffix(err.Error(), ": "+icbfs.ErrSettleStopped.Error()), drive.ErrSettleStopped)
		if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
			// Stopped, then cut by the caller's stop while it cleaned up (ST-16):
			// both, so the caller sees its stop too.
			stopped = fmt.Errorf("%w (%w)", stopped, cerr)
		}
		return s, stopped
	case errors.Is(err, icbfs.ErrInvalidArgument) || errors.Is(err, icbfs.ErrExists):
		return s, fmt.Errorf("%w (%v)", drive.ErrInvalid, err)
	}
	return s, mapErr(err)
}

// FolderWrittenBehind reports whether the folder's root block has a
// version stored after one dated later (drive.LateSettler).
func (d *Drive) FolderWrittenBehind(ctx context.Context, folder drive.ItemRef) (bool, error) {
	n, err := d.resolve(ctx, d.fs, folder)
	if err != nil {
		return false, err
	}
	if n.typ != icbfs.TypeDir {
		return false, drive.ErrNotDir
	}
	behind, err := d.fs.WrittenBehind(ctx, n.key)
	return behind, mapErr(err)
}

// resolveToSettle is the folder a settle is of: the directory at path
// (drive-relative) whose id is folder. Nothing at path, or another item
// there, is drive.ErrFolderNotAtPath: the drive never searches its tree
// for the folder (its caller looks for it as for any item, without a
// walk). A failure to read the way to path is that failure, never "not
// there". No path (a settle with a record only) is "not there" too.
func (d *Drive) resolveToSettle(ctx context.Context, folder, path string) (*node, error) {
	if path == "" {
		return nil, fmt.Errorf("folder %s: no path given: %w", folder, drive.ErrFolderNotAtPath)
	}
	n, err := d.resolvePath(ctx, d.fs, path)
	switch {
	case err == nil && n.key == folder:
		return n, nil
	case err == nil:
		return nil, fmt.Errorf("folder %s: %q is another item (%s): %w", folder, path, n.key, drive.ErrFolderNotAtPath)
	case errors.Is(err, drive.ErrNotFound), errors.Is(err, drive.ErrNotDir):
		return nil, fmt.Errorf("folder %s: nothing at %q: %w", folder, path, drive.ErrFolderNotAtPath)
	}
	return nil, err
}

// lateChanges is changes as a write reports them: its changes to each
// folder numbered in order from 0.
func lateChanges(changes []icbfs.DirChange) []drive.LateChange {
	out := make([]drive.LateChange, len(changes))
	for i, c := range changes {
		op := drive.LateTouch
		switch c.Op {
		case icbfs.ChangeAdd:
			op = drive.LateAdd
		case icbfs.ChangeRemove:
			op = drive.LateRemove
		}
		lc := drive.LateChange{Folder: c.Dir, At: c.At, Until: c.Until, Seq: i, Op: op, Name: c.Name, Item: c.Item}
		if op != drive.LateTouch {
			lc.Type = typeName(c.Type)
		}
		out[i] = lc
	}
	return out
}

// settling is the drive's settles, one at a time: each reads states
// through one view filesystem and keeps its work in the history cache's
// database. Nothing of a settle is kept once it returns: the record of a
// settle under way is its caller's (drive.SettleOptions.Record).
type settling struct {
	mu   sync.Mutex
	view *swapStore
	fsys *icbfs.Filesystem
}

// swapStore is the store of the settle's view filesystem: the view of
// the state being read, swapped for each.
type swapStore struct{ objstore.Store }

// settleDir settles directory dirKey's changes (icbfs.SettleDir),
// reading its states through the drive's history cache, brought up to
// date first (what this drive changed of the past forgotten, the
// past-changed mark looked at), with what is not a state left out. A
// drive with no Config.History uses the temporary cache its views open
// (in Config.CacheDir), whose database holds the working table as it
// holds the views' histories. When it's done, the views forget the
// directory's keys and keep what it wrote. opts is the caller's (its
// Record, Done and ProveOnly), cut the record of a cut settle.
func (d *Drive) settleDir(ctx context.Context, dirKey string, changes []icbfs.DirChange, opts drive.SettleOptions, cut *icbfs.SettleRecord) (drive.Settled, error) {
	record, done := opts.Record, opts.Done
	d.settle.mu.Lock()
	defer d.settle.mu.Unlock()
	if len(changes) == 0 {
		return drive.Settled{}, nil
	}
	ctx = withWrittenKeys(objstore.WithoutDeclaredTime(ctx), nil)
	cache, err := d.views.fresh(ctx)
	if err != nil {
		return drive.Settled{}, err
	}
	tab, err := cache.SettleTable(ctx, dirKey)
	if err != nil {
		return drive.Settled{}, err
	}
	if d.settle.fsys == nil {
		d.settle.view = &swapStore{}
		d.settle.fsys = icbfs.New(d.settle.view, d.cfg.Filesystem)
	}
	var upToCache *objstore.HistoryCache
	stateAt := func(ctx context.Context, t time.Time, upTo map[string]string) ([]block.Entry, []string, []byte, error) {
		if upToCache == nil {
			upToCache = cache.UpTo(upTo)
		}
		asOf, err := objstore.NewAsOf(ctx, upToCache, t)
		if err != nil {
			return nil, nil, nil, err
		}
		d.settle.view.Store = asOf
		if err := d.settle.fsys.BootstrapViewOf(ctx, d.fs); err != nil {
			return nil, nil, nil, err
		}
		entries, keys, err := d.settle.fsys.ReadDirKeys(ctx, dirKey)
		if err != nil {
			return nil, nil, nil, err
		}
		rec, err := d.settle.fsys.DirRecord(ctx, dirKey)
		return entries, keys, rec, err
	}
	toDrive := func(r icbfs.SettleRecord) drive.SettleRecord {
		return drive.SettleRecord{Folder: dirKey, Root: r.Root, Meta: r.Meta,
			Create: append([]string(nil), r.Create...), Old: append([]string(nil), r.Old...),
			Times: append([]time.Time(nil), r.Times...)}
	}
	in := icbfs.SettleInput{
		StateAt: stateAt,
		History: cache.FreshHistory,
		Table:   tab,
		Cut:     cut,
		Forget: func(ctx context.Context, keys []string) error {
			d.views.forgetKeys(ctx, keys)
			return nil
		},
		Keep:      cache.Keep,
		ProveOnly: opts.ProveOnly,
	}
	if record != nil {
		in.Record = func(r icbfs.SettleRecord) error { return record(toDrive(r)) }
	}
	if done != nil {
		in.Done = func(r icbfs.SettleRecord) error { return done(toDrive(r)) }
	}
	s, err := d.fs.SettleDir(ctx, dirKey, changes, in)
	settled := drive.Settled{Read: s.Read, Written: s.Written, Erased: s.Erased}
	if err != nil {
		// What it changed before it stopped isn't known here: views look
		// again from scratch.
		d.views.lookAgain()
		return settled, err
	}
	if onSettle != nil && !opts.ProveOnly {
		onSettle(dirKey, settled)
	}
	return settled, nil
}

// reportLateFolders reports the late changes of a write in a source's
// time: on a drive that defers late folders, the directories it wrote
// into their present, dated at their newest state (icbfs.WithPresentDating,
// set by begin), each change with At its own time and Until that state's
// time, to the context's drive.LateChanges for the caller to settle
// (SettleFolder). Anywhere else a late change is refused before it is
// written (begin). A directory the write found written after its time only
// once it had written it (its .metadata put back on top by RecordDates:
// another writer dated it later meanwhile, which the caller's exclusion
// rules out) is an error: the change is in the directory's present, and
// its past lacks it.
func (d *Drive) reportLateFolders(ctx context.Context, rec *objstore.Recorder, restored map[string]bool, changes *icbfs.DirChanges) error {
	late := map[string]time.Time{}
	for _, w := range rec.Written() {
		if w.Dir == "" {
			continue
		}
		if t, ok := late[w.Dir]; !ok || w.At.Before(t) {
			late[w.Dir] = w.At
		}
	}
	var behind []string
	for dir := range late {
		// Every entry change writes the directory's .metadata (its mtime);
		// one written behind the directory's latest state had its current
		// .metadata put back on top (RecordDates).
		if restored[dir+".metadata"] {
			behind = append(behind, dir)
		}
	}
	if len(behind) > 0 {
		sort.Strings(behind)
		return fmt.Errorf("folder %s was dated after %s by another writer while this change was written; "+
			"the change is in the folder now, and its past does not show it: %w",
			behind[0], late[behind[0]].Format(time.RFC3339Nano), drive.ErrLateChange)
	}
	d.reportPresentDated(ctx, changes, func(string) bool { return true })
	return nil
}

// reportPresentDated reports to ctx's drive.LateChanges, on a drive that
// defers late folders, the changes of each directory the write wrote
// into its present dated at its newest state (icbfs.PresentDated) that
// which says holds it: At its own time, Until that state's time.
func (d *Drive) reportPresentDated(ctx context.Context, changes *icbfs.DirChanges, which func(dir string) bool) {
	presentDated := icbfs.PresentDated(ctx)
	report := drive.LateChangesFrom(ctx)
	if len(presentDated) == 0 || !d.cfg.DeferLateFolders || report == nil {
		return
	}
	dirs := make([]string, 0, len(presentDated))
	for dir := range presentDated {
		if which(dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		chs := changes.For(dir)
		for i := range chs {
			chs[i].Until = presentDated[dir]
		}
		report.Add(lateChanges(chs)...)
	}
}

// lateChangeErr is the drive's error for a late change refused before
// it was written.
func lateChangeErr(le *icbfs.LateChangeError) error {
	return fmt.Errorf("%s: %w", le.Error(), drive.ErrLateChange)
}
