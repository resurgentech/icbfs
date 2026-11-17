package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.LateSettler = (*Drive)(nil)

// onSettle, if set, is told of every settle the drive makes: for the
// tests.
var onSettle func(dir string, s drive.Settled)

// SettleFolder writes a folder's states from its earliest late change on,
// once, with every change in place (drive.LateSettler). changes are the
// folder's late changes not yet settled, all of them: what the folder
// now holds that its states don't, and no change says, goes into them
// from the earliest change, as a late write's own settle does.
func (d *Drive) SettleFolder(ctx context.Context, changes []drive.LateChange, opts drive.SettleOptions) (drive.Settled, error) {
	if len(changes) == 0 {
		return drive.Settled{}, nil
	}
	folder := changes[0].Folder
	for i, c := range changes {
		if c.Folder != folder {
			return drive.Settled{}, fmt.Errorf("changes of folders %s and %s in one settle: %w", folder, c.Folder, drive.ErrInvalid)
		}
		if !icbfs.ValidSettleTime(c.At) {
			return drive.Settled{}, fmt.Errorf("late change %d at %s: zero, before 1970 or in the future: %w", i, c.At.Format(time.RFC3339Nano), drive.ErrInvalid)
		}
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
	// The folder is a directory under the drive's root, whoever asks
	// (opts.Path, if given, is where it is looked for first); anyone but
	// the library's own housekeeping needs its rights to write its
	// history, as ReplaceFolderHistory does.
	n, err := d.resolveToSettle(ctx, folder, opts.Path)
	if err != nil {
		return drive.Settled{}, err
	}
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
	dirChanges := make([]icbfs.DirChange, len(changes))
	for i, c := range changes {
		dc := icbfs.DirChange{Dir: folder, At: c.At, Name: c.Name, Item: c.Item}
		switch c.Op {
		case drive.LateAdd, drive.LateRemove:
			typ, ok := entryType(c.Type)
			if !ok || c.Name == "" || c.Item == "" {
				return drive.Settled{}, fmt.Errorf("late change %d (%s %q, type %q): %w", i, c.Op, c.Name, c.Type, drive.ErrInvalid)
			}
			dc.Type = typ
			dc.Op = icbfs.ChangeAdd
			if c.Op == drive.LateRemove {
				dc.Op = icbfs.ChangeRemove
			}
		case drive.LateTouch:
			dc.Op = icbfs.ChangeTouch
		default:
			return drive.Settled{}, fmt.Errorf("late change %d: kind %q: %w", i, c.Op, drive.ErrInvalid)
		}
		dirChanges[i] = dc
	}
	var cut *icbfs.SettleRecord
	if c := opts.Cut; c != nil {
		if c.Folder != "" && c.Folder != folder {
			return drive.Settled{}, fmt.Errorf("the record of a settle of folder %s settling %s: %w", c.Folder, folder, drive.ErrInvalid)
		}
		cut = &icbfs.SettleRecord{Root: c.Root, Meta: c.Meta, Create: c.Create, Old: c.Old}
	}
	s, err := d.settleDir(ctx, folder, dirChanges, opts.Record, opts.Done, cut)
	if errors.Is(err, icbfs.ErrInvalidArgument) || errors.Is(err, icbfs.ErrExists) {
		return s, fmt.Errorf("%w (%v)", drive.ErrInvalid, err)
	}
	return s, mapErr(err)
}

// resolveToSettle is the folder a settle is of: a directory under the
// drive's root, looked for at path first. One that isn't found by a
// search of the whole tree that read every directory is gone
// (drive.ErrFolderGone): trashed or purged. A directory the search can't
// read fails the settle, rather than take the folder for gone.
func (d *Drive) resolveToSettle(ctx context.Context, folder, path string) (*node, error) {
	n, err := d.resolve(ctx, d.fs, drive.ItemRef{ID: folder, Path: path})
	if err == nil || !errors.Is(err, drive.ErrNotFound) {
		return n, err
	}
	paths, err := d.searchPaths(ctx, d.fs, folder, 1, true)
	if err != nil {
		return nil, mapErr(err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("folder %s: %w", folder, drive.ErrFolderGone)
	}
	d.idPaths.put(folder, paths[0])
	return d.resolvePath(ctx, d.fs, paths[0])
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
		lc := drive.LateChange{Folder: c.Dir, At: c.At, Seq: i, Op: op, Name: c.Name, Item: c.Item}
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
// directory's keys and keep what it wrote. record and done are the
// caller's (drive.SettleOptions), cut the record of a cut settle.
func (d *Drive) settleDir(ctx context.Context, dirKey string, changes []icbfs.DirChange, record, done func(drive.SettleRecord) error, cut *icbfs.SettleRecord) (drive.Settled, error) {
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
	var withoutLate *objstore.HistoryCache
	stateAt := func(ctx context.Context, t time.Time, upTo map[string]string) ([]block.Entry, []string, error) {
		if withoutLate == nil {
			withoutLate = cache.WithoutLate(upTo)
		}
		asOf, err := objstore.NewAsOf(ctx, withoutLate, t)
		if err != nil {
			return nil, nil, err
		}
		d.settle.view.Store = asOf
		if err := d.settle.fsys.BootstrapViewOf(ctx, d.fs); err != nil {
			return nil, nil, err
		}
		return d.settle.fsys.ReadDirKeys(ctx, dirKey)
	}
	toDrive := func(r icbfs.SettleRecord) drive.SettleRecord {
		return drive.SettleRecord{Folder: dirKey, Root: r.Root, Meta: r.Meta,
			Create: append([]string(nil), r.Create...), Old: append([]string(nil), r.Old...)}
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
		Keep: cache.Keep,
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
	if onSettle != nil {
		onSettle(dirKey, settled)
	}
	return settled, nil
}

// reportLateFolders reports the directories that a change in a source's
// time (recorded in rec and changes) changed behind their latest state:
// a late entry. The block just written holds the directory as it is now,
// with the change, but dated at the change's time: a view of then can't
// reach it (the root block of a split directory points elsewhere) and a
// view of later shows the blocks without it. The directory's states from
// the change on must be written again with it (SettleFolder): on a drive
// that defers late folders, the change goes to the context's
// drive.LateChanges for the caller to settle. Anywhere else such a change
// is refused before it is written (begin), and one found here was dated
// behind by another writer meanwhile: an error.
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
	dirs := make([]string, 0, len(late))
	for dir := range late {
		// Every entry change writes the directory's .metadata (its mtime);
		// one written behind the directory's latest state had its current
		// .metadata put back on top (RecordDates): that's a late change.
		if restored[dir+".metadata"] {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	report := drive.LateChangesFrom(ctx)
	for _, dir := range dirs {
		chs := changes.For(dir)
		if len(chs) == 0 {
			chs = []icbfs.DirChange{{Dir: dir, At: late[dir], Op: icbfs.ChangeTouch}}
		}
		if d.cfg.DeferLateFolders && report != nil {
			report.Add(lateChanges(chs)...)
			continue
		}
		// Refused before it was written (icbfs.RefuseLateChanges), unless
		// another writer dated the folder later in the meantime.
		return fmt.Errorf("folder %s was dated after %s by another writer while this change was written; "+
			"the change is in the folder now, and its past does not show it: %w",
			dir, late[dir].Format(time.RFC3339Nano), drive.ErrLateChange)
	}
	return nil
}

// lateChangeErr is the drive's error for a late change refused before
// it was written.
func lateChangeErr(le *icbfs.LateChangeError) error {
	return fmt.Errorf("%s: %w", le.Error(), drive.ErrLateChange)
}
