package icbfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiskCacheSkipsContentBiggerThanItsBudget: caching a file bigger
// than the whole cache would only evict everything else and then itself;
// it's skipped. An entry of another version of the same key stays: its
// bytes are still exactly that version's (pkg/readcache keys by version).
func TestDiskCacheSkipsContentBiggerThanItsBudget(t *testing.T) {
	c := newDiskCache(filepath.Join(t.TempDir(), "c"), 10)
	t.Cleanup(func() { c.close() })
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
	if !c.rc.Has("k", "v1") {
		t.Fatal("8-byte entry wasn't cached")
	}

	big := write("big", 20)
	defer big.Close()
	if err := c.updateSharedFrom("k", "v2", big, 20); err != nil {
		t.Fatalf("cache 20 bytes: %v", err)
	}
	if c.rc.Has("k", "v2") {
		t.Fatal("content bigger than the cache was cached")
	}
	if !c.rc.Has("k", "v1") || c.rc.Stats().Bytes != 8 {
		t.Fatalf("the 8-byte entry of v1: %v, %d bytes", c.rc.Has("k", "v1"), c.rc.Stats().Bytes)
	}
	if pos, _ := big.Seek(0, 1); pos != 0 {
		t.Fatalf("content left at offset %d, want 0", pos)
	}
}
