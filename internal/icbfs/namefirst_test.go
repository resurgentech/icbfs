package icbfs

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// stopsAfter takes the next n writes once armed, then refuses every one
// after: from the store's side, a process that stopped there.
type stopsAfter struct {
	objstore.Store
	armed atomic.Bool
	left  atomic.Int32
}

var errStopped = errors.New("the process stopped")

func (s *stopsAfter) arm(n int32) {
	s.left.Store(n)
	s.armed.Store(true)
}

func (s *stopsAfter) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if s.armed.Load() && s.left.Add(-1) < 0 {
		return nil, errStopped
	}
	return s.Store.Put(ctx, key, body, md, ifMatch)
}

func stoppingFilesystem(t *testing.T) (*Filesystem, *stopsAfter) {
	t.Helper()
	store := &stopsAfter{Store: newTestStore(t)}
	f := New(store, "test")
	if err := f.Bootstrap(context.Background(), 1<<30, 0o755, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	return f, store
}

// TestRenameNewNameFirst: a rename adds the new name before it takes the
// old one away, so one that stops after its first write leaves the file
// with both names, never with neither, and says so.
func TestRenameNewNameFirst(t *testing.T) {
	f, store := stoppingFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	from, _, err := f.Mkdir(ctx, root, "from", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := f.Mkdir(ctx, root, "to", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeNew(t, ctx, f, from, "a.txt", "a")

	store.arm(1)
	if err := f.Rename(ctx, from, "a.txt", to, "a.txt", false); !errors.Is(err, errStopped) {
		t.Fatalf("a rename that stopped: %v; want it to fail", err)
	}
	store.armed.Store(false)
	for _, dir := range []string{from, to} {
		if got := readName(t, ctx, f, dir, "a.txt"); got != "a" {
			t.Fatalf("a.txt in %s: %q", dir, got)
		}
	}
}

// TestLinkNameFirst: a link adds the name before it counts it, so one
// that stops after its first write leaves the count one short (deleting
// the last counted name sends the file to the trash), never one over (the
// file would outlive its names), and says so.
func TestLinkNameFirst(t *testing.T) {
	f, store := stoppingFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	key := writeNew(t, ctx, f, root, "a.txt", "a")

	store.arm(1)
	if _, err := f.Link(ctx, root, "b.txt", key, TypeFile); !errors.Is(err, errStopped) {
		t.Fatalf("a link that stopped: %v; want it to fail", err)
	}
	store.armed.Store(false)
	if got := readName(t, ctx, f, root, "b.txt"); got != "a" {
		t.Fatalf("b.txt: %q", got)
	}
	if a, err := f.Stat(ctx, key, TypeFile); err != nil || a.Nlink != 1 {
		t.Fatalf("nlink %d (%v), want 1: one short, never one over", a.Nlink, err)
	}
}

// TestIT29_RenameAskedAgainFinishesAStoppedRename: a rename stopped
// between its two names (the item under both, counting one link) is
// finished when it is asked again: the old name goes, the item stays,
// whole, under the new one, and nothing goes to the trash
// (docs/STATES.md IT-29: a move onto an item that already has the target
// name still removes the old name). A directory likewise.
func TestIT29_RenameAskedAgainFinishesAStoppedRename(t *testing.T) {
	f, store := stoppingFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	from, _, err := f.Mkdir(ctx, root, "from", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := f.Mkdir(ctx, root, "to", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	key := writeNew(t, ctx, f, from, "a.txt", "a")
	sub, _, err := f.Mkdir(ctx, from, "sub", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "sub"} {
		store.arm(1)
		if err := f.Rename(ctx, from, name, to, name, false); !errors.Is(err, errStopped) {
			t.Fatalf("%s: a rename that stopped: %v; want it to fail", name, err)
		}
		store.armed.Store(false)
		if err := f.Rename(ctx, from, name, to, name, false); err != nil {
			t.Fatalf("%s: the rename asked again: %v", name, err)
		}
		if _, ok, err := f.findInTree(ctx, from, name); err != nil || ok {
			t.Fatalf("%s: the old name is still there (%v)", name, err)
		}
		e, ok, err := f.findInTree(ctx, to, name)
		if err != nil || !ok {
			t.Fatalf("%s: the new name is gone (%v)", name, err)
		}
		want := map[string]string{"a.txt": key, "sub": sub}[name]
		if e.UUID != want {
			t.Fatalf("%s: the new name names %s, want %s", name, e.UUID, want)
		}
	}
	if got := readName(t, ctx, f, to, "a.txt"); got != "a" {
		t.Fatalf("a.txt under its new name: %q", got)
	}
	if a, err := f.Stat(ctx, key, TypeFile); err != nil || a.Nlink != 1 {
		t.Fatalf("nlink %d (%v), want 1", a.Nlink, err)
	}
	if es, err := f.ListTrash(ctx, TrashOptions{}); err != nil || len(es) != 0 {
		t.Fatalf("the trash: %v (%v), want nothing", es, err)
	}
}
