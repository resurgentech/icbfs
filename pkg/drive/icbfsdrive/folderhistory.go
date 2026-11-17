package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
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
	states, err := d.dirStates(ctx, n.key)
	if err != nil {
		return drive.FolderHistory{}, err
	}
	h := drive.FolderHistory{Version: version, States: []drive.FolderState{}}
	for _, s := range states {
		state := drive.FolderState{At: s.At, Entries: []drive.FolderEntry{}}
		for _, e := range s.Entries {
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

// dirStates is directory dirKey's states, oldest first: its entries as a
// view of each time one of its blocks changed (by effective date) sees
// them, states that changed nothing left out.
func (d *Drive) dirStates(ctx context.Context, dirKey string) ([]icbfs.DirState, error) {
	objs, err := d.fs.DirHistory(ctx, dirKey)
	if err != nil {
		return nil, mapErr(err)
	}
	var times []time.Time
	seen := map[time.Time]bool{}
	datesOf := map[string]objstore.Dates{}
	for i := range objs {
		o := &objs[i]
		base := objstore.BaseKey(o.Key)
		dates, ok := datesOf[base]
		if !ok {
			if dates, err = objstore.ReadDates(ctx, d.fs.Store(), base); err != nil {
				return nil, mapErr(err)
			}
			datesOf[base] = dates
		}
		if t := dates.Time(o).UTC(); !seen[t] {
			seen[t] = true
			times = append(times, t)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	var states []icbfs.DirState
	for _, t := range times {
		fsys, err := d.at(ctx, drive.At{Time: t})
		if errors.Is(err, drive.ErrNotFound) {
			continue // the filesystem didn't exist yet
		}
		if err != nil {
			return nil, err
		}
		entries, err := fsys.ReadDir(ctx, dirKey)
		if errors.Is(err, icbfs.ErrNotFound) {
			continue // nor the folder
		}
		if err != nil {
			return nil, mapErr(err)
		}
		if entries == nil {
			entries = []block.Entry{}
		}
		if last := len(states) - 1; last >= 0 && sameEntries(states[last].Entries, entries) {
			continue
		}
		states = append(states, icbfs.DirState{At: t, Entries: entries})
	}
	return states, nil
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

func sameEntries(a, b []block.Entry) bool {
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
func (d *Drive) ReplaceFolderHistory(ctx context.Context, ref drive.ItemRef, states []drive.FolderState, cond drive.Condition) (err error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done(&err)
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

var _ drive.FileHistorian = (*Drive)(nil)

// FileHistory is item's content versions, newest first by date.
func (d *Drive) FileHistory(ctx context.Context, ref drive.ItemRef) (drive.FileHistory, error) {
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return drive.FileHistory{}, err
	}
	if n.typ != icbfs.TypeFile {
		return drive.FileHistory{}, drive.ErrIsDir
	}
	if err := d.check(ctx, n, needRead); err != nil {
		return drive.FileHistory{}, err
	}
	versions, current, err := d.fs.FileHistory(ctx, n.key)
	if err != nil {
		return drive.FileHistory{}, mapErr(err)
	}
	h := drive.FileHistory{Version: current, Versions: []drive.Version{}}
	for _, v := range versions {
		h.Versions = append(h.Versions, drive.Version{ID: v.VersionID, Time: v.At, Stored: v.Stored, Size: v.Size, Current: v.VersionID == current,
			Checksums: toChecksums(n.attr.Checksums[v.ETag])})
	}
	return h, nil
}

// ReplaceFileHistory writes states as item's whole history.
func (d *Drive) ReplaceFileHistory(ctx context.Context, ref drive.ItemRef, states []drive.FileState, cond drive.Condition) (err error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done(&err)
	n, err := d.resolve(ctx, d.fs, ref)
	if err != nil {
		return err
	}
	if n.typ != icbfs.TypeFile {
		return drive.ErrIsDir
	}
	if err := d.check(ctx, n, needWrite); err != nil {
		return err
	}
	fileStates := make([]icbfs.FileState, len(states))
	for i, s := range states {
		attr := n.attr
		attr.Mtime = s.Info.Modified
		if !s.Info.Created.IsZero() {
			attr.Btime = s.Info.Created
		}
		if s.Info.Mode != nil {
			attr.Mode = *s.Info.Mode
		}
		if s.Info.WindowsAttrs != nil {
			attr.WindowsAttrs, attr.HasWindowsAttrs = *s.Info.WindowsAttrs, true
		}
		app, err := appData(s.Info.App, s.Info.Origin)
		if err != nil {
			return err
		}
		if len(app) > 0 {
			merged := make(map[string][]byte, len(attr.App)+len(app))
			for k, v := range attr.App {
				merged[k] = v
			}
			for k, v := range app {
				if v == nil {
					delete(merged, k)
				} else {
					merged[k] = v
				}
			}
			attr.App = merged
		}
		fileStates[i] = icbfs.FileState{At: s.At, Version: s.Version, Content: s.Content, Attr: &attr}
	}
	err = d.fs.ReplaceFileHistory(ctx, n.key, fileStates, cond.IfMatch)
	switch {
	case errors.Is(err, icbfs.ErrFileChanged):
		_, current, _ := d.fs.FileHistory(ctx, n.key)
		return &drive.PreconditionError{Current: current}
	case errors.Is(err, icbfs.ErrInvalidArgument):
		return fmt.Errorf("%w (%v)", drive.ErrInvalid, err)
	case err != nil:
		return mapErr(err)
	}
	d.views.lookAgain()
	d.changed(parentNode(n))
	return nil
}
