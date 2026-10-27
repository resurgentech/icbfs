package icbfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// TestNamedVersions: a name made now shows the filesystem as it was then,
// through -as-of; a name for a past time works the same; names are listed
// in time order, unique, and can be deleted (and then reused); times
// before the filesystem or in the future, and names that read as times,
// are refused.
func TestNamedVersions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	pause := func() { time.Sleep(1100 * time.Millisecond) }

	if _, err := CreateFilesystem(ctx, store, Settings{Name: "named", Size: 1 << 30, RootMode: 0755}); err != nil {
		t.Fatal(err)
	}
	live := New(store, "named")
	if err := live.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	root := live.RootKey()
	key := writeNew(t, ctx, live, root, "f", "one")
	pause()
	one, err := CreateName(ctx, store, "named", NamedVersion{Name: "release 1", Scope: "/", CreatedBy: "uid:5"}, time.Time{})
	if err != nil {
		t.Fatalf("create name: %v", err)
	}
	if one.Time.IsZero() || one.CreatedBy != "uid:5" {
		t.Fatalf("name = %+v", one)
	}
	pause()
	afterOne, _ := store.ServerTime(ctx)
	pause()
	if _, err := live.WriteFile(ctx, key, []byte("two"), ""); err != nil {
		t.Fatal(err)
	}
	pause()
	if _, err := CreateName(ctx, store, "named", NamedVersion{Name: "between"}, afterOne); err != nil {
		t.Fatalf("create past name: %v", err)
	}

	contentAsOf := func(arg string) string {
		t.Helper()
		at, err := ResolveAsOf(ctx, store, "named", arg, time.Local)
		if err != nil {
			t.Fatalf("resolve %q: %v", arg, err)
		}
		hist, err := objstore.OpenTempHistoryCache(ctx, store, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer hist.Close()
		view, err := objstore.NewAsOf(ctx, hist, at)
		if err != nil {
			t.Fatal(err)
		}
		past := New(view, "named")
		if err := past.BootstrapReadOnly(ctx); err != nil {
			t.Fatal(err)
		}
		return readName(t, ctx, past, past.RootKey(), "f")
	}
	if got := contentAsOf("release 1"); got != "one" {
		t.Fatalf("as of release 1: %q", got)
	}
	if got := contentAsOf("between"); got != "one" {
		t.Fatalf("as of between: %q", got)
	}

	names, err := ListNames(ctx, store, "named")
	if err != nil || len(names) != 2 || names[0].Name != "release 1" || names[1].Name != "between" {
		t.Fatalf("names = %+v, %v", names, err)
	}
	if _, err := CreateName(ctx, store, "named", NamedVersion{Name: "between"}, time.Time{}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name: %v; want ErrExists", err)
	}
	for _, bad := range []string{"2026-10-12", "", "../x", "-x"} {
		if _, err := CreateName(ctx, store, "named", NamedVersion{Name: bad}, time.Time{}); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := CreateName(ctx, store, "named", NamedVersion{Name: "future"}, time.Now().Add(time.Hour)); err == nil {
		t.Error("a future time was accepted")
	}
	if _, err := CreateName(ctx, store, "named", NamedVersion{Name: "ancient"}, time.Now().Add(-24*time.Hour)); err == nil {
		t.Error("a time before the filesystem was accepted")
	}

	if err := DeleteName(ctx, store, "named", "between"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteName(ctx, store, "named", "between"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a deleted name: %v", err)
	}
	if _, err := ResolveAsOf(ctx, store, "named", "between", time.Local); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolving a deleted name: %v", err)
	}
	if _, err := CreateName(ctx, store, "named", NamedVersion{Name: "between"}, time.Time{}); err != nil {
		t.Fatalf("reusing a deleted name: %v", err)
	}
	if got := contentAsOf("between"); got != "two" {
		t.Fatalf("as of the new between: %q", got)
	}
}
