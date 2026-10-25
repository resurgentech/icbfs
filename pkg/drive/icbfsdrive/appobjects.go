package icbfsdrive

import (
	"context"
	"errors"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.AppObjects = (*Drive)(nil)

// appObjectAccess checks the actor may read or write the drive's root:
// an app object is about the whole drive.
func (d *Drive) appObjectAccess(ctx context.Context, name string, write bool) error {
	if err := drive.CheckAppKey(name); err != nil {
		return err
	}
	root, err := d.rootNode(ctx, d.fs)
	if err != nil {
		return err
	}
	what := needRead
	if write {
		what = needAddFile
	}
	return d.check(ctx, root, what)
}

// AppObject is app object name's content.
func (d *Drive) AppObject(ctx context.Context, name string) ([]byte, error) {
	if err := d.appObjectAccess(ctx, name, false); err != nil {
		return nil, err
	}
	data, _, err := d.fs.GetAppObject(ctx, name)
	if err != nil {
		return nil, mapErr(err)
	}
	return data, nil
}

// SetAppObject writes app object name.
func (d *Drive) SetAppObject(ctx context.Context, name string, data []byte) error {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := d.appObjectAccess(ctx, name, true); err != nil {
		return err
	}
	if _, err := d.fs.PutAppObject(ctx, name, data); err != nil {
		if errors.Is(err, icbfs.ErrFileTooLarge) {
			return drive.ErrTooLarge
		}
		return mapErr(err)
	}
	return nil
}

// DeleteAppObject deletes app object name.
func (d *Drive) DeleteAppObject(ctx context.Context, name string) error {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := d.appObjectAccess(ctx, name, true); err != nil {
		return err
	}
	return mapErr(d.fs.DeleteAppObject(ctx, name))
}

// ListAppObjects lists the app objects named with prefix.
func (d *Drive) ListAppObjects(ctx context.Context, prefix string) ([]string, error) {
	root, err := d.rootNode(ctx, d.fs)
	if err != nil {
		return nil, err
	}
	if err := d.check(ctx, root, needRead); err != nil {
		return nil, err
	}
	names, err := d.fs.ListAppObjects(ctx, prefix)
	if err != nil {
		return nil, mapErr(err)
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}
