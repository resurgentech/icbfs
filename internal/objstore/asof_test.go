package objstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The as-of view's and the history cache's behaviour on a real store is
// the conformance suite's (objstore/storetest); this is the temporary
// cache's cleanup.

// clockStore is a store that only tells the time.
type clockStore struct{ Store }

func (clockStore) ServerTime(context.Context) (time.Time, error) { return time.Now(), nil }

func TestTempHistoryCacheRemovesCachesOfDeadProcesses(t *testing.T) {
	store := clockStore{}
	ctx := context.Background()
	parent := t.TempDir()

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(parent, tempCachePrefix+"dead")
	live := filepath.Join(parent, tempCachePrefix+"live")
	for dir, pid := range map[string]int{dead: cmd.Process.Pid, live: os.Getppid()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := OpenTempHistoryCache(ctx, store, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("a dead process's cache survived: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a live process's cache was removed: %v", err)
	}
}

// fixedStore is a store with one key, "k", written once a minute ago.
type fixedStore struct{ clockStore }

func (fixedStore) History(_ context.Context, key string) ([]Object, error) {
	if key != "k" {
		return nil, nil
	}
	return []Object{{Key: "k", VersionID: "v1", LastModified: time.Now().Add(-time.Minute)}}, nil
}

func TestHistoryCacheSharedByManyHandles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		go func() {
			errs <- func() error {
				c, err := OpenHistoryCache(ctx, fixedStore{}, path, time.Hour)
				if err != nil {
					return err
				}
				defer c.Close()
				for j := 0; j < 20; j++ {
					if err := c.forget(ctx, "k"); err != nil {
						return err
					}
					h, err := c.history(ctx, "k", time.Now().Add(-time.Second))
					if err != nil {
						return err
					}
					if len(h) != 1 || h[0].VersionID != "v1" {
						return fmt.Errorf("history %v", h)
					}
				}
				return nil
			}()
		}()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
