//go:build !unix && !windows

package icbfs

import "os"

// tryLockFile: no file locks here, so no sweep ever removes a directory;
// Close still removes its own.
func tryLockFile(*os.File) bool { return false }
