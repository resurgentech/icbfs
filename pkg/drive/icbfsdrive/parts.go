package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// Uploads in parts (T82-10, drive.PartWriter): the parts are staged by
// the filesystem (icbfs uploads.go), outside the tree; CompleteUpload is
// Write with the staged parts as its source (writeSource.upload), so
// everything Write does about the file (resolve, permissions, locks, the
// condition, checksums, the .metadata, events) is the same.

var _ drive.PartWriter = (*Drive)(nil)

func (d *Drive) UploadPartSize() int64 { return icbfs.UploadPartSize }
func (d *Drive) MaxUploadParts() int   { return icbfs.MaxUploadParts }

// uploadsWritable: the drive takes writes now.
func (d *Drive) uploadsWritable(ctx context.Context) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	if err := d.fs.Refresh(ctx); err != nil {
		return mapErr(err)
	}
	if d.fs.Archived() {
		return fmt.Errorf("the filesystem is archived: %w", drive.ErrReadOnly)
	}
	return nil
}

// uploadErr is err as the drive's errors say it.
func uploadErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, icbfs.ErrUploadOrder):
		return fmt.Errorf("%w (%v)", drive.ErrUploadOrder, err)
	case errors.Is(err, icbfs.ErrUploadParts):
		return fmt.Errorf("%w (%v)", drive.ErrUploadParts, err)
	case errors.Is(err, icbfs.ErrUploadsUnsupported):
		return fmt.Errorf("%w (%v)", drive.ErrNotSupported, err)
	case errors.Is(err, icbfs.ErrInvalidArgument):
		return fmt.Errorf("%w (%v)", drive.ErrInvalid, err)
	}
	return mapErr(err)
}

func (d *Drive) StartUpload(ctx context.Context, id string, record []byte) error {
	if err := d.uploadsWritable(ctx); err != nil {
		return err
	}
	return uploadErr(d.fs.StartUpload(ctx, id, record))
}

func (d *Drive) UploadRecord(ctx context.Context, id string) ([]byte, time.Time, error) {
	rec, t, err := d.fs.UploadRecord(ctx, id)
	return rec, t, uploadErr(err)
}

func (d *Drive) StagePart(ctx context.Context, id string, n int, content io.ReaderAt, size int64) error {
	if err := d.uploadsWritable(ctx); err != nil {
		return err
	}
	return uploadErr(d.fs.StageUploadPart(ctx, id, n, content, size))
}

func (d *Drive) UploadState(ctx context.Context, id string) (drive.UploadState, error) {
	st, err := d.fs.UploadState(ctx, id)
	if err != nil {
		return drive.UploadState{}, uploadErr(err)
	}
	return drive.UploadState{Parts: st.Parts, Size: st.Size, LastPart: st.LastPart}, nil
}

func (d *Drive) CompleteUpload(ctx context.Context, id, p string, parts int, cond drive.Condition, opts drive.WriteOptions) (drive.Item, error) {
	if err := d.uploadsWritable(ctx); err != nil {
		return drive.Item{}, err
	}
	if opts.SameAs != nil {
		return drive.Item{}, fmt.Errorf("an upload's content is its parts, not a reference: %w", drive.ErrInvalid)
	}
	src, err := d.fs.UploadSources(ctx, id, parts)
	if err != nil {
		return drive.Item{}, uploadErr(err)
	}
	return d.write(ctx, p, writeSource{upload: &src}, cond, opts)
}

func (d *Drive) FinishUpload(ctx context.Context, id string, result []byte) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return uploadErr(d.fs.FinishUpload(ctx, id, result))
}

func (d *Drive) UploadResult(ctx context.Context, id string) ([]byte, time.Time, error) {
	r, t, err := d.fs.UploadResult(ctx, id)
	return r, t, uploadErr(err)
}

func (d *Drive) DropUploadParts(ctx context.Context, id string) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return uploadErr(d.fs.DropUploadParts(ctx, id))
}

func (d *Drive) DropUploads(ctx context.Context) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return uploadErr(d.fs.DropUploads(ctx))
}

func (d *Drive) DropUpload(ctx context.Context, id string) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return uploadErr(d.fs.DropUpload(ctx, id))
}

func (d *Drive) Uploads(ctx context.Context, fn func(drive.UploadEntry) error) error {
	return uploadErr(d.fs.ListUploads(ctx, func(e icbfs.UploadEntry) error {
		return fn(drive.UploadEntry{ID: e.ID, Record: e.Record, Done: e.Done, Parts: e.Parts})
	}))
}
