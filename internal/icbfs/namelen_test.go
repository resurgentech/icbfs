package icbfs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestNameLengthOperations checks that every operation taking a name
// refuses one over the limit with ErrNameTooLong — before looking it up,
// so a missing too-long name is "too long", not "not found" (pjdfstest's
// */02.t) — and that a primary-Windows filesystem takes what NTFS takes.
func TestNameLengthOperations(t *testing.T) {
	for _, primaryWindows := range []bool{false, true} {
		t.Run(fmt.Sprintf("primaryWindows=%v", primaryWindows), func(t *testing.T) {
			store := newTestStore(t)
			fsys := New(store, "test")
			ctx := context.Background()
			if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, primaryWindows); err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			root := fsys.RootKey()
			tooLong := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, ErrNameTooLong) {
					t.Fatalf("%s: err = %v, want ErrNameTooLong", what, err)
				}
			}

			max, over := strings.Repeat("a", 255), strings.Repeat("a", 256)
			key, _, _, err := fsys.Create(ctx, root, max, 0644, 0, 0)
			if err != nil {
				t.Fatalf("create a 255-character name: %v", err)
			}
			if _, _, err := fsys.Lookup(ctx, root, max); err != nil {
				t.Fatalf("lookup a 255-character name: %v", err)
			}

			_, _, _, err = fsys.Create(ctx, root, over, 0644, 0, 0)
			tooLong("create", err)
			_, _, err = fsys.Mkdir(ctx, root, over, 0755, 0, 0)
			tooLong("mkdir", err)
			_, _, err = fsys.Symlink(ctx, root, over, "target", 0, 0)
			tooLong("symlink", err)
			_, _, err = fsys.Mknod(ctx, root, over, 0644, TypeFifo, 0, 0, 0)
			tooLong("mknod", err)
			_, err = fsys.Link(ctx, root, over, key, TypeFile)
			tooLong("link", err)
			_, _, err = fsys.Lookup(ctx, root, over)
			tooLong("lookup", err)
			_, _, err = fsys.LookupFold(ctx, root, over)
			tooLong("case-insensitive lookup", err)
			tooLong("unlink", fsys.Unlink(ctx, root, over))
			tooLong("rmdir", fsys.Rmdir(ctx, root, over))
			tooLong("rename from", fsys.Rename(ctx, root, over, root, "x", true))
			tooLong("rename to", fsys.Rename(ctx, root, max, root, over, true))
			if _, _, err := fsys.Lookup(ctx, root, max); err != nil {
				t.Fatalf("the 255-character file after a refused rename: %v", err)
			}

			// 255 CJK characters: 765 bytes, 255 UTF-16 units.
			wide := strings.Repeat("名", 255)
			_, _, _, err = fsys.Create(ctx, root, wide, 0644, 0, 0)
			if primaryWindows {
				if err != nil {
					t.Fatalf("primary-Windows create of 255 CJK characters: %v", err)
				}
				if _, _, err := fsys.Lookup(ctx, root, wide); err != nil {
					t.Fatalf("lookup of 255 CJK characters: %v", err)
				}
				_, _, _, err = fsys.Create(ctx, root, wide+"名", 0644, 0, 0)
				tooLong("primary-Windows create of 256 CJK characters", err)
			} else {
				tooLong("primary-POSIX create of 765 bytes", err)
			}
		})
	}
}
