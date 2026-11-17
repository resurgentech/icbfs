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
			// nothing else could become "." or "..".
			if strings.Trim(n, ". ") == "" {
				return "", fmt.Errorf("%q: the name %q is only dots and spaces, which Windows may read as a relative name: %w", p, n, drive.ErrInvalidName)
			}
		}
	}
	r := strings.TrimRight(root, sep)
	if windows {
		r = strings.TrimRight(r, "/")
	}
	return r + sep + strings.Join(names, sep), nil
}
