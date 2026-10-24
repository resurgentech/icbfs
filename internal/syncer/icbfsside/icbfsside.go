// Package icbfsside is the icbfs end of a sync pair: a drive (a whole
// icbfs filesystem or a directory in one), through the drive library,
// with its versions, its trash, file times kept as given, and content
// hashes stored with each file.
package icbfsside

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/syncer"
	"github.com/resurgentech/icbfs/pkg/drive"
	"github.com/resurgentech/icbfs/pkg/drive/icbfsdrive"
)

// Side is a drive as a sync side.
type Side struct {
	d       *icbfsdrive.Drive
	fs      *icbfs.Filesystem
	actor   string
	windows bool

	mu      sync.Mutex
	trashed map[string]drive.TrashEntry // by trash entry id, from the last TrashedFiles
}

var (
	_ syncer.Side   = (*Side)(nil)
	_ syncer.Hashes = (*Side)(nil)
	_ syncer.Trash  = (*Side)(nil)
)

// Open attaches to filesystem fsName in store, the drive rooted at root
// ("" or "/" for all of it). actor is recorded as who made the changes
// (in the trash, for instance).
func Open(ctx context.Context, store objstore.Store, fsName, root, actor, cacheDir string) (*Side, error) {
	d, err := icbfsdrive.New(ctx, icbfsdrive.Config{Store: store, Filesystem: fsName, Root: root, ID: "sync:" + fsName + root, CacheDir: cacheDir})
	if err != nil {
		return nil, err
	}
	return &Side{d: d, fs: d.Filesystem(), actor: actor, windows: d.Filesystem().PrimaryWindows(), trashed: map[string]drive.TrashEntry{}}, nil
}

// Close releases the drive.
func (s *Side) Close() { s.d.Close() }

// ctx is the system acting, recorded as the actor: the sync tool is
// trusted with the whole drive.
func (s *Side) ctx(ctx context.Context) context.Context {
	return drive.WithProfile(drive.WithActor(ctx, s.actor), drive.Profile{System: true})
}

func abs(p string) string { return "/" + strings.TrimPrefix(p, "/") }

func rel(p string) string { return strings.TrimPrefix(p, "/") }

// notExist maps the drive's not-found to os.ErrNotExist, which the
// engine looks for.
func notExist(err error) error {
	if errors.Is(err, drive.ErrNotFound) {
		return fmt.Errorf("%w (%v)", os.ErrNotExist, err)
	}
	return err
}

func entry(it drive.Item) syncer.Entry {
	e := syncer.Entry{Path: rel(it.Path), Dir: it.Type == drive.TypeDir, Size: it.Size, ModTime: it.Modified, Created: it.Created, ID: it.ID}
	if it.Version != nil {
		e.Version = it.Version.ID
	}
	return e
}

func (s *Side) Caps() syncer.Caps {
	return syncer.Caps{IDs: true, ACLs: s.windows, TimeResolution: time.Nanosecond, Created: true}
}

func (s *Side) List(ctx context.Context) ([]syncer.Entry, error) {
	page, err := s.d.List(s.ctx(ctx), drive.ByPath("/"), drive.ListOptions{Depth: 1 << 30})
	if err != nil {
		return nil, err
	}
	out := make([]syncer.Entry, 0, len(page.Items))
	for _, it := range page.Items {
		if it.Type == drive.TypeSymlink {
			continue
		}
		out = append(out, entry(it))
	}
	return out, nil
}

func (s *Side) Read(ctx context.Context, p string) (io.ReadCloser, error) {
	rc, _, err := s.d.Read(s.ctx(ctx), drive.ByPath(abs(p)), drive.At{}, nil)
	return rc, notExist(err)
}

// stat is a live item, with its stored attributes.
func (s *Side) stat(ctx context.Context, p string) (drive.Item, icbfs.Attr, error) {
	it, err := s.d.Stat(s.ctx(ctx), drive.ByPath(abs(p)), drive.At{})
	if err != nil {
		return drive.Item{}, icbfs.Attr{}, notExist(err)
	}
	typ := icbfs.TypeFile
	if it.Type == drive.TypeDir {
		typ = icbfs.TypeDir
	}
	attr, err := s.fs.Stat(ctx, it.ID, typ)
	return it, attr, err
}

// Stat is one item, as listed.
func (s *Side) Stat(ctx context.Context, p string) (syncer.Entry, error) {
	it, err := s.d.Stat(s.ctx(ctx), drive.ByPath(abs(p)), drive.At{})
	if err != nil {
		return syncer.Entry{}, notExist(err)
	}
	return entry(it), nil
}

