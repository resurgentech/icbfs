package icbfs

import (
	"context"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// TestForgetChangedPastClearsHistoryCaches: a history cache keeps a
// directory's attributes and blocks, which name what was in it; after a
// purge marks the filesystem, ForgetChangedPast drops them, and a past view
// finds nothing there, as it would with no cache.
func TestForgetChangedPastClearsHistoryCaches(t *testing.T) {
	f, store := newTestFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	dir, _, err := f.Mkdir(ctx, root, "d", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeNew(t, ctx, f, dir, "secret.txt", "x")
	time.Sleep(1100 * time.Millisecond)
	at, err := store.ServerTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	hist, err := objstore.OpenTempHistoryCache(ctx, store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	listDir := func() ([]string, error) {
		t.Helper()
		view, err := objstore.NewAsOf(ctx, hist, at)
		if err != nil {
			t.Fatal(err)
		}
		past := New(view, "test")
		if err := past.BootstrapReadOnly(ctx); err != nil {
			t.Fatal(err)
		}
		e, _, err := past.Lookup(ctx, past.RootKey(), "d")
		if err != nil {
			return nil, err
		}
		entries, err := past.ReadDir(ctx, e.UUID)
		var names []string
		for _, c := range entries {
			names = append(names, c.Name)
		}
		return names, err
	}
	if names, err := listDir(); err != nil || len(names) != 1 {
		t.Fatalf("before the purge: %v, %v", names, err)
	}
	if _, err := ForgetChangedPast(ctx, f, hist); err != nil {
		t.Fatalf("with no purge yet: %v", err)
	}

	if err := f.Purge(ctx, root, "d", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ForgetChangedPast(ctx, f, hist); err != nil {
		t.Fatal(err)
	}
	if names, err := listDir(); err == nil {
		t.Fatalf("after the purge, the directory as it was lists %v", names)
	}
	if note, err := hist.Note(ctx, "past:"+f.id); err != nil || note == "" {
		t.Fatalf("the purge mark seen: %q, %v", note, err)
	}
}
