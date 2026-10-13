package icbfs

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// Named data streams (Windows alternate data streams). A stream's bytes
// live in their own object, StreamKey(owner, id); the owner's .metadata
// maps stream names to ids (Attr.Streams). Stream objects have no
// .metadata of their own: everything but size and mtime is the owner's.
// POSIX access never sees them.

const streamKeyInfix = ".ads."

// StreamKey is the object key holding stream id's bytes for owner.
func StreamKey(owner string, id uint64) string {
	return owner + streamKeyInfix + strconv.FormatUint(id, 10)
}

// isStreamKey reports whether key is a stream object (owner keys are
// UUIDs or the root key, neither of which contain dots).
func isStreamKey(key string) bool { return strings.Contains(key, streamKeyInfix) }

// statStream is Stat for a stream object: size and times from the
// object itself. A stream whose object hasn't landed yet (CreateStream
// records the name first) reads as empty.
func (f *Filesystem) statStream(ctx context.Context, key string) (Attr, error) {
	obj, err := f.store.Head(ctx, key)
	if objstore.IsNotFound(err) {
		return Attr{Nlink: 1}, nil
	}
	if err != nil {
		return Attr{}, err
	}
	return Attr{Nlink: 1, Size: obj.Size, Mtime: obj.LastModified, Ctime: obj.LastModified}, nil
}

// StatStream returns a stream object's size and modification time.
func (f *Filesystem) StatStream(ctx context.Context, streamKey string) (Attr, error) {
	return f.statStream(ctx, streamKey)
}

// CreateStream adds an empty stream named name to owner (a file,
// symlink or directory of type typ), returning its object key and the
// new object's ETag (for NewOpenFile). ErrExists if the name is taken.
func (f *Filesystem) CreateStream(ctx context.Context, owner string, typ EntryType, name string) (string, string, error) {
	if err := f.checkWritable(); err != nil {
		return "", "", err
	}
	var id uint64
	exists := false
	if _, err := f.updateMetadata(ctx, owner, typ, func(a *Attr) {
		exists = false // the closure reruns on a lost CAS race
		if _, ok := a.Streams[name]; ok {
			exists = true
			return
		}
		if a.Streams == nil {
			a.Streams = make(map[string]uint64)
		}
		a.nextStreamID++
		id = a.nextStreamID
		a.Streams[name] = id
	}); err != nil {
		return "", "", err
	}
	if exists {
		return "", "", ErrExists
	}
	key := StreamKey(owner, id)
	obj, err := f.store.Put(ctx, key, bytes.NewReader(nil), nil, "")
	if err != nil {
		return "", "", err
	}
	return key, obj.ETag, nil
}

// DeleteStream removes stream name from owner. ErrNotFound if absent.
func (f *Filesystem) DeleteStream(ctx context.Context, owner string, typ EntryType, name string) error {
	removed, err := f.DeleteStreams(ctx, owner, typ, []string{name})
	if err != nil {
		return err
	}
	if removed == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteStreams removes every listed stream owner has, in one metadata
// write, ignoring names it doesn't have, and returns how many it
// removed.
func (f *Filesystem) DeleteStreams(ctx context.Context, owner string, typ EntryType, names []string) (int, error) {
	if err := f.checkWritable(); err != nil {
		return 0, err
	}
	var removed map[string]uint64
	if _, err := f.updateMetadata(ctx, owner, typ, func(a *Attr) {
		removed = make(map[string]uint64)
		for _, name := range names {
			if id, ok := a.Streams[name]; ok {
				removed[name] = id
				delete(a.Streams, name)
			}
		}
	}); err != nil {
		return 0, err
	}
	f.deleteStreamObjects(ctx, owner, removed)
	return len(removed), nil
}

// RenameStream renames owner's stream oldName to newName, replacing an
// existing newName only if replace is set (else ErrExists).
func (f *Filesystem) RenameStream(ctx context.Context, owner string, typ EntryType, oldName, newName string, replace bool) error {
	if err := f.checkWritable(); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	var opErr error
	var replaced uint64
	hadReplaced := false
	if _, err := f.updateMetadata(ctx, owner, typ, func(a *Attr) {
		opErr, hadReplaced = nil, false
		id, ok := a.Streams[oldName]
		if !ok {
			opErr = ErrNotFound
			return
		}
		if old, taken := a.Streams[newName]; taken {
			if !replace {
				opErr = ErrExists
				return
			}
			replaced, hadReplaced = old, true
		}
		delete(a.Streams, oldName)
		a.Streams[newName] = id
	}); err != nil {
		return err
	}
	if opErr != nil {
		return opErr
	}
	if hadReplaced {
		f.deleteStreamObjects(ctx, owner, map[string]uint64{newName: replaced})
	}
	return nil
}

// deleteStreamObjects removes stream objects, best-effort: a straggler
// is unreachable once no .metadata maps to it.
func (f *Filesystem) deleteStreamObjects(ctx context.Context, owner string, streams map[string]uint64) {
	for _, id := range streams {
		_ = f.store.Delete(ctx, StreamKey(owner, id), "")
	}
}
