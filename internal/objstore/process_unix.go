//go:build !windows

package objstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// processAlive reports whether a process with this pid exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// notOthers refuses, when euid is root's, a path whose nearest existing
// part (the file, or the directory it would be made in) belongs to
// another user: root would leave files there that user can't open.
func notOthers(path string, euid int) error {
	if euid != 0 {
		return nil
	}
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
				return fmt.Errorf("%s belongs to uid %d, and this is root: a root-owned cache there would lock that user out of it", p, st.Uid)
			}
			return nil
		}
		if parent := filepath.Dir(p); parent == p {
			return nil
		}
	}
}

func notOthersHere(path string) error { return notOthers(path, os.Geteuid()) }
