//go:build !windows

package folderside

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/resurgentech/icbfs/internal/syncer"
)

// aclsSupported: ACLs are Windows'.
const aclsSupported = false

// probe: inode numbers are stable across renames on a local disk.
func probe(root string) syncer.Caps {
	return syncer.Caps{IDs: true, TimeResolution: time.Nanosecond}
}

func fileID(p string, info fs.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	return ""
}

func readACL(string) string { return "" }

func writeACL(string, string) error { return nil }

func openShared(p string) (io.ReadCloser, error) { return os.Open(p) }

func replace(from, to string) error { return os.Rename(from, to) }

// created is zero: a Linux or macOS folder's creation times can't be set,
// so they aren't copied (Caps.Created is false).
func created(string, fs.FileInfo) time.Time { return time.Time{} }

func setCreated(string, time.Time) error { return nil }
