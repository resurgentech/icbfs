package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// A file's history is its content's versions and its .metadata's. Like a
// directory's (ReplaceDirHistory), it's normally only added to;
// ReplaceFileHistory rewrites it, for a caller correcting it (a date the
// source got wrong, a version that shouldn't be there).

// FileState is one state of a file: from At, content Version (an
// existing version of the file, copied) or Content (new bytes), with
// attributes Attr (nil: the file's attributes now), whose Mtime, if
// zero, is At.
type FileState struct {
	At      time.Time
	Version string
	Content io.Reader
	Attr    *Attr
}

// FileVersion is one content version of a file with its effective date.
type FileVersion struct {
	VersionID string
	ETag      string
	Size      int64
	At        time.Time
	Stored    time.Time
}

// ErrFileChanged is ReplaceFileHistory's refusal when the file isn't the
// version the caller read: it has changed since.
var (
	ErrFileChanged = errors.New("the file has changed since its history was read")
	// ErrFileOpen is its refusal while the file is open through a mount or
	// a session here: its content is in flux.
	ErrFileOpen = errors.New("the file is open")
)

// FileHistory is file key's content versions, newest first by effective
// date, and the id of its current version (what ReplaceFileHistory's
// ifMatch takes).
func (f *Filesystem) FileHistory(ctx context.Context, key string) ([]FileVersion, string, error) {
	history, err := f.store.History(ctx, key)
	if err != nil {
		return nil, "", mapNotFound(err)
	}
	dates, err := objstore.ReadDates(ctx, f.store, key)
	if err != nil {
		return nil, "", err
	}
	current := ""
	var out []FileVersion
	for i, o := range history {
		if o.Deleted {
			continue
		}
		if i == 0 {
			current = o.VersionID
		}
		out = append(out, FileVersion{VersionID: o.VersionID, ETag: o.ETag, Size: o.Size, At: dates.Time(&o), Stored: o.LastModified})
	}
	for i := 1; i < len(out); i++ { // insertion sort: newest first by date, the store's order on ties
		for j := i; j > 0 && out[j].At.After(out[j-1].At); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, current, nil
}

// ReplaceFileHistory erases file key's history (its content's versions
// and its .metadata's) and writes states in its place, oldest first, each
// dated at its time (At, strictly increasing and not in the future); the
// last is the file now. A state's Version is copied (on the server, for
// content past one part), so nothing is downloaded to keep a version.
// ifMatch, if set, is the current version id FileHistory gave:
// ErrFileChanged if the file has changed since. Streams and the file's
// names are left as they are. The caller stops its own writes to the
// file meanwhile.
func (f *Filesystem) ReplaceFileHistory(ctx context.Context, key string, states []FileState, ifMatch string) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if isStreamKey(key) {
		return fmt.Errorf("a stream's history: %w", ErrInvalidArgument)
	}
	ctx = undeclared(ctx)
	if len(states) == 0 {
		return fmt.Errorf("a file's history needs its state now: %w", ErrInvalidArgument)
	}
	now := time.Now()
	for i, s := range states {
		if s.At.After(now) || i > 0 && !s.At.After(states[i-1].At) {
			return fmt.Errorf("state %d at %s: states go oldest first, none in the future: %w", i, s.At.Format(time.RFC3339Nano), ErrInvalidArgument)
		}
		if (s.Version == "") == (s.Content == nil) {
			return fmt.Errorf("state %d: a version to keep or new content, one of the two: %w", i, ErrInvalidArgument)
		}
	}
	if c := f.openContent(key); c != nil {
		return fmt.Errorf("the file is open: %w", ErrFileOpen)
	}
	oldContent, err := f.store.History(ctx, key)
	if err != nil {
		return mapNotFound(err)
	}
	if ifMatch != "" && (len(oldContent) == 0 || oldContent[0].VersionID != ifMatch) {
		return ErrFileChanged
	}
	mk := metadataObjectKey(key)
	oldMeta, err := f.store.History(ctx, mk)
	if err != nil {
		return mapNotFound(err)
	}
	attrNow, err := f.statFile(ctx, key)
	if err != nil {
		return err
	}
	byID := map[string]objstore.Object{}
	for _, o := range oldContent {
		if !o.Deleted {
			byID[o.VersionID] = o
		}
	}

	rec := &objstore.Recorder{}
	for i, s := range states {
		sctx := objstore.WithRecorder(objstore.WithDeclaredTime(ctx, s.At), rec)
		var obj *objstore.Object
		if s.Version != "" {
			src, ok := byID[s.Version]
			if !ok {
				return fmt.Errorf("state %d: version %s: %w", i, s.Version, ErrNotFound)
			}
			if obj, err = f.copyVersionOnTop(sctx, key, src); err != nil {
				return err
			}
		} else if obj, err = f.store.Put(sctx, key, s.Content, nil, ""); err != nil {
			return err
		}
		attr := attrNow
		if s.Attr != nil {
			attr = *s.Attr
		}
		if attr.Mtime.IsZero() {
			attr.Mtime = s.At
		}
		if s.Version != "" {
			rekeyETag(&attr, byID[s.Version].ETag, obj.ETag) // the copy's checksums are the original's
		}
		attr.mtimeOverrideETag = obj.ETag
		if attr.Ctime.IsZero() || attr.Ctime.Before(s.At) {
			attr.Ctime = s.At
		}
		data, err := f.encodeMetadata(sctx, attr)
		if err != nil {
			return err
		}
		if _, err := f.store.Put(sctx, mk, bytes.NewReader(data), nil, ""); err != nil {
			return err
		}
	}
	if err := objstore.SetDates(ctx, f.store, key, rec.Dates()[key]); err != nil {
		return err
	}
	for _, v := range oldContent {
		if err := f.store.Erase(ctx, key, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	for _, v := range oldMeta {
		if err := f.store.Erase(ctx, mk, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	return f.markPast(ctx)
}
