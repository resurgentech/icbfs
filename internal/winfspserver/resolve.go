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
	"fmt"
	"strings"

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
func resolvePath(ctx context.Context, fsys *icbfs.Filesystem, path string) (resolved, error) {
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
		entry, a, err := fsys.Lookup(ctx, key, name)
		if err != nil {
			return resolved{}, fmt.Errorf("resolve %q: %w", path, err)
		}
		key, typ, attr = entry.UUID, entry.Type, a
	}
	return resolved{Key: key, Type: typ, Attr: attr}, nil
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
