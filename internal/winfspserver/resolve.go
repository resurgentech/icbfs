// Package winfspserver is the WinFsp (Windows) access layer: icbfs's
// filesystem on go-winfsp's native Behaviour interfaces.
// internal/fuseserver is the Linux one.
//
// This file is portable: path resolution is plain icbfs.Filesystem logic,
// so it builds and tests on any platform.
package winfspserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
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
// cached inodes, so every call walks from the root; there's no path cache.
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
func resolvePath(ctx context.Context, fsys *icbfs.Filesystem, path string, caseInsensitive bool) (resolved, error) {
	key := fsys.RootKey()
	segments := splitPath(path)

	if len(segments) == 0 {
		attr, err := fsys.Stat(ctx, key, icbfs.TypeDir)
		if err != nil {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, err)
		}
		return resolved{Key: key, Type: icbfs.TypeDir, Attr: attr, Path: `\`}, nil
	}

	typ := icbfs.TypeDir
	var attr icbfs.Attr
	var stored strings.Builder
	for _, name := range segments {
		if typ != icbfs.TypeDir {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, icbfs.ErrNotDir)
		}
		entry, a, err := lookupCaseAware(ctx, fsys, key, name, caseInsensitive)
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
func hasIntermediateSymlink(ctx context.Context, fsys *icbfs.Filesystem, path string, caseInsensitive bool) bool {
	segments := splitPath(path)
	key := fsys.RootKey()
	for _, name := range segments[:max(len(segments)-1, 0)] {
		entry, _, err := lookupCaseAware(ctx, fsys, key, name, caseInsensitive)
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
func resolvePathFollow(ctx context.Context, fsys *icbfs.Filesystem, path string, caseInsensitive bool) (resolved, error) {
	r, err := resolvePath(ctx, fsys, path, caseInsensitive)
	if err != nil {
		return resolved{}, err
	}
	requested := r.Path
	dir, _ := splitParent(path)
	for i := 0; r.Type == icbfs.TypeSymlink; i++ {
		if i >= maxSymlinkHops {
			return resolved{}, fmt.Errorf("resolve %q: too many levels of symbolic links", path)
		}
		target, err := fsys.Readlink(ctx, r.Key)
		if err != nil {
			return resolved{}, err
		}
		next := resolveSymlinkTarget(dir, target)
		r, err = resolvePath(ctx, fsys, next, caseInsensitive)
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
