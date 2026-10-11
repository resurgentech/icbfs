// Package winfspserver is the WinFsp (Windows) access layer: it adapts
// icbfs's core filesystem logic to the chosen WinFsp binding
// (github.com/winfsp/go-winfsp, used directly against its native
// Behaviour* interfaces — see ROADMAP.md's Part F, tasks F1/F3).
// Windows only for the binding-specific files — internal/fuseserver is
// the separate, parallel Linux/POSIX access layer per ARCHITECTURE.md.
//
// This file (resolve.go) is deliberately portable, not Windows-only:
// path resolution is pure icbfs.Filesystem logic with no dependency on
// go-winfsp at all, so it builds and tests on any platform — see its
// own doc comment for why that's the point, not an accident.
package winfspserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/icbfs"
)

// resolved is what resolvePath found: the entry's own key (the UUID
// every other icbfs.Filesystem call is keyed on), its type, and its
// current attributes — everything a WinFsp callback needs to answer
// Open/GetFileInfo/GetSecurityByName without a second round trip back
// through icbfs.Filesystem.
type resolved struct {
	Key  string
	Type icbfs.EntryType
	Attr icbfs.Attr
}

// resolvePath walks path — a WinFsp path string, backslash-separated,
// conventionally starting with `\` — from fsys's root down to the
// named entry, via repeated calls to icbfs.Filesystem.Lookup.
//
// This exists because neither WinFsp binding under consideration (nor
// any WinFsp binding at all — ROADMAP.md's F1/F2 entries) gives this
// driver what go-fuse's node tree gives internal/fuseserver: a cached
// *Inode a path lookup can hand back on a later call. WinFsp's native
// interface is flatly path-string-addressed on every single callback,
// so every call re-walks from the root here; there is no per-path
// cache in this first cut (a driver-owned one is a possible future
// optimization, not attempted now — see ROADMAP.md's F2 entry).
//
// Intermediate path components that resolve to something other than a
// directory fail with icbfs.ErrNotDir, matching the real filesystem's
// semantics for e.g. opening `\a-plain-file\nested`. Symlink-following
// is deliberately not attempted here — not in scope for F2's own
// "Done when" (nested directories and hardlinked names resolving to
// the same UUID), and genuinely a separate design question for
// whoever wires in real symlink/reparse-point handling (F3).
//
// caseInsensitive controls whether a path segment is matched against
// stored names exactly (false) or case-foldedly (true, via
// lookupCaseAware) — ARCHITECTURE.md's Windows compatibility section
// calls for exactly the standard NTFS/Samba/WSL2 pattern here: storage
// itself always stays case-sensitive/case-preserving (icbfs.Filesystem's
// own Lookup is never touched by this), and case-insensitive matching
// is purely an access-layer behavior, driven by task F4's mount flag
// (see cmd/icbfs-winfsp's --case-insensitive).
func resolvePath(ctx context.Context, fsys *icbfs.Filesystem, path string, caseInsensitive bool) (resolved, error) {
	key := fsys.RootKey()
	segments := splitPath(path)

	if len(segments) == 0 {
		attr, err := fsys.Stat(ctx, key, icbfs.TypeDir)
		if err != nil {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, err)
		}
		return resolved{Key: key, Type: icbfs.TypeDir, Attr: attr}, nil
	}

	typ := icbfs.TypeDir
	var attr icbfs.Attr
	for _, name := range segments {
		if typ != icbfs.TypeDir {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, icbfs.ErrNotDir)
		}
		entry, a, err := lookupCaseAware(ctx, fsys, key, name, caseInsensitive)
		if err != nil {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, err)
		}
		key, typ, attr = entry.UUID, entry.Type, a
	}
	return resolved{Key: key, Type: typ, Attr: attr}, nil
}

