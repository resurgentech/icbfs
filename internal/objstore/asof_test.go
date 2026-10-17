package objstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The as-of view's behaviour on a real store is the conformance suite's
// (objstore/storetest); this is its local cache handling.

// clockStore is a store that only tells the time.
type clockStore struct{ Store }

func (clockStore) ServerTime(context.Context) (time.Time, error) { return time.Now(), nil }

func TestAsOfRemovesCachesOfDeadProcesses(t *testing.T) {
	store := clockStore{}
	ctx := context.Background()
	parent := t.TempDir()

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(parent, asOfCachePrefix+"dead")
	live := filepath.Join(parent, asOfCachePrefix+"live")
	for dir, pid := range map[string]int{dead: cmd.Process.Pid, live: os.Getppid()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v, err := NewAsOf(ctx, store, time.Now(), parent)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("a dead process's cache survived: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a live process's cache was removed: %v", err)
	}
}
