//go:build !windows

package objstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootRefusesAnotherUsersCacheDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a directory owned by someone other than root")
	}
	mine := filepath.Join(t.TempDir(), "not", "made", "yet", "history.db")
	if err := notOthersHere(mine); err != nil {
		t.Fatalf("my own directory, as me: %v", err)
	}
	if err := notOthers(mine, 0); err == nil {
		t.Fatal("root was let make a cache in another user's directory")
	}
	if err := notOthers("/nonexistent-root-owned/history.db", 0); err != nil {
		t.Fatalf("root, under a directory of its own: %v", err)
	}
}
