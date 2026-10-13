package objstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// pause lets the store's clock pass a whole second, so a time taken
// after it falls strictly between the writes on either side.
func pause() { time.Sleep(1100 * time.Millisecond) }

func serverNow(t *testing.T, s Store) time.Time {
	t.Helper()
	now, err := s.ServerTime(context.Background())
	if err != nil {
		t.Fatalf("server time: %v", err)
	}
	return now
}

func TestListVersionsPaginatesAndIncludesDeleteMarkers(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	defer func(n int32) { listVersionsPageSize = n }(listVersionsPageSize)
	listVersionsPageSize = 2

	var ids []string
	for i := 0; i < 4; i++ {
		o, err := store.Put(ctx, "k", bytes.NewReader([]byte{byte('0' + i)}), nil, "")
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		ids = append(ids, o.VersionID)
	}
	if err := store.Delete(ctx, "k", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Put(ctx, "k.metadata", bytes.NewReader(nil), nil, ""); err != nil {
		t.Fatalf("put sibling: %v", err)
	}

	versions, err := store.ListVersions(ctx, "k")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 5 || !versions[0].DeleteMarker {
		t.Fatalf("got %d versions (first a delete marker: %v), want 5 with the delete first", len(versions), len(versions) > 0 && versions[0].DeleteMarker)
	}
	for i, v := range versions[1:] {
		if v.DeleteMarker || v.VersionID != ids[3-i] {
			t.Fatalf("version %d = %+v, want %s", i+1, v, ids[3-i])
		}
	}
	all, err := store.ListAllVersions(ctx, "k")
	if err != nil || len(all) != 6 {
		t.Fatalf("all versions under k: %d, %v; want 6", len(all), err)
	}
	body, obj, err := store.GetVersion(ctx, "k", ids[1])
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "1" || obj.VersionID != ids[1] {
		t.Fatalf("version %s = %q", ids[1], data)
	}
}

// TestAsOf checks the view at three moments: before a key existed,
// between two writes, and after it was deleted, plus ListByPrefix,
// read-only writes, the future check and the cache's cleanup.
func TestAsOf(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	pause()
	before := serverNow(t, store)
	pause()
	if _, err := store.Put(ctx, "p/a", bytes.NewReader([]byte("one")), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "p/gone", bytes.NewReader([]byte("x")), nil, ""); err != nil {
		t.Fatal(err)
	}
	pause()
	middle := serverNow(t, store)
	pause()
	if _, err := store.Put(ctx, "p/a", bytes.NewReader([]byte("two!")), nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "p/gone", ""); err != nil {
		t.Fatal(err)
	}
	pause()
	after := serverNow(t, store)

	read := func(v *AsOf, key string) (string, error) {
		body, _, err := v.Get(ctx, key)
		if err != nil {
			return "", err
		}
		defer body.Close()
		data, err := io.ReadAll(body)
		return string(data), err
	}
	for _, tc := range []struct {
		at         time.Time
		a, gone    string // "" = didn't exist
		listed     int
		listedSize int64
	}{
		{before, "", "", 0, 0},
		{middle, "one", "x", 2, 4},
		{after, "two!", "", 1, 4},
	} {
		v, err := NewAsOf(ctx, store, tc.at, t.TempDir())
		if err != nil {
			t.Fatalf("as of %v: %v", tc.at, err)
		}
		for key, want := range map[string]string{"p/a": tc.a, "p/gone": tc.gone} {
			for round := 0; round < 2; round++ { // the second answer comes from the cache
				got, err := read(v, key)
				if want == "" {
					if !IsNotFound(err) {
						t.Fatalf("as of %v, %s (round %d): %q, %v; want not found", tc.at, key, round, got, err)
					}
					if _, err := v.Head(ctx, key); !IsNotFound(err) {
						t.Fatalf("as of %v, head %s: %v; want not found", tc.at, key, err)
					}
				} else if got != want || err != nil {
					t.Fatalf("as of %v, %s (round %d): %q, %v; want %q", tc.at, key, round, got, err, want)
				}
			}
		}
		listed, err := v.ListByPrefix(ctx, "p/")
		var size int64
		for _, o := range listed {
			size += o.Size
		}
		if err != nil || len(listed) != tc.listed || size != tc.listedSize {
			t.Fatalf("as of %v, listing: %d objects, %d bytes, %v; want %d, %d", tc.at, len(listed), size, err, tc.listed, tc.listedSize)
		}
		if _, err := v.Put(ctx, "p/a", bytes.NewReader(nil), nil, ""); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("put through an as-of view: %v", err)
		}
		if err := v.Delete(ctx, "p/a", ""); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("delete through an as-of view: %v", err)
		}
		if err := v.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := os.Stat(v.dir); !os.IsNotExist(err) {
			t.Fatalf("cache dir still there after Close: %v", err)
		}
	}

	if _, err := NewAsOf(ctx, store, after.Add(time.Hour), t.TempDir()); err == nil {
		t.Fatal("an as-of time in the future was accepted")
	}
}

func TestAsOfRemovesCachesOfDeadProcesses(t *testing.T) {
	store := newTestStore(t)
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
	v, err := NewAsOf(ctx, store, serverNow(t, store), parent)
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
