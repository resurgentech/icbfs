package drive

import (
	"fmt"
	"strings"
)

// The one rule for a path, for every drive and every caller that takes a
// path from someone else (icbfsdrive, cliodrive, icbfs-git, a mount).
//
// A path is names joined by "/", with or without a leading "/"; "" and
// "/" are the top. The separator is "/" and nothing else: a backslash is
// a character of a name, never a separator, so no layer may read a path
// differently from the layer that checked it. A name that is empty (a
// "//", or a "/" at the end), "." or ".." makes the whole path invalid:
// it is refused with ErrInvalidName, never tidied. Nothing is ever
// resolved upwards, so a path given under a root names something under
// that root or nothing.
//
// A name may hold a backslash (a name stored before T82-14, or one a
// POSIX program made on a mount of the filesystem itself): such a name is
// read, listed, renamed and removed like any other. A new name may not
// (CheckNewName).

// SplitPath is p's names from the top, or ErrInvalidName.
func SplitPath(p string) ([]string, error) {
	q := strings.TrimPrefix(p, "/")
	if q == "" {
		return nil, nil
	}
	names := strings.Split(q, "/")
	for _, n := range names {
		if err := CheckPathName(n); err != nil {
			return nil, fmt.Errorf("path %q: %w", p, err)
		}
	}
	return names, nil
}

// CleanPath is p as "/" and its names joined by "/" ("/" for the top),
// or ErrInvalidName: SplitPath's rule, nothing tidied.
func CleanPath(p string) (string, error) {
	names, err := SplitPath(p)
	if err != nil {
		return "", err
	}
	return "/" + strings.Join(names, "/"), nil
}

// CheckPathName is whether name can be one name of a path: not empty,
// not "." or "..", no "/".
func CheckPathName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("an empty name: %w", ErrInvalidName)
	case name == "." || name == "..":
		return fmt.Errorf("the name %q: %w", name, ErrInvalidName)
	case strings.Contains(name, "/"):
		return fmt.Errorf("the name %q holds a /: %w", name, ErrInvalidName)
	}
	return nil
}

// CheckNewName is whether an item may be made with name, or moved or
// renamed to it: a path's name (CheckPathName) with no backslash. Every
// create, mkdir, move, rename and restore to a new place asks it, at the
// drive, so every route that makes a name gets it.
func CheckNewName(name string) error {
	if err := CheckPathName(name); err != nil {
		return err
	}
	if strings.Contains(name, `\`) {
		return fmt.Errorf("the name %q holds a backslash: %w", name, ErrInvalidName)
	}
	return nil
}

// Within is p, a path a caller gave, under root, a path of the same
// drive: root's names, then p's, joined by "/". Both are held to the
// rule (an invalid one is ErrInvalidName), so the result names root
// itself, something under it, or nothing; never anything beside or above
// it. It is the one way to put a caller's path under a folder: never glue
// a root onto a caller's string.
func Within(root, p string) (string, error) {
	r, err := SplitPath(root)
	if err != nil {
		return "", fmt.Errorf("root: %w", err)
	}
	names, err := SplitPath(p)
	if err != nil {
		return "", err
	}
	return "/" + strings.Join(append(r, names...), "/"), nil
}

// Under is whether p (a clean path, as CleanPath gives) is root or under
// it.
func Under(root, p string) bool {
	if root == "" || root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == root || strings.HasPrefix(p, root+"/")
}
