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
