//go:build windows

package icbfs

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes f's exclusive lock on its first byte without waiting:
// false if another handle holds it. The lock lasts until f is closed.
func tryLockFile(f *os.File) bool {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol) == nil
}
