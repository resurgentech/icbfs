//go:build !windows

package objstore

import (
	"errors"
	"os"
	"syscall"
)

// historyLock is the history cache's write turn across processes: an
// exclusive flock on a file beside the database. A process blocked in
// flock is woken when the lock is let go, not on a timer, so a writer
// waiting its turn is not passed over by writers that keep coming, as it
// is in SQLite's own busy wait (a sleep of up to 100 ms between tries).
type historyLock struct{ f *os.File }

func openHistoryLock(path string) (*historyLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &historyLock{f: f}, nil
}

// lock blocks until this process holds the lock.
func (l *historyLock) lock() error {
	for {
		err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func (l *historyLock) unlock() error {
	return syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
}

func (l *historyLock) close() error { return l.f.Close() }
