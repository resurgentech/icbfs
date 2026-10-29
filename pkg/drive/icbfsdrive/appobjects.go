package icbfsdrive

import (
	"context"
	"errors"
	"fmt"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.ConditionalAppObjects = (*Drive)(nil)

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

// AppObjectVersion is AppObject with the version read, for a
// SetAppObjectIf or DeleteAppObjectIf conditioned on it.
func (d *Drive) AppObjectVersion(ctx context.Context, name string) ([]byte, string, error) {
	if err := d.appObjectAccess(ctx, name, false); err != nil {
		return nil, "", err
	}
	data, obj, err := d.fs.GetAppObject(ctx, name)
	if err != nil {
		return nil, "", mapErr(err)
	}
	return data, obj.VersionID, nil
}

// SetAppObject writes app object name.
func (d *Drive) SetAppObject(ctx context.Context, name string, data []byte) error {
	_, err := d.SetAppObjectIf(ctx, name, data, drive.Condition{})
	return err
}

// SetAppObjectIf writes app object name when cond holds, and returns the
// version written. As a file's conditional write: the version in
// IfMatch is checked against the current one, then the store's own
// compare-and-swap on that version makes the write itself conditional,
// so a writer that slips in between is caught too. A condition that
// doesn't hold is a *drive.PreconditionError with the current version
// ("" when there is no object).
func (d *Drive) SetAppObjectIf(ctx context.Context, name string, data []byte, cond drive.Condition) (version string, err error) {
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return "", err
	}
	defer done(&err)
	if err := d.appObjectAccess(ctx, name, true); err != nil {
		return "", err
	}
	ifMatch := "" // the store's condition: an ETag
	if cond.IfMatch != "" {
		cur, err := d.fs.HeadAppObject(ctx, name)
		if err != nil {
			if errors.Is(err, icbfs.ErrNotFound) {
				return "", &drive.PreconditionError{}
			}
			return "", mapErr(err)
		}
		if cur.VersionID != cond.IfMatch {
			return "", &drive.PreconditionError{Current: cur.VersionID}
		}
		ifMatch = cur.ETag
	}
	obj, err := d.fs.PutAppObjectIf(ctx, name, data, ifMatch, cond.IfNoneMatch)
	if err != nil {
		switch {
		case errors.Is(err, icbfs.ErrFileTooLarge):
			return "", drive.ErrTooLarge
		case objstore.IsPreconditionFailed(err):
			return "", d.appObjectPrecondition(ctx, name)
		}
		return "", mapErr(err)
	}
	return obj.VersionID, nil
}

// DeleteAppObject deletes app object name.
func (d *Drive) DeleteAppObject(ctx context.Context, name string) error {
	return d.DeleteAppObjectIf(ctx, name, drive.Condition{})
}

// DeleteAppObjectIf deletes app object name when cond holds: IfMatch, the
// version it still has to be. IfNoneMatch is meaningless for a delete and
// ErrInvalid. An object that isn't that version, or is gone, is a
// *drive.PreconditionError with the current version ("" when there is no
// object), as SetAppObjectIf answers; only an unconditional delete of
// nothing is ErrNotFound. Unlike SetAppObjectIf's write, the delete
// itself isn't conditional: the store has no delete-if-match, so the
// version is checked and then the object deleted, and a write that lands
// between the two is deleted with it (it stays in the object's history).
// Best-effort there, as locks are.
func (d *Drive) DeleteAppObjectIf(ctx context.Context, name string, cond drive.Condition) (err error) {
	if cond.IfNoneMatch {
		return fmt.Errorf("delete app object %q: IfNoneMatch: %w", name, drive.ErrInvalid)
	}
	ctx, done, err := d.begin(ctx)
	if err != nil {
		return err
	}
	defer done(&err)
	if err := d.appObjectAccess(ctx, name, true); err != nil {
		return err
	}
	if cond.IfMatch != "" {
		cur, err := d.fs.HeadAppObject(ctx, name)
		if err != nil {
			if errors.Is(err, icbfs.ErrNotFound) {
				return &drive.PreconditionError{}
			}
			return mapErr(err)
		}
		if cur.VersionID != cond.IfMatch {
			return &drive.PreconditionError{Current: cur.VersionID}
		}
	}
	return mapErr(d.fs.DeleteAppObject(ctx, name))
}

// appObjectPrecondition is the error for a conditional write of name the
// store refused: a PreconditionError with the version there is now.
func (d *Drive) appObjectPrecondition(ctx context.Context, name string) error {
	e := &drive.PreconditionError{}
	if cur, err := d.fs.HeadAppObject(ctx, name); err == nil {
		e.Current = cur.VersionID
	}
	return e
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

// AppObjectVersions lists app object name's versions, newest first. A
// deletion has no content and isn't one; Current is the newest unless
// the object is deleted now.
func (d *Drive) AppObjectVersions(ctx context.Context, name string) ([]drive.Version, error) {
	if err := d.appObjectAccess(ctx, name, false); err != nil {
		return nil, err
	}
	hist, err := d.fs.AppObjectHistory(ctx, name)
	if err != nil {
		return nil, mapErr(err)
	}
	out := []drive.Version{}
	for i, o := range hist {
		if o.Deleted {
			continue
		}
		out = append(out, drive.Version{ID: o.VersionID, Time: o.LastModified, Stored: o.LastModified, Size: o.Size, Current: i == 0})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("app object %q: %w", name, drive.ErrNotFound)
	}
	return out, nil
}

// AppObjectAt is the content of one version of app object name.
func (d *Drive) AppObjectAt(ctx context.Context, name, version string) ([]byte, error) {
	if err := d.appObjectAccess(ctx, name, false); err != nil {
		return nil, err
	}
	data, err := d.fs.GetAppObjectVersion(ctx, name, version)
	if err != nil {
		return nil, mapErr(err)
	}
	return data, nil
}
