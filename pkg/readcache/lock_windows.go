//go:build windows

package readcache

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes f's exclusive lock on its first byte without waiting.
func tryLockFile(f *os.File) bool {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol) == nil
}

// lockFile takes f's exclusive lock on its first byte, waiting for it.
func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

// CheckDir: on Windows only that dir is a directory (its access list is
// the system's to set).
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
