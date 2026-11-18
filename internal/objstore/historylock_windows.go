//go:build windows

package objstore

import (
	"os"

	"golang.org/x/sys/windows"
)

// historyLock is the history cache's write turn across processes: an
// exclusive LockFileEx on a file beside the database, which blocks until
// the lock is let go (see historylock_unix.go).
type historyLock struct{ f *os.File }

func openHistoryLock(path string) (*historyLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &historyLock{f: f}, nil
}

func (l *historyLock) lock() error {
	return windows.LockFileEx(windows.Handle(l.f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped))
}

func (l *historyLock) unlock() error {
	return windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

func (l *historyLock) close() error { return l.f.Close() }
