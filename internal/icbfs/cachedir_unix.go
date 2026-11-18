//go:build unix

package icbfs

import (
	"os"
	"syscall"
)

// tryLockFile takes f's exclusive lock without waiting: false if another
// open file holds it (in this process or another) or locks are not
// supported. The lock lasts until f is closed.
func tryLockFile(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
