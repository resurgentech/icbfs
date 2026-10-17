package block

import (
	"strings"
	"unicode/utf8"
)

// upcase is upcasePairs as a direct lookup: upcase[c] is code unit c's
// uppercase, or 0 where c has none (maps to itself).
var upcase [0x10000]uint16

func init() {
	for _, p := range upcasePairs {
		upcase[p[0]] = p[1]
	}
}

// foldedUnit decodes the first character of s and returns its
// case-folded value and its length in bytes. A character in the Basic
// Multilingual Plane folds to its simple uppercase, per the pinned table
// in upcase_table.go. Everything else is kept as is: characters outside
// the BMP (NTFS compares their UTF-16 surrogate halves, which have no
// case) and bytes that aren't valid UTF-8 (possible from a POSIX
// mount), which get values above every rune so they never collide with
// one.
func foldedUnit(s string) (rune, int) {
	if c := s[0]; c < utf8.RuneSelf {
		if u := upcase[c]; u != 0 {
			return rune(u), 1
		}
		return rune(c), 1
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return utf8.MaxRune + 1 + rune(s[0]), 1
	}
	if r <= 0xFFFF {
		if u := upcase[r]; u != 0 {
			return rune(u), size
		}
	}
	return r, size
}

// CompareFold compares a and b case-insensitively, the way NTFS does:
// character by character after folding each to its uppercase (see
// foldedUnit). It returns 0 when NTFS would call the two names the
// same, and otherwise orders them consistently.
//
// CompareFold(a, b) == 0 is the one definition of a case-insensitive
// name match in icbfs. Folded directories are sorted by it, so the
// table behind it must never change under existing data.
func CompareFold(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ra, na := foldedUnit(a[i:])
		rb, nb := foldedUnit(b[j:])
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		i += na
		j += nb
	}
	switch {
	case i < len(a):
		return 1
	case j < len(b):
		return -1
	}
	return 0
}

// EqualFold reports whether a and b are the same name case-insensitively
// (CompareFold(a, b) == 0).
func EqualFold(a, b string) bool { return CompareFold(a, b) == 0 }

// FoldKey is a name's case-folded form, for keying a map: FoldKey(a) ==
// FoldKey(b) exactly when EqualFold(a, b). It isn't a readable name.
func FoldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s) * 2)
	for i := 0; i < len(s); {
		r, n := foldedUnit(s[i:])
		b.WriteByte(byte(r >> 24))
		b.WriteByte(byte(r >> 16))
		b.WriteByte(byte(r >> 8))
		b.WriteByte(byte(r))
		i += n
	}
	return b.String()
}

// Collation is the order a directory's entries are kept in.
type Collation uint8

const (
	// Exact sorts names by their bytes. Primary-POSIX filesystems.
	Exact Collation = iota
	// Folded sorts names case-insensitively first (CompareFold) and by
	// their bytes second, so every name that matches a given name
	// case-insensitively is in one contiguous run that a lookup can seek
	// to, instead of scanning the whole directory. Primary-Windows
	// filesystems.
	Folded
)

// Compare orders a and b under c. Distinct names never compare equal
// under either collation.
func (c Collation) Compare(a, b string) int {
	if c == Folded {
		if n := CompareFold(a, b); n != 0 {
			return n
		}
	}
	return strings.Compare(a, b)
}
