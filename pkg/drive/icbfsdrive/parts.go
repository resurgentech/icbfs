package icbfsdrive

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
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
	return d.CompleteUploadWith(ctx, id, p, parts, cond, opts, drive.UploadWrite{})
}

// CompleteUploadWith is CompleteUpload with up (T82-10 round 6): the
// complete's time recorded on the version, and up.Plan told the key before
// the first byte and the store's copy id before the first piece.
func (d *Drive) CompleteUploadWith(ctx context.Context, id, p string, parts int, cond drive.Condition, opts drive.WriteOptions, up drive.UploadWrite) (drive.Item, error) {
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
	src.Completed = up.Completed
	return d.write(ctx, p, writeSource{upload: &src, plan: up.Plan}, cond, opts)
}

// FindUpload is what an earlier write of upload id left under key, or
// under the file at p's key when key is "" (drive.PartWriter). Where a
// person has the file is asked in order of cost: p (one walk down), the
// trash (three reads), then a lookup by the key as an id (the drive's
// memory, then a search of its tree: the one rare case, a key written,
// not found at p nor in the trash, costs a walk). An error at any step is
// returned: "could not be searched", never "not there".
func (d *Drive) FindUpload(ctx context.Context, id, key, p string, since, completed time.Time) (drive.UploadFind, error) {
	var out drive.UploadFind
	at, err := d.resolve(ctx, d.fs, drive.ByPath(p))
	switch {
	case err == nil && at.typ == icbfs.TypeFile:
	case err == nil, errors.Is(mapErr(err), drive.ErrNotFound), errors.Is(mapErr(err), drive.ErrNotDir):
		at = nil
	default:
		return out, mapErr(err)
	}
	if key == "" {
		if at == nil {
			return out, nil
		}
		key = at.key
	}
	out.Key = key
	own, newer, err := d.fs.UploadVersions(ctx, key, id, since, completed)
	if err != nil {
		return out, uploadErr(err)
	}
	out.Newer = newer
	if own != nil {
		out.Version = &drive.Version{ID: own.VersionID, Size: own.Size, Stored: own.LastModified, Time: own.LastModified}
	}
	if at != nil && at.key == key {
		out.AtPath, out.Named = true, true
		return out, nil
	}
	if own == nil {
		// Nothing of this upload's under the key: whether a name reaches it
		// is not asked (its caller writes again, or erases a new key that
		// holds nothing of it).
		return out, nil
	}
	if _, err := d.fs.TrashedItem(ctx, key); err == nil {
		out.Trashed = true
		return out, nil
	} else if !errors.Is(err, icbfs.ErrNotFound) {
		return out, mapErr(err)
	}
	if _, err := d.resolve(ctx, d.fs, drive.ByID(key)); err == nil {
		out.Named = true
	} else if !errors.Is(mapErr(err), drive.ErrNotFound) {
		return out, mapErr(err)
	}
	return out, nil
}

// EraseUnnamed erases every object under key, a new file's key whose
// write was cut before its link (drive.PartWriter): refused (ErrExists)
// if a name reaches it, or it is in the trash.
func (d *Drive) EraseUnnamed(ctx context.Context, key string) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	if _, err := d.fs.TrashedItem(ctx, key); err == nil {
		return fmt.Errorf("%s is in the trash: %w", key, drive.ErrExists)
	} else if !errors.Is(err, icbfs.ErrNotFound) {
		return mapErr(err)
	}
	if _, err := d.resolve(ctx, d.fs, drive.ByID(key)); err == nil {
		return fmt.Errorf("%s is named: %w", key, drive.ErrExists)
	} else if !errors.Is(mapErr(err), drive.ErrNotFound) {
		return mapErr(err)
	}
	return uploadErr(d.fs.EraseKey(ctx, key))
}

// AbortCopy ends the store's copy copyID into key (drive.PartWriter).
func (d *Drive) AbortCopy(ctx context.Context, key, copyID string) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return mapErr(objstore.AbortCopy(ctx, d.fs.Store(), key, copyID))
}

var _ drive.UploadIndex = (*Drive)(nil)
var _ drive.CopyKeeper = (*Drive)(nil)

