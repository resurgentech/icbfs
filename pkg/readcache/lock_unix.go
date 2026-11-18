//go:build unix

package readcache

import (
	"fmt"
	"os"
	"syscall"
)

// tryLockFile takes f's exclusive lock without waiting: false if another
// open file holds it (in this process or another). It lasts until f closes.
func tryLockFile(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

// lockFile takes f's exclusive lock, waiting for it.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

// CheckDir is ErrUnsafeDir unless dir is a directory of this process's
// user that neither group nor other can write.
func CheckDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafeDir, dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, not %d", ErrUnsafeDir, dir, sys.Uid, os.Getuid())
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s has mode %04o (group or other can write it)", ErrUnsafeDir, dir, st.Mode().Perm())
	}
	return nil
}
