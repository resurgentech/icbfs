//go:build windows

// Command icbfs-winfsp mounts an icbfs filesystem on Windows via WinFsp,
// the Windows counterpart to cmd/icbfs's FUSE mount. The mount is
// pkg/winfspcmd; this is it with the store flags of this module
// (pkg/storeflags).
//
// It is built as a windowed program (-ldflags -H=windowsgui), so a mount
// started at sign-in by the Windows package's task opens no console
// window; run from a prompt, it writes to that prompt's console. Without
// one, the log goes to a file (pkg/winfspcmd's -log).
package main

import (
	"os"

	"github.com/resurgentech/icbfs/internal/console"
	"github.com/resurgentech/icbfs/pkg/storeflags"
	"github.com/resurgentech/icbfs/pkg/winfspcmd"
)

func main() {
	console.UseParent()
	os.Exit(winfspcmd.Main(os.Args[1:], storeflags.Default))
}
