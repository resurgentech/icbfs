// Command icbfs-cli does what isn't a filesystem operation: creating and
// managing icbfs filesystems in a bucket, naming versions, and the trash.
// The command is pkg/clicmd; this is it with the store flags of this
// module (pkg/storeflags).
package main

import (
	"os"

	"github.com/resurgentech/icbfs/pkg/clicmd"
	"github.com/resurgentech/icbfs/pkg/storeflags"
)

func main() {
	os.Exit(clicmd.Main(os.Args[1:], storeflags.Default))
}
