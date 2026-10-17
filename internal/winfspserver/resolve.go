// Package winfspserver is the WinFsp (Windows) access layer: icbfs's
// filesystem on go-winfsp's native Behaviour interfaces.
// internal/fuseserver is the Linux one.
//
// This file is portable: path resolution is plain icbfs.Filesystem logic,
// so it builds and tests on any platform.
package winfspserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/namecache"
)

// resolved is what resolvePath found: the entry's key, type and current
// attributes, enough for Open, GetFileInfo and GetSecurityByName without
// another round trip.
type resolved struct {
	Key  string
	Type icbfs.EntryType
	Attr icbfs.Attr

	// Path is the requested path with each component in its stored case, as
	// WinFsp's NormalizedName wants (setNormalizedName). For
	// resolvePathFollow it's still the requested path, not the target's.
	Path string
}

// resolvePath walks path (a WinFsp path, backslash-separated, usually
// starting with `\`) from fsys's root to the named entry with
// icbfs.Filesystem.Lookup.
//
// WinFsp addresses every callback by path, with nothing like go-fuse's
// cached inodes, so every call walks from the root, through res's name
// cache (-meta-cache) when it has one: a component it knows costs nothing.
//
// An intermediate component that isn't a directory is icbfs.ErrNotDir, as
// opening `\a-file\nested` is on NTFS. It never follows a symlink partway
// along the path; hasIntermediateSymlink hands that case to WinFsp.
//
// caseInsensitive matches components case-insensitively (lookupCaseAware),
// the mount's -case-insensitive. Storage is always case-preserving; the
// primary mode only decides the cost, since a primary-Windows filesystem's
// directories are sorted case-insensitively and icbfs.Filesystem.LookupFold
// can seek.
func resolvePath(ctx context.Context, res resolver, path string) (resolved, error) {
	key := res.fsys.RootKey()
	segments := splitPath(path)

	if len(segments) == 0 {
		// The root's own attributes, cached like a name's (under no
		// directory: the root has no parent): Explorer asks for them all
		// the time.
		load := func() (namecache.Value, error) {
			attr, err := res.fsys.Stat(ctx, key, icbfs.TypeDir)
			if err != nil {
				return namecache.Value{}, err
			}
			return namecache.Value{Found: true, Entry: block.Entry{UUID: key, Type: icbfs.TypeDir}, Attr: attr}, nil
		}
		var v namecache.Value
		var err error
		if res.freshLast {
			v, err = load()
		} else {
			v, err = res.names.Lookup("", key, load)
		}
		if err != nil {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, err)
		}
		return resolved{Key: key, Type: icbfs.TypeDir, Attr: v.Attr, Path: `\`}, nil
	}

	typ := icbfs.TypeDir
	var attr icbfs.Attr
	var stored strings.Builder
	for i, name := range segments {
		if typ != icbfs.TypeDir {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, icbfs.ErrNotDir)
		}
		entry, a, err := res.lookup(ctx, key, name, res.freshLast && i == len(segments)-1)
		if err != nil {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, err)
		}
		key, typ, attr = entry.UUID, entry.Type, a
		stored.WriteString(`\`)
		stored.WriteString(entry.Name)
	}
	return resolved{Key: key, Type: typ, Attr: attr, Path: stored.String()}, nil
}

// hasIntermediateSymlink reports whether walking path reaches a symlink
// before its last component. resolvePath never follows one there; WinFsp
// does: GetSecurityByName answers STATUS_REPARSE (as WinFsp's memfs does
// with FspFileSystemFindReparsePoint), and WinFsp walks the path through
// GetReparsePointByName, handles relative targets and "." and "..", and
// reissues the open (winfsp-tests' reparse_symlink_relative_test).
func hasIntermediateSymlink(ctx context.Context, res resolver, path string) bool {
	segments := splitPath(path)
	key := res.fsys.RootKey()
	for _, name := range segments[:max(len(segments)-1, 0)] {
		entry, _, err := res.lookup(ctx, key, name, false)
		if err != nil {
			return false
		}
		switch entry.Type {
		case icbfs.TypeSymlink:
			return true
		case icbfs.TypeDir:
			key = entry.UUID
		default:
			return false
		}
	}
	return false
}

// resolver is how the driver looks names up: in fsys, case-insensitively
// with ci (the mount's -case-insensitive), through names, the mount's name
// cache (nil or disabled: every lookup asks the store). freshLast makes a
// path's last component always come from the store, as an open's must
// (close-to-open): the name may have gone, or lead elsewhere, since it
// was cached.
type resolver struct {
	fsys      *icbfs.Filesystem
	ci        bool
	names     *namecache.Cache
	freshLast bool
}

// lookup is lookupCaseAware through the name cache; fresh skips the cache
// (and refreshes it).
func (res resolver) lookup(ctx context.Context, dirKey, name string, fresh bool) (block.Entry, icbfs.Attr, error) {
	load := func() (namecache.Value, error) {
		e, a, err := lookupCaseAware(ctx, res.fsys, dirKey, name, res.ci)
		if errors.Is(err, icbfs.ErrNotFound) {
			return namecache.Value{}, nil
		}
		if err != nil {
			return namecache.Value{}, err
		}
		return namecache.Value{Found: true, Entry: e, Attr: a}, nil
	}
	var v namecache.Value
	var err error
	if fresh || !res.names.Enabled() {
		gen := res.names.Gen()
		if v, err = load(); err == nil {
			res.names.Refresh(dirKey, name, v, gen)
		}
	} else {
		v, err = res.names.Lookup(dirKey, name, load)
	}
	if err != nil {
		return block.Entry{}, icbfs.Attr{}, err
	}
	if !v.Found {
		return block.Entry{}, icbfs.Attr{}, fmt.Errorf("%s: %w", name, icbfs.ErrNotFound)
	}
	return v.Entry, v.Attr, nil
}

// lookupCaseAware is fsys.Lookup, or with caseInsensitive fsys.LookupFold:
// an exact match if there is one (names differing only in case can coexist,
// made by a case-sensitive or Linux mount), otherwise a case-insensitive
// one.
func lookupCaseAware(ctx context.Context, fsys *icbfs.Filesystem, dirKey, name string, caseInsensitive bool) (block.Entry, icbfs.Attr, error) {
	if caseInsensitive {
		return fsys.LookupFold(ctx, dirKey, name)
	}
	return fsys.Lookup(ctx, dirKey, name)
}

// maxSymlinkHops bounds resolvePathFollow: a symlink can point at itself,
// or at one that points back.
const maxSymlinkHops = 20

// ntPathPrefix is the NT "DOS device path" prefix CreateSymbolicLinkW puts
// on an absolute target. It's how resolvePathFollow tells an absolute
// target from a relative one; the stored target carries no flag.
const ntPathPrefix = `\??\`

// resolvePathFollow is resolvePath that also follows a symlink at the end
// of path.
//
// The kernel follows an absolute symlink target itself, by opening the
// target afresh, but not a relative one: without this, reading through a
// relative symlink failed with "Could not find a part of the path".
func resolvePathFollow(ctx context.Context, res resolver, path string) (resolved, error) {
	r, err := resolvePath(ctx, res, path)
	if err != nil {
		return resolved{}, err
	}
	requested := r.Path
	dir, _ := splitParent(path)
	for i := 0; r.Type == icbfs.TypeSymlink; i++ {
		if i >= maxSymlinkHops {
			return resolved{}, fmt.Errorf("resolve %q: too many levels of symbolic links", path)
		}
		target, err := res.fsys.Readlink(ctx, r.Key)
		if err != nil {
			return resolved{}, err
		}
		next := resolveSymlinkTarget(dir, target)
		r, err = resolvePath(ctx, res, next)
		if err != nil {
			return resolved{}, err
		}
		dir, _ = splitParent(next)
	}
	r.Path = requested
	return r, nil
}

// resolveSymlinkTarget turns a stored target into a path resolvePath can
// walk; dir is the symlink's parent, for a relative target:
//
//   - `\??\K:\sub\file` loses the prefix and drive letter (a mount is one
//     drive), leaving `\sub\file`;
//   - a target starting with `\` is already a volume path;
//   - anything else is relative to dir.
func resolveSymlinkTarget(dir, target string) string {
	target = strings.TrimPrefix(target, ntPathPrefix)
	if idx := strings.Index(target, ":"); idx >= 0 {
		return target[idx+1:]
	}
	if strings.HasPrefix(target, `\`) {
		return target
	}
	return dir + `\` + target
}

// splitParent splits path into its parent's path and its name:
// `\dir1\dir2\file.txt` is (`\dir1\dir2`, `file.txt`), and `\file.txt` is
// ("", `file.txt`); resolvePath resolves "" to the root.
func splitParent(path string) (dir, base string) {
	path = strings.TrimSuffix(path, `\`)
	idx := strings.LastIndex(path, `\`)
	if idx < 0 {
		return "", path
	}
	return path[:idx], path[idx+1:]
}

// splitPath splits a WinFsp path into its non-empty components: `\` and ""
// (the root) have none, and extra backslashes make no empty ones.
func splitPath(path string) []string {
	parts := strings.Split(path, `\`)
	segments := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			segments = append(segments, p)
		}
	}
	return segments
}

// splitStream splits a WinFsp name into its owner's path and, for a named
// data stream (`\dir\file:stream`, optionally with `:$DATA`), the stream
// name. `\dir\file::$DATA` is the main stream, i.e. no stream. ok is false
// for a stream type other than $DATA, the only kind supported.
func splitStream(name string) (path, stream string, ok bool) {
	slash := strings.LastIndexByte(name, '\\')
	colon := strings.IndexByte(name[slash+1:], ':')
	if colon < 0 {
		return name, "", true
	}
	colon += slash + 1
	path, rest := name[:colon], name[colon+1:]
	stream, typ, hasType := strings.Cut(rest, ":")
	if hasType && !strings.EqualFold(typ, "$DATA") {
		return "", "", false
	}
	return path, stream, true
}

// lookupStream finds a stream by name among streams, case-insensitively
// when caseInsensitive (NTFS stream names are), returning its stored
// spelling and id.
func lookupStream(streams map[string]uint64, name string, caseInsensitive bool) (string, uint64, bool) {
	if id, ok := streams[name]; ok {
		return name, id, true
	}
	if caseInsensitive {
		for n, id := range streams {
			if block.EqualFold(n, name) {
				return n, id, true
			}
		}
	}
	return "", 0, false
}

// joinPath appends name to a WinFsp directory path.
func joinPath(dir, name string) string {
	if dir == `\` || dir == "" {
		return `\` + name
	}
	return dir + `\` + name
}
