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
	n, err := d.resolve(ctx, d.fs, drive.ItemRef{ID: folder, Path: opts.Path})
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
	var cut *icbfs.SettlePlan
	if opts.Cut != nil {
		cut = &icbfs.SettlePlan{From: opts.Cut.From, States: opts.Cut.States, Create: opts.Cut.Create,
			RootBefore: opts.Cut.RootBefore, MetaBefore: opts.Cut.MetaBefore}
		for _, v := range opts.Cut.Erase {
			cut.Erase = append(cut.Erase, objstore.Object{Key: v.Key, VersionID: v.Version})
		}
	}
	s, err := d.settleDir(ctx, folder, dirChanges, opts.Plan, cut, false)
	if errors.Is(err, icbfs.ErrInvalidArgument) || errors.Is(err, icbfs.ErrExists) {
		return s, fmt.Errorf("%w (%v)", drive.ErrInvalid, err)
	}
	return s, mapErr(err)
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
// database. pending is the settles that gave up after they began to
// write (or, for a write's own settle, at all): each is finished before
// the drive's next change (begin), which is refused if it can't be.
type settling struct {
	mu   sync.Mutex
	view *swapStore
	fsys *icbfs.Filesystem

	pmu     sync.Mutex
	pending map[string]*pendingSettle
}

// pendingSettle is a folder's settle that gave up: its changes, and its
// plan if it got as far as one (what it may have written).
type pendingSettle struct {
	changes []icbfs.DirChange
	cut     *icbfs.SettlePlan
}

// remember keeps a settle that gave up, with any of the folder's already
// kept.
func (s *settling) remember(dir string, changes []icbfs.DirChange, cut *icbfs.SettlePlan) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if s.pending == nil {
		s.pending = map[string]*pendingSettle{}
	}
	p := s.pending[dir]
	if p == nil {
		p = &pendingSettle{}
		s.pending[dir] = p
	}
	p.changes = mergeChanges(p.changes, changes)
	p.cut = mergePlans(p.cut, cut)
}

// take is the folder's kept settle, which is no longer kept.
func (s *settling) take(dir string) *pendingSettle {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	p := s.pending[dir]
	delete(s.pending, dir)
	return p
}

// finishPending settles every folder whose settle gave up, before the
// drive changes anything else: a change into such a folder would build
// on a present the cut left (a past state), and make it the present for
// good.
func (d *Drive) finishPending(ctx context.Context) error {
	d.settle.pmu.Lock()
	dirs := make([]string, 0, len(d.settle.pending))
	for dir := range d.settle.pending {
		dirs = append(dirs, dir)
	}
	d.settle.pmu.Unlock()
	sort.Strings(dirs)
	for _, dir := range dirs {
		// settleDir takes the folder's kept settle in with it.
		if _, err := d.settleDir(ctx, dir, nil, nil, nil, true); err != nil {
			return fmt.Errorf("finishing the settle of folder %s that was cut: %w", dir, err)
		}
	}
	return nil
}

// mergeChanges is a then b, without b's changes a already has.
func mergeChanges(a, b []icbfs.DirChange) []icbfs.DirChange {
	have := map[icbfs.DirChange]bool{}
	for _, c := range a {
		have[c] = true
	}
	out := append([]icbfs.DirChange(nil), a...)
	for _, c := range b {
		if !have[c] {
			have[c] = true
			out = append(out, c)
		}
	}
	return out
}

// mergePlans is what two cut settles' plans name together.
func mergePlans(a, b *icbfs.SettlePlan) *icbfs.SettlePlan {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	m := &icbfs.SettlePlan{From: a.From, States: max(a.States, b.States), RootBefore: a.RootBefore, MetaBefore: a.MetaBefore}
	if b.From.Before(m.From) {
		m.From = b.From
	}
	m.Create = append(append([]string(nil), a.Create...), b.Create...)
	m.Erase = append(append([]objstore.Object(nil), a.Erase...), b.Erase...)
	return m
}

// swapStore is the store of the settle's view filesystem: the view of
// the state being read, swapped for each.
type swapStore struct{ objstore.Store }

