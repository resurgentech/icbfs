package fuseserver

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// TestNeverReadAtimeIsCreation: an item never read reports its creation
// time as atime, so a later change to it (a new entry in a directory)
// doesn't look like an access (xfstests' generic/003).
func TestNeverReadAtimeIsCreation(t *testing.T) {
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	key := id.String()
	created := icbfs.Btime(key)
	var out fuse.Attr
	fillAttr(&out, key, icbfs.TypeDir, icbfs.Attr{Mtime: created.Add(time.Hour)})
	if got := time.Unix(int64(out.Atime), int64(out.Atimensec)); !got.Equal(created) {
		t.Fatalf("atime %v, want the creation time %v", got, created)
	}
}
