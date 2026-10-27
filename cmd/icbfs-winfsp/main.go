//go:build windows

// Command icbfs-winfsp mounts an icbfs filesystem on Windows via WinFsp,
// the Windows counterpart to cmd/icbfs's FUSE mount. The mount is
// pkg/winfspcmd; this is it with the store flags of this module
// (pkg/storeflags).
package main

import (
	"os"

	"github.com/resurgentech/icbfs/pkg/storeflags"
	"github.com/resurgentech/icbfs/pkg/winfspcmd"
)

func main() {
	os.Exit(winfspcmd.Main(os.Args[1:], storeflags.Default))
}