// settleDir settles directory dirKey's changes (icbfs.SettleDir), with
// any settle of it that gave up before, reading its states through the
// drive's history cache, brought up to date first (what this drive
// changed of the past forgotten, the past-changed mark looked at), with
// late and unlisted versions left out. A drive with no Config.History
// uses the temporary cache its views open (in Config.CacheDir), whose
// database holds the working table as it holds the views' histories.
// When it's done, the views forget the directory's keys and keep what
// it wrote. If it gives up, it is kept to be finished before the drive's
// next change (finishPending): always for a write's own settle (own),
// whose changes nobody else holds; for a caller's (SettleFolder) once it
// may have written, the caller holding the changes until then.
func (d *Drive) settleDir(ctx context.Context, dirKey string, changes []icbfs.DirChange, plan func(drive.SettlePlan) error, cut *icbfs.SettlePlan, own bool) (drive.Settled, error) {
	d.settle.mu.Lock()
	defer d.settle.mu.Unlock()
	if p := d.settle.take(dirKey); p != nil {
		changes = mergeChanges(p.changes, changes)
		cut = mergePlans(p.cut, cut)
	}
	if len(changes) == 0 {
		return drive.Settled{}, nil
	}
	// Not a write's own: the settle forgets and keeps for itself.
	ctx = withWrittenKeys(objstore.WithoutDeclaredTime(ctx), nil)
	cache, err := d.views.fresh(ctx)
	if err != nil {
		if own {
			d.settle.remember(dirKey, changes, cut)
		}
		return drive.Settled{}, err
	}
	tab, err := cache.SettleTable(ctx, dirKey)
	if err != nil {
		if own {
			d.settle.remember(dirKey, changes, cut)
		}
		return drive.Settled{}, err
	}
	if d.settle.fsys == nil {
		d.settle.view = &swapStore{}
		d.settle.fsys = icbfs.New(d.settle.view, d.cfg.Filesystem)
	}
	withoutLate := cache.WithoutLate()
	stateAt := func(ctx context.Context, t time.Time) ([]block.Entry, []string, error) {
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
	var planned *icbfs.SettlePlan
	in := icbfs.SettleInput{
		StateAt: stateAt,
		History: cache.FreshHistory,
		Table:   tab,
		Cut:     cut,
		Plan: func(p icbfs.SettlePlan) error {
			if plan != nil {
				dp := drive.SettlePlan{Folder: dirKey, From: p.From, States: p.States, Create: p.Create,
					RootBefore: p.RootBefore, MetaBefore: p.MetaBefore, Erase: make([]drive.StoreVersion, len(p.Erase))}
				for i, o := range p.Erase {
					dp.Erase[i] = drive.StoreVersion{Key: o.Key, Version: o.VersionID}
				}
				if err := plan(dp); err != nil {
					return err
				}
			}
			planned = &p
			return nil
		},
		Forget: func(ctx context.Context, keys []string) error {
			d.views.forgetKeys(ctx, keys)
			return nil
		},
		Keep: cache.Keep,
	}
	s, err := d.fs.SettleDir(ctx, dirKey, changes, in)
	settled := drive.Settled{Read: s.Read, Written: s.Written, Erased: s.Erased}
	if err != nil {
		if own || planned != nil {
			d.settle.remember(dirKey, changes, mergePlans(cut, planned))
		}
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

// settleLateFolders settles the directories that a change in a source's
// time (recorded in rec and changes) changed behind their latest state:
// a late entry. The block just written holds the directory as it is now,
// with the change, but dated at the change's time: a view of then can't
// reach it (the root block of a split directory points elsewhere) and a
// view of later shows the blocks without it. So the directory's states
// from the change on are written again with it (settleDir), or, on a
// drive that defers late folders, the change goes to the context's
// drive.LateChanges for the caller to settle (SettleFolder).
func (d *Drive) settleLateFolders(ctx context.Context, rec *objstore.Recorder, restored map[string]bool, changes *icbfs.DirChanges) error {
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
	ctx = objstore.WithoutDeclaredTime(ctx)
	for _, dir := range dirs {
		chs := changes.For(dir)
		if len(chs) == 0 {
			chs = []icbfs.DirChange{{Dir: dir, At: late[dir], Op: icbfs.ChangeTouch}}
		}
		if d.cfg.DeferLateFolders && report != nil {
			report.Add(lateChanges(chs)...)
			continue
		}
		if _, err := d.settleDir(ctx, dir, chs, nil, nil, true); err != nil {
			return fmt.Errorf("settle folder %s with a change at %s: %w", dir, late[dir].Format(time.RFC3339), err)
		}
	}
	return nil
}