// lookupCaseAware is fsys.Lookup, with an optional case-insensitive
// fallback: an exact match is always tried first (and is always what
// gets returned when one exists, even in case-insensitive mode — this
// matters when two differently-cased names coexist, a state this
// driver's own Create never produces but that pre-existing data or a
// POSIX mount of the same filesystem could), and only on a genuine
// not-found does case-insensitive mode fall back to a full directory
// scan for a case-folded match.
//
// icbfs.Filesystem itself gains no case-insensitive mode at all —
// deliberately: storage stays case-sensitive ground truth regardless
// of how any one mount chooses to look things up (ARCHITECTURE.md).
// The fallback scan's O(directory size) cost on every case-mismatched
// lookup is accepted for the same reason ListByPrefix's own listing
// cost is accepted elsewhere in this project: correct first, and nothing
// faster exists without new storage-layer indexing this task doesn't
// call for.
func lookupCaseAware(ctx context.Context, fsys *icbfs.Filesystem, dirKey, name string, caseInsensitive bool) (block.Entry, icbfs.Attr, error) {
	entry, attr, err := fsys.Lookup(ctx, dirKey, name)
	if err == nil || !caseInsensitive || !errors.Is(err, icbfs.ErrNotFound) {
		return entry, attr, err
	}
	entries, rdErr := fsys.ReadDir(ctx, dirKey)
	if rdErr != nil {
		return block.Entry{}, icbfs.Attr{}, err // the original not-found is still the right error to report
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name, name) {
			a, statErr := fsys.Stat(ctx, e.UUID, e.Type)
			if statErr != nil {
				return block.Entry{}, icbfs.Attr{}, statErr
			}
			return e, a, nil
		}
	}
	return block.Entry{}, icbfs.Attr{}, err
}

// maxSymlinkHops bounds resolvePathFollow against a symlink cycle —
// nothing currently prevents a symlink from pointing at itself or at
// an ancestor that points back, so this is a real, not theoretical,
// termination guarantee.
const maxSymlinkHops = 20

// ntPathPrefix is what Windows' own CreateSymbolicLinkW prepends to an
// absolute symlink target before this driver ever sees it, confirmed
// empirically against a real WinFsp mount, not assumed (see
// ROADMAP.md's F3 entry) — the NT namespace's "DOS device path"
// prefix. Recognizing it is how resolvePathFollow tells an absolute
// stored target apart from a relative one, since icbfs.Filesystem.
// Symlink's own target string carries no separate flag for this.
const ntPathPrefix = `\??\`

// resolvePathFollow is resolvePath, but transparently follows a
// symlink at the very end of path too (resolvePath's own "intermediate
// component must be a directory" rule already means a symlink midway
// through a path is never transparently followed by either function —
// a genuinely separate, harder question not attempted here, same as
// resolvePath's own doc comment already flags).
//
// Needed because, confirmed empirically against a real mount: the
// kernel transparently follows an *absolute* symlink target itself
// (it can just reissue an independent, fresh open against it, with no
// involvement from this filesystem) but not a *relative* one —
// resolving "reltarget.txt relative to this link's own directory"
// needs this filesystem's own cooperation, which nothing else
// provides. Tried shipping without this and confirmed the gap for
// real first, not assumed: an absolute-target symlink's content read
// correctly through a real mount; the identical read through a
// relative-target symlink failed with "Could not find a part of the
// path," traced directly to the kernel never issuing a follow-up open
// against the resolved target at all.
func resolvePathFollow(ctx context.Context, fsys *icbfs.Filesystem, path string, caseInsensitive bool) (resolved, error) {
	r, err := resolvePath(ctx, fsys, path, caseInsensitive)
	if err != nil {
		return resolved{}, err
	}
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
	return r, nil
}

// resolveSymlinkTarget turns a stored symlink target into a path
// resolvePath can walk, given dir (the symlink's own parent
// directory, for a relative target):
//   - an NT-prefixed absolute target (`\??\K:\sub\file`) has the
//     prefix and drive letter stripped, leaving an in-volume path —
//     this driver only ever serves one drive letter per mount, so
//     whatever follows the colon is that path;
//   - a target already starting with `\` is already volume-absolute;
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

// splitParent splits path into its parent directory path and its own
// base name — e.g. `\dir1\dir2\file.txt` splits to (`\dir1\dir2`,
// `file.txt`); a root-level name like `\file.txt` splits to an empty
// parent and `file.txt`, and resolvePath on an empty string already
// resolves to the root, so callers never need to special-case that
// themselves.
func splitParent(path string) (dir, base string) {
	path = strings.TrimSuffix(path, `\`)
	idx := strings.LastIndex(path, `\`)
	if idx < 0 {
		return "", path
	}
	return path[:idx], path[idx+1:]
}

// splitPath breaks a WinFsp path into its non-empty segments: both a
// bare backslash and an empty string (the root) split to zero
// segments, and a leading/trailing/doubled backslash never produces a
// spurious empty segment.
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
