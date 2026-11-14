package winfspserver

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// Names Windows can't hold, on a WinFsp mount of a primary-POSIX
// filesystem (T164).
//
// A primary-POSIX filesystem stores any POSIX-legal name, so it can hold
// `a:b`, `a\b`, `a.`, a tab. Windows can't open these by path. The mount
// shows each as a substitute and takes the substitute back; the stored name
// never changes, and Linux and the API see it as stored. The convention is
// Cygwin's ("Forbidden characters in filenames"): each of
// `< > : " | ? *` and 0x01-0x1F is the character plus 0xF000 (`:` is
// U+F03A, tab U+F009). Cygwin leaves backslash alone, as a separator; here
// a stored backslash is U+F05C too. A last character that is a space or a
// dot is U+F020 or U+F02E (only the last). Device names (CON, NUL.txt,
// com1, LPT¹) are left as they are: T163 found they open on a plain path.
//
// The mapping is one to one, so every stored name is listed and
// reachable. A stored name may already hold a code point this mapping
// produces; that one is shown as the marker U+F000 followed by itself (a
// stored U+F000 is U+F000 U+F000). Shown to stored, U+F000 followed by a
// code point of the set is that code point, literally; U+F000 followed by
// anything else, or last, is an invalid name (icbfs.ErrInvalidName).
// Private-use code points outside the set pass through unchanged.
//
// This file is portable: plain string logic, tested on any platform.

const (
	winMarker = 0xF000 // the escape marker, and the offset of a substitute
	winSpace  = 0xF020 // a trailing space
	winDot    = 0xF02E // a trailing dot
)

// winForbidden are the stored characters shown as themselves plus 0xF000,
// besides 0x01-0x1F.
const winForbidden = `<>:"|?*\`

// winInSet reports whether r is a code point this mapping can produce, or
// the marker: the ones a stored name must escape.
func winInSet(r rune) bool {
	switch {
	case r >= winMarker && r <= winMarker+0x1F: // marker and U+F001-U+F01F
		return true
	case r == winSpace, r == winDot:
		return true
	case r >= winMarker+0x21 && r <= winMarker+0x7C:
		// U+F022 " , F02A * , F03A : , F03C < , F03E > , F03F ? ,
		// F05C \ , F07C |
		return strings.ContainsRune(winForbidden, r-winMarker)
	}
	return false
}

// winShown is the name a Windows mount shows for a stored name.
func winShown(stored string) string {
	var b strings.Builder
	b.Grow(len(stored))
	last := len(stored)
	if last > 0 {
		_, w := utf8.DecodeLastRuneInString(stored)
		last -= w
	}
	for i, r := range stored {
		switch {
		case r == utf8.RuneError && invalidByteAt(stored, i):
			// A byte that is not UTF-8 is kept as it is.
			b.WriteString(stored[i : i+1])
		case winInSet(r):
			b.WriteRune(winMarker)
			b.WriteRune(r)
		case r >= 1 && r <= 0x1F, r < utf8.RuneSelf && strings.ContainsRune(winForbidden, r):
			b.WriteRune(r + winMarker)
		case i == last && r == ' ':
			b.WriteRune(winSpace)
		case i == last && r == '.':
			b.WriteRune(winDot)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// invalidByteAt reports whether s[i] is a byte that is not part of a UTF-8
// character (a real U+FFFD is three bytes).
func invalidByteAt(s string, i int) bool {
	_, w := utf8.DecodeRuneInString(s[i:])
	return w == 1
}

// winStored is the stored name a name typed in Windows stands for. A U+F000
// not followed by a code point of the set is icbfs.ErrInvalidName.
func winStored(shown string) (string, error) {
	if !strings.ContainsFunc(shown, func(r rune) bool { return r >= winMarker && r < winMarker+0x80 }) {
		return shown, nil
	}
	var b strings.Builder
	b.Grow(len(shown))
	for i := 0; i < len(shown); {
		r, w := utf8.DecodeRuneInString(shown[i:])
		i += w
		switch {
		case r == winMarker:
			next, nw := utf8.DecodeRuneInString(shown[i:])
			if i >= len(shown) || !winInSet(next) {
				return "", fmt.Errorf("%w: %q has a U+F000 not followed by a substitute character", icbfs.ErrInvalidName, shown)
			}
			i += nw
			b.WriteRune(next)
		case winInSet(r):
			b.WriteRune(r - winMarker)
		case r == utf8.RuneError && w == 1:
			b.WriteString(shown[i-1 : i])
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}

// winMapPath applies f to each component of a backslash-separated path,
// keeping the separators (a leading one, doubled ones, a trailing one) and
// leaving `.` and `..` alone.
func winMapPath(path string, f func(string) (string, error)) (string, error) {
	parts := strings.Split(path, `\`)
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			continue // separators and relative steps in a symlink target are not names
		}
		m, err := f(p)
		if err != nil {
			return "", err
		}
		parts[i] = m
	}
	return strings.Join(parts, `\`), nil
}

// winShownPath is a stored path (components joined by backslashes) as the
// mount shows it. Each component alone: the backslash in a stored name is
// already U+F05C once shown, so the separators stay unambiguous.
func winShownPath(stored string) string {
	out, _ := winMapPath(stored, func(s string) (string, error) { return winShown(s), nil })
	return out
}

// winStoredPath is a shown path as stored; it fails like winStored.
func winStoredPath(shown string) (string, error) { return winMapPath(shown, winStored) }

// winTargetParts splits a symlink target into its NT prefix and drive,
// which are not names (`\??\M:` has a `?` and a `:`), and the rest.
func winTargetParts(target string) (head, rest string) {
	rest = target
	if strings.HasPrefix(rest, ntPathPrefix) {
		head, rest = ntPathPrefix, rest[len(ntPathPrefix):]
		if len(rest) >= 2 && rest[1] == ':' && (rest[0]|0x20 >= 'a' && rest[0]|0x20 <= 'z') && (len(rest) == 2 || rest[2] == '\\') {
			head, rest = head+rest[:2], rest[2:]
		}
	}
	return head, rest
}

// winShownTarget is a stored symlink target as the mount shows it: the
// `\??\X:` prefix of an absolute target is kept, every name in the rest
// is mapped.
func winShownTarget(stored string) string {
	head, rest := winTargetParts(stored)
	return head + winShownPath(rest)
}

// winStoredTarget is a symlink target written through the mount, as
// stored.
func winStoredTarget(shown string) (string, error) {
	head, rest := winTargetParts(shown)
	s, err := winStoredPath(rest)
	if err != nil {
		return "", err
	}
	return head + s, nil
}
