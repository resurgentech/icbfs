package icbfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A Filesystem's own cache directory (T82-16, docs/STATES.md ME-6 in
// interloc): New makes none; the first open file that needs the disk
// cache makes "icbfs-cache-<random>" in the temp directory, and Close
// removes it. A directory left by a process that died (killed, or never
// closed its Filesystems) is removed by the next process's first New.
//
// Which directories belong to a process that died is told by a lock, not
// by a guess: the directory holds a file, cacheLockName, locked by its
// maker for as long as the directory is in use (flock on Unix, LockFileEx
// on Windows) and written with one byte once it is locked. The kernel lets
// go of the lock when the process ends, however it ends. The sweep removes
// a directory only when it takes that lock itself and the file has its
// byte: a live process's directory (another Drives, a daemon, a test, or
// another Filesystem in this process) is locked and is left; a directory
// with no lock file (made by older code) or an empty one (its maker is
// between making the file and locking it) is left too. Nothing but the
// temp directory's icbfs-cache-* directories is ever looked at, and a
// directory a caller gave (SetCacheOptions) is never removed.

const (
	cacheDirPrefix = "icbfs-cache-"
	cacheLockName  = "owner.lock"
)

// errCacheClosed is the disk cache of a closed Filesystem.
var errCacheClosed = errors.New("icbfs: the filesystem is closed")

// makeOwnedCacheDir makes a new cache directory in the temp directory
// and takes its lock: the lock file is open (and locked) until
// releaseOwnedCacheDir.
func makeOwnedCacheDir() (string, *os.File, error) {
	dir, err := os.MkdirTemp("", cacheDirPrefix)
	if err != nil {
		return "", nil, err
	}
	lf, err := os.OpenFile(filepath.Join(dir, cacheLockName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if ok := tryLockFile(lf); ok {
		// The byte says the maker held the lock: a sweep that takes the
		// lock of a file with it knows the maker let go.
		if _, err := lf.Write([]byte{1}); err != nil {
			lf.Close()
			_ = os.RemoveAll(dir)
			return "", nil, err
		}
	}
	// No lock (a platform or a filesystem without locks): the file stays
	// empty, and no sweep ever removes the directory; Close still does.
	return dir, lf, nil
}

// releaseOwnedCacheDir lets go of dir's lock and removes it.
func releaseOwnedCacheDir(dir string, lf *os.File) error {
	if lf != nil {
		lf.Close() // the lock goes with the file
	}
	return os.RemoveAll(dir)
}

var sweepOnce sync.Once

// sweepDeadCacheDirsOnce is the sweep, once a process, at its first New.
func sweepDeadCacheDirsOnce() {
	sweepOnce.Do(func() { removeDeadCacheDirs(os.TempDir()) })
}

// removeDeadCacheDirs removes the icbfs-cache-* directories in parent
// whose maker has let go of their lock (see above), and returns how many.
// It reads parent a page of names at a time, so a temp directory with a
// great many entries is never held in memory whole.
func removeDeadCacheDirs(parent string) int {
	d, err := os.Open(parent)
	if err != nil {
		return 0
	}
	defer d.Close()
	removed := 0
	for {
		ents, err := d.ReadDir(1024)
		for _, e := range ents {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), cacheDirPrefix) {
				continue
			}
			if removeIfDead(filepath.Join(parent, e.Name())) {
				removed++
			}
		}
		if err != nil || len(ents) == 0 { // io.EOF at the end, or a read error
			return removed
		}
	}
}

// removeIfDead removes dir if its lock file has its byte and its lock can
// be taken: its maker has let go.
func removeIfDead(dir string) bool {
	lf, err := os.OpenFile(filepath.Join(dir, cacheLockName), os.O_RDWR, 0)
	if err != nil {
		return false // no lock file: older code's, or being made
	}
	if !tryLockFile(lf) {
		lf.Close()
		return false // its maker holds it: in use
	}
	st, err := lf.Stat()
	if err != nil || st.Size() == 0 {
		lf.Close()
		return false // its maker has not locked it yet
	}
	return releaseOwnedCacheDir(dir, lf) == nil
}
