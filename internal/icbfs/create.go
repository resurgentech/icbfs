package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/pb"
)

// Settings are a new filesystem's. All are fixed at creation except Size,
// which Resize changes.
type Settings struct {
	Name           string
	Size           uint64 // declared capacity: what df shows as the total
	PrimaryWindows bool
	MaxFileSize    uint64 // the largest file it holds; 0 means DefaultMaxFileSize
	// DirBlockEntries is how many entries a directory block holds before
	// it splits; 0 means DefaultDirBlockEntries.
	DirBlockEntries uint32

	// The root folder's owner and permissions: mode, uid and gid on any
	// filesystem, and on a primary-Windows one RootSDDL, the root's
	// security descriptor (empty leaves the root without one, and a
	// Windows mount then shows the mounting account's default). The root
	// is a folder like any other, so these can be changed later.
	RootMode, RootUid, RootGid uint32
	RootSDDL                   string
}

// Info describes one filesystem in a bucket.
type Info struct {
	Name            string
	ID              string
	Size            uint64
	PrimaryWindows  bool
	MaxFileSize     int64
	DirBlockEntries int
	Archived        bool
	Created         time.Time // zero for filesystems created before it was recorded
	FormatVersion   int       // its objects' format as a whole (see FormatVersion)
}

// filesystemName is what a filesystem may be called: it's part of the
// root's object key, so it's kept to characters every store takes.
var filesystemName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ErrInvalidFilesystemName is returned for a name outside filesystemName.
var ErrInvalidFilesystemName = errors.New("filesystem names are 1-63 letters, digits, '.', '_' or '-', starting with a letter or digit")

// CreateFilesystem creates a new filesystem in store, failing with
// ErrExists if the name is taken. This is the only way a filesystem comes
// to exist: mounts attach to existing ones (Filesystem.Attach). A create
// interrupted before the root was written is completed by calling it
// again with the same name.
func CreateFilesystem(ctx context.Context, store objstore.Store, s Settings) (Info, error) {
	if !filesystemName.MatchString(s.Name) {
		return Info{}, fmt.Errorf("%q: %w", s.Name, ErrInvalidFilesystemName)
	}
	if s.MaxFileSize > DefaultMaxFileSize {
		return Info{}, fmt.Errorf("max file size %d is past the largest supported, %d", s.MaxFileSize, int64(DefaultMaxFileSize))
	}
	if n := s.DirBlockEntries; n != 0 && (n < minDirBlockEntries || n > maxDirBlockEntries) {
		return Info{}, fmt.Errorf("directory block entries %d: %d to %d", n, minDirBlockEntries, maxDirBlockEntries)
	}
	id, entry, existed, err := registerFilesystem(ctx, store, &pb.FilesystemEntry{
		Name: s.Name, Size: s.Size, PrimaryWindows: s.PrimaryWindows, MaxFileSize: s.MaxFileSize,
		DirBlockEntries: s.DirBlockEntries,
		Created:         timestamppb.Now(),
	})
	if err != nil {
		return Info{}, err
	}
	f := New(store, s.Name)
	if err := f.applyEntry(id, entry); err != nil {
		return Info{}, err
	}
	if existed {
		if _, err := store.Head(ctx, f.RootKey()); err == nil {
			return Info{}, fmt.Errorf("filesystem %q: %w", s.Name, ErrExists)
		}
		// registered, but the root never landed: finish creating it
	}
	attr := Attr{Mode: s.RootMode, Uid: s.RootUid, Gid: s.RootGid}
	if s.PrimaryWindows && s.RootSDDL != "" {
		attr.WindowsSDDL = []byte(s.RootSDDL)
	}
	if err := f.createRoot(ctx, attr); err != nil {
		return Info{}, err
	}
	return infoOf(id, entry), nil
}

