package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCCE15_IcbfsMountWarnsOnlyOnAnOpenCacheDir: icbfs mount is exempt
// from the owner-only cache directory check icbfs-gitd makes: a
// -cache-dir others can write is used as before, with one warning line
// naming it and why; its own or a missing one gets none.
func TestCCE15_IcbfsMountWarnsOnlyOnAnOpenCacheDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	w := cacheDirWarning(open)
	if !strings.Contains(w, open) || !strings.Contains(w, "0777") || !strings.Contains(w, "used anyway") {
		t.Fatalf("warning for a 0777 directory: %q", w)
	}
	own := filepath.Join(t.TempDir(), "own")
	if err := os.Mkdir(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if w := cacheDirWarning(own); w != "" {
		t.Fatalf("warning for the user's 0700 directory: %q", w)
	}
	if w := cacheDirWarning(filepath.Join(own, "missing")); w != "" {
		t.Fatalf("warning for a directory not made yet: %q", w)
	}
}
