package icbfsdrive

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// SideStore is a drive.SideStore on an icbfs filesystem of its own: each
// key a file (its '/'-separated parts directories), metadata in its app data.
type SideStore struct {
	fs *icbfs.Filesystem
}

var _ drive.SideStore = (*SideStore)(nil)

const sideMetaPrefix = icbfs.AppSidePrefix

// OpenSideStore attaches to the side store's filesystem, creating it (a
// primary-POSIX filesystem nobody mounts) the first time.
func OpenSideStore(ctx context.Context, store objstore.Store, fsName string) (*SideStore, error) {
	if _, err := icbfs.CreateFilesystem(ctx, store, icbfs.Settings{Name: fsName, Size: 1 << 40, RootMode: 0o700}); err != nil && !errors.Is(err, icbfs.ErrExists) {
		return nil, err
	}
	fsys := icbfs.New(store, fsName)
	if err := fsys.Attach(ctx); err != nil {
		return nil, err
	}
	return &SideStore{fs: fsys}, nil
}

// dir walks (with create, makes) the directories of key, returning the
// last one's key and the key's base name.
func (s *SideStore) dir(ctx context.Context, key string, create bool) (string, string, error) {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", fmt.Errorf("side store key %q: %w", key, drive.ErrInvalidName)
	}
	dir := s.fs.RootKey()
	for _, p := range parts[:len(parts)-1] {
		e, _, err := s.fs.Lookup(ctx, dir, p)
		if errors.Is(err, icbfs.ErrNotFound) && create {
			k, _, merr := s.fs.Mkdir(ctx, dir, p, 0o700, 0, 0)
			if merr == nil {
				dir = k
				continue
			}
			if !errors.Is(merr, icbfs.ErrExists) {
				return "", "", mapErr(merr)
			}
			e, _, err = s.fs.Lookup(ctx, dir, p)
		}
		if err != nil {
			return "", "", mapErr(err)
		}
		dir = e.UUID
	}
	return dir, parts[len(parts)-1], nil
}

// Put writes key's content and metadata, as a new version.
func (s *SideStore) Put(ctx context.Context, key string, body []byte, meta map[string]string) error {
	dir, name, err := s.dir(ctx, key, true)
	if err != nil {
		return err
	}
	var fileKey, etag string
	e, _, err := s.fs.Lookup(ctx, dir, name)
	switch {
	case err == nil:
		fileKey = e.UUID
	case errors.Is(err, icbfs.ErrNotFound):
		fileKey, _, etag, err = s.fs.Create(ctx, dir, name, 0o600, 0, 0)
		if errors.Is(err, icbfs.ErrExists) {
			return s.Put(ctx, key, body, meta)
		}
		if err != nil {
			return mapErr(err)
		}
	default:
		return mapErr(err)
	}
	if _, _, err := s.fs.WriteFile(ctx, fileKey, body, etag); err != nil {
		return mapErr(err)
	}
	attr, err := s.fs.Stat(ctx, fileKey, icbfs.TypeFile)
	if err != nil {
		return mapErr(err)
	}
	for k := range attr.App {
		if strings.HasPrefix(k, sideMetaPrefix) {
			if _, ok := meta[strings.TrimPrefix(k, sideMetaPrefix)]; !ok {
				if _, err := s.fs.SetAppData(ctx, fileKey, icbfs.TypeFile, k, nil); err != nil {
					return mapErr(err)
				}
			}
		}
	}
	for k, v := range meta {
		if _, err := s.fs.SetAppData(ctx, fileKey, icbfs.TypeFile, sideMetaPrefix+k, []byte(v)); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// Get reads key's content and metadata.
func (s *SideStore) Get(ctx context.Context, key string) ([]byte, map[string]string, error) {
	dir, name, err := s.dir(ctx, key, false)
	if err != nil {
		return nil, nil, err
	}
	e, _, err := s.fs.Lookup(ctx, dir, name)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	data, attr, _, err := s.fs.ReadFile(ctx, e.UUID)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	meta := map[string]string{}
	for k, v := range attr.App {
		if strings.HasPrefix(k, sideMetaPrefix) {
			meta[strings.TrimPrefix(k, sideMetaPrefix)] = string(v)
		}
	}
	return data, meta, nil
}

// Erase deletes key with all its versions.
func (s *SideStore) Erase(ctx context.Context, key string) error {
	dir, name, err := s.dir(ctx, key, false)
	if err != nil {
		return err
	}
	return mapErr(s.fs.Purge(ctx, dir, name, true))
}

// List lists the names directly under prefix: keys, and the next
// level of longer keys.
func (s *SideStore) List(ctx context.Context, prefix string) ([]string, error) {
	dir, _, err := s.dir(ctx, strings.TrimSuffix(prefix, "/")+"/x", false)
	if err != nil {
		return nil, err
	}
	entries, err := s.fs.ReadDir(ctx, dir)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out, nil
}