// Attach opens an existing filesystem for use, failing with ErrNotFound
// if there's none of this name in the store (or its creation never
// finished).
func (f *Filesystem) Attach(ctx context.Context) error {
	mb, _, err := readMaster(ctx, f.store)
	if err != nil {
		if errors.Is(mapNotFound(err), ErrNotFound) {
			return fmt.Errorf("no filesystem %q in this bucket: %w", f.fsName, ErrNotFound)
		}
		return err
	}
	for i, e := range mb.Filesystems {
		if e.Name == f.fsName {
			if err := f.applyEntry(fsID(i), e); err != nil {
				return err
			}
			if _, err := f.store.Head(ctx, f.RootKey()); err != nil {
				return fmt.Errorf("filesystem %q has no root (an interrupted create?): %w", f.fsName, mapNotFound(err))
			}
			return nil
		}
	}
	return fmt.Errorf("no filesystem %q in this bucket: %w", f.fsName, ErrNotFound)
}

// Archived reports whether the filesystem is archived (read-only for
// good), as of attaching or the last Refresh.
func (f *Filesystem) Archived() bool { return f.archived }

// Refresh re-reads the filesystem's master block entry, so a filesystem
// archived since it was attached stops taking writes.
func (f *Filesystem) Refresh(ctx context.Context) error {
	mb, _, err := readMaster(ctx, f.store)
	if err != nil {
		return mapNotFound(err)
	}
	for i, e := range mb.Filesystems {
		if e.Name == f.fsName && fsID(i) == f.id {
			f.archived = e.Archived
			return nil
		}
	}
	return fmt.Errorf("filesystem %q: %w", f.fsName, ErrNotFound)
}

// ListFilesystems lists the filesystems in store's bucket.
func ListFilesystems(ctx context.Context, store objstore.Store) ([]Info, error) {
	mb, _, err := readMaster(ctx, store)
	if err != nil {
		if errors.Is(mapNotFound(err), ErrNotFound) {
			return nil, nil // a bucket nothing has been created in
		}
		return nil, err
	}
	var out []Info
	for i, e := range mb.Filesystems {
		if e.Name != "" {
			out = append(out, infoOf(fsID(i), e))
		}
	}
	return out, nil
}

// DescribeFilesystem is one filesystem's Info.
func DescribeFilesystem(ctx context.Context, store objstore.Store, name string) (Info, error) {
	all, err := ListFilesystems(ctx, store)
	if err != nil {
		return Info{}, err
	}
	for _, fs := range all {
		if fs.Name == name {
			return fs, nil
		}
	}
	return Info{}, fmt.Errorf("no filesystem %q in this bucket: %w", name, ErrNotFound)
}

func infoOf(id string, e *pb.FilesystemEntry) Info {
	info := Info{Name: e.Name, ID: id, Size: e.Size, PrimaryWindows: e.PrimaryWindows, Archived: e.Archived, MaxFileSize: DefaultMaxFileSize,
		DirBlockEntries: dirBlockEntries(e), FormatVersion: max(1, int(e.FormatVersion))}
	if e.MaxFileSize != 0 && e.MaxFileSize <= DefaultMaxFileSize {
		info.MaxFileSize = int64(e.MaxFileSize)
	}
	if e.Created != nil {
		info.Created = e.Created.AsTime()
	}
	return info
}

// createRoot writes an empty root folder with attr's owner, mode and
// xattrs: .metadata first, so an interrupted create leaves no root block
// and is simply redone.
func (f *Filesystem) createRoot(ctx context.Context, attr Attr) error {
	empty, err := (&block.Block{Kind: block.Leaf}).Encode()
	if err != nil {
		return err
	}
	now := time.Now()
	attr.Nlink, attr.Mtime, attr.Ctime = 1, now, now
	metaData, err := encodeFileMetadata(attr)
	if err != nil {
		return err
	}
	rootKey := f.RootKey()
	if _, err := f.store.Put(ctx, metadataObjectKey(rootKey), bytes.NewReader(metaData), nil, ""); err != nil {
		return err
	}
	_, err = f.store.Put(ctx, rootKey, bytes.NewReader(empty), nil, "")
	return err
}
