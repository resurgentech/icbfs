// Package folderside is the folder end of a sync pair: a directory on
// this machine — a local disk or a mapped drive. What it can tell
// depends on the volume: on NTFS, stable file ids (so renames are seen
// as renames) and real ACLs; on a network drive such as a WebDAV drive,
// only names, sizes and times, to the second. Open finds out, and each
// can be forced on or off.
package folderside

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/resurgentech/icbfs/internal/syncer"
)

// TempPrefix starts the names of the files a download writes before
// renaming them into place; the sync ignores them.
const TempPrefix = ".icbfs-sync-tmp-"

// Options override what Open finds out about the volume.
type Options struct {
	// IDs, ACLs: "auto" (or ""), "on", "off".
	IDs, ACLs string
	// TimeResolution, if set, is used instead of what Open finds.
	TimeResolution time.Duration
}

// Side is a directory.
type Side struct {
	root string
	caps syncer.Caps
}

var _ syncer.Side = (*Side)(nil)

// Open is the directory at root.
func Open(root string, o Options) (*Side, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s isn't a directory", root)
	}
	caps := probe(root)
	switch o.IDs {
	case "on":
		caps.IDs = true
	case "off":
		caps.IDs = false
	}
	switch o.ACLs {
	case "on":
		caps.ACLs = aclsSupported
	case "off":
		caps.ACLs = false
	}
	if o.TimeResolution > 0 {
		caps.TimeResolution = o.TimeResolution
	}
	return &Side{root: root, caps: caps}, nil
}

func (s *Side) Caps() syncer.Caps { return s.caps }

func (s *Side) abs(p string) string { return filepath.Join(s.root, filepath.FromSlash(p)) }

func (s *Side) List(ctx context.Context) ([]syncer.Entry, error) {
	var out []syncer.Entry
	err := filepath.WalkDir(s.root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == s.root {
				return err
			}
			// Unreadable: leave it out this pass rather than fail it all.
			if de != nil && de.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == s.root {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(path.Base(rel), TempPrefix) {
			return nil
		}
		if de.Type()&fs.ModeSymlink != 0 || de.Type()&fs.ModeIrregular != 0 {
			return nil // links and reparse points aren't followed or copied
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		e := syncer.Entry{Path: rel, Dir: de.IsDir(), ModTime: info.ModTime().UTC()}
		if !e.Dir {
			e.Size = info.Size()
		}
		if s.caps.IDs {
			e.ID = fileID(p, info)
		}
		if s.caps.ACLs {
			e.ACL = readACL(p)
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Side) Read(ctx context.Context, p string) (io.ReadCloser, error) {
	return openShared(s.abs(p))
}

func (s *Side) entry(p string) (syncer.Entry, error) {
	full := s.abs(p)
	info, err := os.Stat(full)
	if err != nil {
		return syncer.Entry{}, err
	}
	e := syncer.Entry{Path: p, Dir: info.IsDir(), ModTime: info.ModTime().UTC()}
	if !e.Dir {
		e.Size = info.Size()
	}
	if s.caps.IDs {
		e.ID = fileID(full, info)
	}
	if s.caps.ACLs {
		e.ACL = readACL(full)
	}
	return e, nil
}

// Write writes to a temporary file beside the target and renames it
// into place, so a reader never sees half a file.
func (s *Side) Write(ctx context.Context, p string, r io.Reader, size int64, mod time.Time) (syncer.Entry, error) {
	full := s.abs(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return syncer.Entry{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), TempPrefix+"*")
	if err != nil {
		return syncer.Entry{}, err
	}
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("write %s: %d bytes, want %d", p, n, size)
	}
	if err == nil {
		err = os.Chtimes(tmp.Name(), mod, mod)
	}
	if err == nil {
		err = replace(tmp.Name(), full)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return syncer.Entry{}, err
	}
	return s.entry(p)
}

func (s *Side) Mkdir(ctx context.Context, p string) (syncer.Entry, error) {
	if err := os.MkdirAll(s.abs(p), 0o755); err != nil {
		return syncer.Entry{}, err
	}
	return s.entry(p)
}

func (s *Side) Move(ctx context.Context, from, to string) (syncer.Entry, error) {
	dst := s.abs(to)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return syncer.Entry{}, err
	}
	if err := os.Rename(s.abs(from), dst); err != nil {
		return syncer.Entry{}, err
	}
	return s.entry(to)
}

func (s *Side) Remove(ctx context.Context, p string) error {
	err := os.Remove(s.abs(p))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return err
}

func (s *Side) SetACL(ctx context.Context, p, sddl string) error {
	if !s.caps.ACLs {
		return nil
	}
	return writeACL(s.abs(p), sddl)
}
