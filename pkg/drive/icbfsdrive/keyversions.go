package icbfsdrive

import (
	"context"
	"errors"
	"sync"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.KeyVersioner = (*Drive)(nil)

// VersionsOf is the versions of file it as Versions gives them, less the
// Names (drive.KeyVersioner). For a profile that may read anything it
// reads them by its key alone, the file's history, its dates and its
// attributes at once: one round trip, where Versions first finds the
// file by its path, a request or two a folder down. A file deleted since
// is the trash's, as in Versions. Any other profile is checked as
// Versions checks it, finding the file by its path.
func (d *Drive) VersionsOf(ctx context.Context, it drive.Item) ([]drive.Version, error) {
	if it.Type != drive.TypeFile {
		return nil, drive.ErrIsDir
	}
	p, err := profile(ctx)
	if err != nil {
		return nil, err
	}
	if !p.System || it.ID == "" {
		return d.versions(ctx, drive.ItemRef{ID: it.ID, Path: it.Path}, false)
	}
	var (
		wg       sync.WaitGroup
		objs     []objstore.Object
		dates    objstore.Dates
		attr     icbfs.Attr
		histErr  error
		datesErr error
		statErr  error
	)
	wg.Go(func() { objs, histErr = d.fs.Store().History(ctx, it.ID) })
	wg.Go(func() { dates, datesErr = objstore.ReadDates(ctx, d.fs.Store(), it.ID) })
	attr, statErr = d.fs.Stat(ctx, it.ID, icbfs.TypeFile)
	wg.Wait()
	sums, refs := attr.Checksums, attr.Refs
	if statErr != nil {
		statErr = mapErr(statErr)
		if !errors.Is(statErr, drive.ErrNotFound) {
			return nil, statErr
		}
		// Gone since the walk: in the trash, its versions are still the
		// file's, as Versions finds them; else there is no such file.
		if _, err := d.trashedItem(ctx, it.ID); err != nil {
			return nil, statErr
		}
		sums = nil
		refs, _ = d.fs.ContentRefs(ctx, it.ID) // a trashed file's: best effort, as its checksums are
	}
	if histErr != nil {
		return nil, mapErr(histErr)
	}
	if datesErr != nil {
		return nil, mapErr(datesErr)
	}
	return versionList(objs, dates, sums, refs, nil), nil
}