func (s *Side) Write(ctx context.Context, p string, r io.Reader, size int64, mod, created time.Time) (syncer.Entry, error) {
	// The folder's times, not the copy's, in the write's own metadata
	// update: so the next pass sees the two as the same.
	it, err := s.d.Write(s.ctx(ctx), abs(p), r, drive.Condition{}, drive.WriteOptions{Parents: true, ModTime: mod, Created: created})
	if err != nil {
		return syncer.Entry{}, err
	}
	if it.Size != size {
		return syncer.Entry{}, fmt.Errorf("write %s: %d bytes written, want %d", p, it.Size, size)
	}
	e := entry(it)
	e.ModTime = mod
	return e, nil
}

// SetCreated sets an item's creation time, with no new version.
func (s *Side) SetCreated(ctx context.Context, p string, t time.Time) error {
	_, err := s.d.SetTimes(s.ctx(ctx), drive.ByPath(abs(p)), time.Time{}, t)
	return notExist(err)
}

func (s *Side) Mkdir(ctx context.Context, p string) (syncer.Entry, error) {
	it, err := s.d.Mkdir(s.ctx(ctx), abs(p), true)
	if err != nil {
		return syncer.Entry{}, err
	}
	return entry(it), nil
}

func (s *Side) Move(ctx context.Context, from, to string) (syncer.Entry, error) {
	if dir := path.Dir(rel(to)); dir != "." {
		if _, err := s.d.Mkdir(s.ctx(ctx), abs(dir), true); err != nil {
			return syncer.Entry{}, err
		}
	}
	it, err := s.d.Move(s.ctx(ctx), drive.ByPath(abs(from)), abs(to), false)
	if err != nil {
		return syncer.Entry{}, notExist(err)
	}
	return entry(it), nil
}

func (s *Side) Remove(ctx context.Context, p string) error {
	return notExist(s.d.Delete(s.ctx(ctx), drive.ByPath(abs(p)), false, drive.Condition{}))
}

func (s *Side) SetACL(ctx context.Context, p, sddl string) error {
	if !s.windows {
		return nil
	}
	it, err := s.d.Stat(s.ctx(ctx), drive.ByPath(abs(p)), drive.At{})
	if err != nil {
		return notExist(err)
	}
	typ := icbfs.TypeFile
	if it.Type == drive.TypeDir {
		typ = icbfs.TypeDir
	}
	_, err = s.fs.SetWindowsACL(ctx, it.ID, typ, []byte(sddl))
	return err
}

// --- syncer.Hashes ---

func (s *Side) StoredHash(ctx context.Context, p string) (string, bool) {
	_, attr, err := s.stat(ctx, p)
	if err != nil {
		return "", false
	}
	h, ok := attr.App[icbfs.AppSyncHash]
	return string(h), ok && len(h) > 0
}

func (s *Side) StoreHash(ctx context.Context, p, hash string) error {
	it, err := s.d.Stat(s.ctx(ctx), drive.ByPath(abs(p)), drive.At{})
	if err != nil {
		return notExist(err)
	}
	_, err = s.fs.SetAppData(ctx, it.ID, icbfs.TypeFile, icbfs.AppSyncHash, []byte(hash))
	return err
}

// --- syncer.Trash ---

func (s *Side) TrashedFiles(ctx context.Context, size int64) ([]syncer.Trashed, error) {
	page, err := s.d.Trash(s.ctx(ctx), drive.TrashOptions{})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []syncer.Trashed
	for _, e := range page.Entries {
		if e.Type != drive.TypeFile || e.Size != size || e.Version == "" {
			continue
		}
		s.trashed[e.ID] = e
		out = append(out, syncer.Trashed{ID: e.ID, Path: rel(e.Path), Size: e.Size, DeletedAt: e.DeletedAt})
	}
	return out, nil
}

func (s *Side) TrashedHash(ctx context.Context, t syncer.Trashed) (string, error) {
	s.mu.Lock()
	e, ok := s.trashed[t.ID]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("trash entry %s: %w", t.ID, os.ErrNotExist)
	}
	rc, _, err := s.d.Read(s.ctx(ctx), drive.ByID(e.ItemID), drive.At{Version: e.Version}, nil)
	if err != nil {
		return "", notExist(err)
	}
	defer rc.Close()
	return syncer.HashReader(rc)
}

func (s *Side) Restore(ctx context.Context, t syncer.Trashed, to string) (syncer.Entry, error) {
	it, err := s.d.Restore(s.ctx(ctx), t.ID, drive.RestoreOptions{To: abs(to), Parents: true})
	if err != nil {
		return syncer.Entry{}, notExist(err)
	}
	s.mu.Lock()
	delete(s.trashed, t.ID)
	s.mu.Unlock()
	return entry(it), nil
}
