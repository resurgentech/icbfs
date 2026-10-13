package icbfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiskCacheSkipsContentBiggerThanItsBudget: caching a file bigger
// than the whole cache would only evict everything else and then itself;
// it's skipped, and an older entry for the same key is dropped.
func TestDiskCacheSkipsContentBiggerThanItsBudget(t *testing.T) {
	dir := t.TempDir()
	c := newDiskCache(dir, 10)
	write := func(name string, n int) *os.File {
		f, err := os.Create(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		return f
	}

	small := write("small", 8)
	defer small.Close()
	if err := c.updateSharedFrom("k", "v1", small, 8); err != nil {
		t.Fatalf("cache 8 bytes: %v", err)
	}
	if !c.validLocked("k", "v1") {
		t.Fatal("8-byte entry wasn't cached")
	}

	big := write("big", 20)
	defer big.Close()
	if err := c.updateSharedFrom("k", "v2", big, 20); err != nil {
		t.Fatalf("cache 20 bytes: %v", err)
	}
	if c.validLocked("k", "v1") || c.validLocked("k", "v2") {
		t.Fatal("an entry for k survived content bigger than the cache")
	}
	if c.totalBytes != 0 {
		t.Fatalf("cache holds %d bytes, want 0", c.totalBytes)
	}
	if pos, _ := big.Seek(0, 1); pos != 0 {
		t.Fatalf("content left at offset %d, want 0", pos)
	}
}
