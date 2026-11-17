package folderside

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// onWindows is whether this side's paths are Windows paths.
var onWindows = runtime.GOOS == "windows"

// localPath is the file a sync path p names under root, or an error if
// p cannot be a place under root (T82-14). p is the drive's path, names
// joined by "/" (the one rule, drive.SplitPath): an empty name, "." or
// ".." is refused, never resolved, so nothing above root is reached. On
// Windows a backslash is the volume's separator, so a name holding one,
// stored by a POSIX writer, would be read there as several names, ".."
// among them: it is refused, as is a name that Windows trims to nothing
// or to "." or ".." (trailing dots and spaces). On anything else such a
// name is one file of that name.
//
// It is plain string logic, chosen by windows rather than by the build,
// so both are tested on Linux.
func localPath(root, p string, windows bool) (string, error) {
	names, err := drive.SplitPath(p)
	if err != nil {
		return "", fmt.Errorf("%q is not a place in the folder: %w", p, err)
	}
	if len(names) == 0 {
		return root, nil
	}
	sep := "/"
	if windows {
		sep = `\`
		for _, n := range names {
			if strings.Contains(n, `\`) {
				return "", fmt.Errorf("%q: the name %q holds a backslash, a separator on Windows: %w", p, n, drive.ErrInvalidName)
			}
			// Win32 trims a name's trailing dots and spaces: a name of
			// nothing else could become "." or "..", and one that ends in
			// either is another name on disk (two drive items, one file).
			if strings.Trim(n, ". ") == "" {
				return "", fmt.Errorf("%q: the name %q is only dots and spaces, which Windows may read as a relative name: %w", p, n, drive.ErrInvalidName)
			}
			if strings.HasSuffix(n, ".") || strings.HasSuffix(n, " ") {
				return "", fmt.Errorf("%q: the name %q ends in a dot or a space, which Windows drops: %w", p, n, drive.ErrInvalidName)
			}
			if isDeviceName(n) {
				return "", fmt.Errorf("%q: the name %q is a Windows device name: %w", p, n, drive.ErrInvalidName)
			}
		}
	}
	r := strings.TrimRight(root, sep)
	if windows {
		r = strings.TrimRight(r, "/")
	}
	return r + sep + strings.Join(names, sep), nil
}

// isDeviceName is whether Windows opens a device, not a file, for name:
// CON, PRN, AUX, NUL, COM0-9, LPT0-9 (and the superscript digits),
// CONIN$ and CONOUT$, in any case, alone or before an extension, with
// trailing spaces.
func isDeviceName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) >= 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		switch base[3:] {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
			return len([]rune(base[3:])) == 1
		}
	}
	return false
}
