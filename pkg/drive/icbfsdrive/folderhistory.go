package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.FolderHistorian = (*Drive)(nil)

// FolderHistory is dir's history: its state at each time one of its
// blocks changed, as a view of then sees it, with what doesn't change
// the entries left out.
func (d *Drive) FolderHistory(ctx context.Context, ref drive.ItemRef) (drive.FolderHistory, error) {
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return drive.FolderHistory{}, err
	}
	if n.typ != icbfs.TypeDir {
		return drive.FolderHistory{}, drive.ErrNotDir
	}
	if err := d.check(ctx, n, needRead); err != nil {
		return drive.FolderHistory{}, err
	}
	version, err := d.fs.DirVersion(ctx, n.key)
	if err != nil {
		return drive.FolderHistory{}, mapErr(err)
	}
	objs, err := d.fs.DirHistory(ctx, n.key)
	if err != nil {
		return drive.FolderHistory{}, mapErr(err)
	}
	if err := d.versionTimes(ctx, objs); err != nil {
		return drive.FolderHistory{}, mapErr(err)
	}
	var times []time.Time
	seen := map[time.Time]bool{}
	for _, o := range objs {
		if t := o.Time().UTC(); !seen[t] {
			seen[t] = true
			times = append(times, t)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	h := drive.FolderHistory{Version: version, States: []drive.FolderState{}}
	for _, t := range times {
		fsys, err := d.at(ctx, drive.At{Time: t})
		if errors.Is(err, drive.ErrNotFound) {
			continue // the filesystem didn't exist yet
		}
		if err != nil {
			return drive.FolderHistory{}, err
		}
		entries, err := fsys.ReadDir(ctx, n.key)
		if errors.Is(err, icbfs.ErrNotFound) {
			continue // nor the folder
		}
		if err != nil {
			return drive.FolderHistory{}, mapErr(err)
		}
		state := drive.FolderState{At: t, Entries: []drive.FolderEntry{}}
		for _, e := range entries {
			if visible(e.Type) {
				state.Entries = append(state.Entries, drive.FolderEntry{Name: e.Name, ID: e.UUID, Type: typeName(e.Type)})
			}
		}
		if last := len(h.States) - 1; last >= 0 && sameState(h.States[last].Entries, state.Entries) {
			continue
		}
		h.States = append(h.States, state)
	}
	return h, nil
}

func sameState(a, b []drive.FolderEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ReplaceFolderHistory writes states as dir's whole history.
func (d *Drive) ReplaceFolderHistory(ctx context.Context, ref drive.ItemRef, states []drive.FolderState, cond drive.Condition) error {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return err
	}
	if n.typ != icbfs.TypeDir {
		return drive.ErrNotDir
	}
	if err := d.check(ctx, n, needAddFile); err != nil {
		return err
	}
	if err := d.check(ctx, n, needAddDir); err != nil {
		return err
	}
	dirStates := make([]icbfs.DirState, len(states))
	for i, s := range states {
		ds := icbfs.DirState{At: s.At}
		for _, e := range s.Entries {
			typ, ok := entryType(e.Type)
			if !ok {
				return fmt.Errorf("state %d, %q: type %q: %w", i, e.Name, e.Type, drive.ErrInvalid)
			}
			ds.Entries = append(ds.Entries, block.Entry{Name: e.Name, UUID: e.ID, Type: typ})
		}
		dirStates[i] = ds
	}
	err = d.fs.ReplaceDirHistory(ctx, n.key, dirStates, cond.IfMatch)
	switch {
	case errors.Is(err, icbfs.ErrDirChanged):
		current, _ := d.fs.DirVersion(ctx, n.key)
		return &drive.PreconditionError{Current: current}
	case errors.Is(err, icbfs.ErrInvalidArgument):
		return fmt.Errorf("%w (%v)", drive.ErrInvalid, err)
	case err != nil:
		return mapErr(err)
	}
	d.views.lookAgain()
	d.changed(n)
	return nil
}

// entryType is the icbfs type of a drive item type.
func entryType(t string) (icbfs.EntryType, bool) {
	switch t {
	case drive.TypeFile:
		return icbfs.TypeFile, true
	case drive.TypeDir:
		return icbfs.TypeDir, true
	case drive.TypeSymlink:
		return icbfs.TypeSymlink, true
	}
	return 0, false
}