// KeepCopy names a store copy that could not be ended now, for
// EndKeptCopies (drive.CopyKeeper).
func (d *Drive) KeepCopy(ctx context.Context, key, copyID string) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return mapErr(icbfs.KeepCopy(ctx, d.fs.Store(), key, copyID))
}

// EndKeptCopies ends the store copies KeepCopy named (drive.CopyKeeper).
func (d *Drive) EndKeptCopies(ctx context.Context) (int, error) {
	if d.cfg.ReadOnly {
		return 0, errDriveReadOnly
	}
	n, err := icbfs.EndKeptCopies(ctx, d.fs.Store())
	return n, mapErr(err)
}

// UploadingFilesystems is the names of the store's filesystems with
// uploads in parts (drive.UploadIndex): one listing of their marks.
func (d *Drive) UploadingFilesystems(ctx context.Context) (map[string]bool, error) {
	m, err := icbfs.UploadingFilesystems(ctx, d.fs.Store())
	return m, mapErr(err)
}

// UnmarkUploads erases the mark that says the drive's filesystem has
// uploads in parts (drive.UploadMarker): its caller found none.
func (d *Drive) UnmarkUploads(ctx context.Context) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return mapErr(d.fs.UnmarkUploads(ctx))
}

func (d *Drive) CheckUploadParts(ctx context.Context, id string, count int) (int64, drive.Checksums, error) {
	size, sums, err := d.fs.CheckUploadParts(ctx, id, count)
	if err != nil {
		return 0, drive.Checksums{}, uploadErr(err)
	}
	return size, drive.Checksums{MD5: hex.EncodeToString(sums.MD5), SHA256: hex.EncodeToString(sums.SHA256)}, nil
}

func (d *Drive) SetUploadNote(ctx context.Context, id, note string, data []byte) error {
	if d.cfg.ReadOnly {
		return errDriveReadOnly
	}
	return uploadErr(d.fs.SetUploadNote(ctx, id, note, data))
}

func (d *Drive) UploadNote(ctx context.Context, id, note string) ([]byte, time.Time, error) {
	data, t, err := d.fs.UploadNote(ctx, id, note)
	return data, t, uploadErr(err)
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
		return fn(drive.UploadEntry{ID: e.ID, Record: e.Record, Notes: e.Notes, NoteTimes: e.NoteTimes, Parts: e.Parts})
	}))
}

func (d *Drive) SetUploadNoteIf(ctx context.Context, id, note string, data []byte, etag string) (string, error) {
	if d.cfg.ReadOnly {
		return "", errDriveReadOnly
	}
	tag, err := d.fs.SetUploadNoteIf(ctx, id, note, data, etag)
	if errors.Is(err, icbfs.ErrUploadTaken) {
		return "", fmt.Errorf("%w (%v)", drive.ErrPrecondition, err)
	}
	return tag, uploadErr(err)
}

func (d *Drive) UploadNoteTag(ctx context.Context, id, note string) ([]byte, string, time.Time, error) {
	data, tag, t, err := d.fs.UploadNoteTag(ctx, id, note)
	return data, tag, t, uploadErr(err)
}

// UploadWritten finds the version of the file at p that upload id wrote
// (icbfs's MetaUpload), dated since or later.
func (d *Drive) UploadWritten(ctx context.Context, id, p string, since time.Time) (drive.Item, bool, error) {
	n, err := d.resolve(ctx, d.fs, drive.ByPath(p))
	if errors.Is(mapErr(err), drive.ErrNotFound) {
		return drive.Item{}, false, nil
	}
	if err != nil {
		return drive.Item{}, false, mapErr(err)
	}
	if n.typ != icbfs.TypeFile {
		return drive.Item{}, false, nil
	}
	obj, err := d.fs.UploadVersion(ctx, n.key, id, since)
	if errors.Is(err, icbfs.ErrNotFound) {
		return drive.Item{}, false, nil
	}
	if err != nil {
		return drive.Item{}, false, uploadErr(err)
	}
	it := d.item(ctx, n, true)
	it.Size = obj.Size
	it.Version = &drive.Version{ID: obj.VersionID, Size: obj.Size, Stored: obj.LastModified, Time: obj.LastModified}
	return it, true, nil
}
