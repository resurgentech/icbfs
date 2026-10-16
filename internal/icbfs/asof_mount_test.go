package icbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// TestAsOfFilesystem mounts a filesystem read-only as of earlier
// moments and checks it shows exactly what the live filesystem showed
// then: contents, renames, deletes, a multi-level directory, attributes
// and disk usage; that it doesn't exist before it was created; and that
// it refuses writes.
func TestAsOfFilesystem(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := func() time.Time {
		t.Helper()
		n, err := store.ServerTime(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	pause := func() { time.Sleep(1100 * time.Millisecond) }

	pause()
	beforeCreation := now()
	pause()

	live := New(store, "past")
	if err := live.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	root := live.RootKey()
	dir, _, err := live.Mkdir(ctx, root, "d", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, _, _, err := live.Create(ctx, dir, "a", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := live.WriteFile(ctx, a, []byte("one"), ""); err != nil {
		t.Fatal(err)
	}
	big, _, err := live.Mkdir(ctx, root, "big", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3*testDirBlockEntries; i++ {
		if _, _, _, err := live.Create(ctx, big, fmt.Sprintf("n%02d", i), 0644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}

	type snapshot struct {
		at    time.Time
		names map[string][]string // directory -> sorted names
		files map[string]string   // path -> content
		mode  uint32              // d/a or d/b's mode
		used  int64
	}
	listing := func(f *Filesystem, keys map[string]string) map[string][]string {
		t.Helper()
		out := map[string][]string{}
		for name, key := range keys {
			entries, err := f.ReadDir(ctx, key)
			if err != nil {
				t.Fatalf("readdir %s: %v", name, err)
			}
			for _, e := range entries {
				out[name] = append(out[name], e.Name)
			}
			sort.Strings(out[name])
		}
		return out
	}
	dirs := map[string]string{"/": root, "d": dir, "big": big}
	take := func(file string) snapshot {
		t.Helper()
		pause()
		used, err := live.DiskUsage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e, attr, err := live.Lookup(ctx, dir, file)
		if err != nil {
			t.Fatal(err)
		}
		data, _, _, err := live.ReadFile(ctx, e.UUID)
		if err != nil {
			t.Fatal(err)
		}
		s := snapshot{at: now(), names: listing(live, dirs), files: map[string]string{"d/" + file: string(data)}, mode: attr.Mode, used: used}
		pause()
		return s
	}
	first := take("a")

	// Change everything.
	if _, _, err := live.WriteFile(ctx, a, []byte("two, longer"), ""); err != nil {
		t.Fatal(err)
	}
	mode := uint32(0600)
	if _, err := live.SetAttr(ctx, a, TypeFile, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := live.Rename(ctx, dir, "a", dir, "b", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2*testDirBlockEntries; i++ {
		if err := live.Unlink(ctx, big, fmt.Sprintf("n%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := live.Create(ctx, root, "later", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	second := take("b")

	open := func(at time.Time) (*Filesystem, *objstore.AsOf, error) {
		view, err := objstore.NewAsOf(ctx, store, at, t.TempDir())
		if err != nil {
			t.Fatalf("as of %v: %v", at, err)
		}
		f := New(view, "past")
		return f, view, f.BootstrapReadOnly(ctx)
	}

	if _, view, err := open(beforeCreation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("as of before creation: %v, want ErrNotFound", err)
	} else {
		view.Close()
	}

	for _, s := range []snapshot{first, second} {
		f, view, err := open(s.at)
		if err != nil {
			t.Fatalf("as of %v: %v", s.at, err)
		}
		if got := listing(f, dirs); fmt.Sprint(got) != fmt.Sprint(s.names) {
			t.Fatalf("as of %v, directories:\n got  %v\n want %v", s.at, got, s.names)
		}
		for path, want := range s.files {
			e, attr, err := f.Lookup(ctx, dir, path[len("d/"):])
			if err != nil {
				t.Fatalf("as of %v, lookup %s: %v", s.at, path, err)
			}
			data, _, _, err := f.ReadFile(ctx, e.UUID)
			if err != nil || string(data) != want || attr.Mode != s.mode {
				t.Fatalf("as of %v, %s = %q mode %o, %v; want %q mode %o", s.at, path, data, attr.Mode, err, want, s.mode)
			}
			// Through an open session too, as the mounts read.
			o, err := f.Open(ctx, e.UUID)
			if err != nil {
				t.Fatalf("as of %v, open %s: %v", s.at, path, err)
			}
			if got := string(o.ReadAt(0, 100)); got != want {
				t.Fatalf("as of %v, read %s = %q, want %q", s.at, path, got, want)
			}
			o.Close()
		}
		used, err := f.DiskUsage(ctx)
		if err != nil || used != s.used {
			t.Fatalf("as of %v, du = %d, %v; want %d", s.at, used, err, s.used)
		}
		if _, used2, err := f.StatFS(ctx); err != nil || int64(used2) != s.used {
			t.Fatalf("as of %v, df used = %d, %v; want %d", s.at, used2, err, s.used)
		}

		// Read-only.
		if _, _, _, err := f.Create(ctx, f.RootKey(), "new", 0644, 0, 0); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("create in the past: %v", err)
		}
		if _, err := f.SetAttr(ctx, dir, TypeDir, &mode, nil, nil, nil, nil, nil, nil); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("chmod in the past: %v", err)
		}
		if err := f.TouchAtime(ctx, dir, TypeDir); err != nil {
			t.Fatalf("a read in the past tried to record atime: %v", err)
		}
		view.Close()
	}

	// The multi-level directory really was multi-level at first.
	body, _, err := store.Get(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if blk, err := block.Decode(data); err != nil || blk.Kind != block.Internal {
		t.Fatalf("big's root block isn't internal (%v): the test didn't cover a multi-level directory", err)
	}
}
