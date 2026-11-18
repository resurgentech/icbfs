package icbfs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestZZFanout: creates, lookups, a listing and unlinks in one folder of
// many blocks, timed and logged. 2,000 files takes about three minutes
// under -race, so just ci runs it at 200 (25 blocks of 8, three levels)
// and just pre-merge (ICBFS_PRE_MERGE=1) at 2,000.
func TestZZFanout(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	dir, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "d", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 200
	if os.Getenv("ICBFS_PRE_MERGE") != "" {
		n = 2000
	}
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, _, _, err := fsys.Create(ctx, dir, fmt.Sprintf("file-%05d", i), 0644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	c := time.Since(start) / time.Duration(n)
	start = time.Now()
	for i := 0; i < n; i++ {
		if _, _, err := fsys.Lookup(ctx, dir, fmt.Sprintf("file-%05d", (i*7919)%n)); err != nil {
			t.Fatal(err)
		}
	}
	l := time.Since(start) / time.Duration(n)
	start = time.Now()
	for r := 0; r < 5; r++ {
		if _, err := fsys.ReadDir(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}
	rd := time.Since(start) / 5
	start = time.Now()
	for i := 0; i < n/4; i++ {
		if err := fsys.Unlink(ctx, dir, fmt.Sprintf("file-%05d", i*4)); err != nil {
			t.Fatal(err)
		}
	}
	u := time.Since(start) / time.Duration(n/4)
	t.Logf("fanout %d: create %v lookup %v readdir(%d) %v unlink %v", testDirBlockEntries, c, l, n, rd, u)
}
