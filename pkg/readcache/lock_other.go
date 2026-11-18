//go:build !unix && !windows

package readcache

import (
	"fmt"
	"os"
)

// No file locks here: one process a directory is assumed; no sweep removes
// anything (tryLockFile never succeeds) and the index lock is the
// process's own (every change to the index is under Cache.mu).
func tryLockFile(*os.File) bool { return false }

func lockFile(*os.File) error { return nil }

// CheckDir: only that dir is a directory.
func CheckDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafeDir, dir)
	}
	return nil
}
