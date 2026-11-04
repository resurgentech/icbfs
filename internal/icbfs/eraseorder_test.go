package icbfs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// azureOrderStore is a Store whose EraseAll goes in Azure's order: it
// lists the prefix's history, ends every current version, then erases
// what it listed and the markers it put. A version made after the
// listing it doesn't touch, as Azure doesn't. pause, if set, runs
// between the two passes of an EraseAll of a prefix it names.
type azureOrderStore struct {
	objstore.Store
	mu    sync.Mutex
	pause map[string]func()
}

func (s *azureOrderStore) pauseOn(prefix string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pause == nil {
		s.pause = map[string]func(){}
	}
	s.pause[prefix] = fn
}

func (s *azureOrderStore) EraseAll(ctx context.Context, prefix string) error {
	listed, err := s.Store.HistoryUnder(ctx, prefix)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var current []string
	for _, v := range listed {
		if !seen[v.Key] {
			seen[v.Key] = true // newest first: the key's first entry is its current
			if !v.Deleted {
				current = append(current, v.Key)
			}
		}
	}
	var markers []objstore.Object
	for _, k := range current {
		if err := s.Store.Delete(ctx, k); err != nil && !objstore.IsNotFound(err) {
			return err
		}
		h, err := s.Store.History(ctx, k)
		if err != nil {
			return err
		}
		if len(h) > 0 && h[0].Deleted {
			markers = append(markers, h[0])
		}
	}
	s.mu.Lock()
	fn := s.pause[prefix]
	delete(s.pause, prefix)
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
	for _, v := range append(listed, markers...) {
		if err := s.Store.Erase(ctx, v.Key, v.VersionID); err != nil && !objstore.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func newAzureOrderFilesystem(t *testing.T) (*Filesystem, *azureOrderStore) {
	t.Helper()
	store := &azureOrderStore{Store: newTestStore(t)}
	f := New(store, "test")
	if err := f.Bootstrap(context.Background(), 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return f, store
}

// raceErase runs PutRef of key to target's version tv with an erase whole
// of target (started by erase, in a goroutine) landing in it: the PutRef
// waits after its check of the target until the erase has ended every
// current version under target's prefix, and the erase's second pass
// waits for the PutRef to return. It checks what the step wants: the
// PutRef refused, key not written, and nothing left under target.
func raceErase(t *testing.T, f *Filesystem, store *azureOrderStore, key, target, tv string, erase func() error) {
	t.Helper()
	ctx := context.Background()
	before, err := store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	paused, putDone, eraseDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	store.pauseOn(target, func() {
		close(paused)
		select {
		case <-putDone:
		case <-time.After(30 * time.Second):
			t.Errorf("the PutRef never returned")
		}
	})
	defer func() { testHookRefChecked = nil }()
	testHookRefChecked = func() {
		testHookRefChecked = nil
		go func() { eraseDone <- erase() }()
		select {
		case <-paused:
		case err := <-eraseDone:
			eraseDone <- err
			t.Errorf("the erase ended without an EraseAll of %s", target)
		case <-time.After(30 * time.Second):
			t.Errorf("the erase never reached its EraseAll of %s", target)
		}
	}
	_, perr := f.PutRef(ctx, key, target, tv, nil, "")
	close(putDone)
	if err := <-eraseDone; err != nil {
		t.Fatalf("the erase in the race: %v", err)
	}
	if !errors.Is(perr, ErrRefTarget) {
		t.Errorf("put ref raced against an erase of its target: %v; want ErrRefTarget", perr)
	}
	if now, err := store.Head(ctx, key); err != nil || now.VersionID != before.VersionID {
		t.Errorf("%s was written: current %+v, %v; was %s", key, now, err, before.VersionID)
	}
	if n := countVersions(t, ctx, f, target); n != 0 {
		vs, _ := store.HistoryUnder(ctx, target)
		t.Errorf("%d versions left under the target: %+v", n, vs)
	}
}

// TestPurgeRaceAzureOrder: a PutRef to a lonely target's version, past its
// check, while a purge erases the target whole on a store whose EraseAll
// ends every current version (.refs too) before it erases any: the PutRef
// is refused, and nothing of the target is left.
func TestPurgeRaceAzureOrder(t *testing.T) {
	f, store := newAzureOrderFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	target := writeNew(t, ctx, f, root, "lonely", strings.Repeat("lonely ", 100))
	tv, err := store.Head(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	c := writeNew(t, ctx, f, root, "c", "c's own")
	raceErase(t, f, store, c, target, tv.VersionID, func() error {
		return f.Purge(ctx, root, "lonely", false)
	})
}

// TestHolderRaceAzureOrder: the same race against a retired holder's
// erase: a PutRef to the version it keeps, past its check, while the
// holder's last referrer is purged.
func TestHolderRaceAzureOrder(t *testing.T) {
	f, store := newAzureOrderFilesystem(t)
	ctx := context.Background()
	root := f.RootKey()
	target := writeNew(t, ctx, f, root, "target", strings.Repeat("held ", 100))
	tv, err := store.Head(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	a := writeNew(t, ctx, f, root, "a", "a's own")
	if _, err := f.PutRef(ctx, a, target, tv.VersionID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.PurgeWithReport(ctx, root, "target", false); err != nil || len(rep.Retired) != 1 {
		t.Fatalf("purge target: %+v, %v; want it retired", rep, err)
	}
	c := writeNew(t, ctx, f, root, "c", "c's own")
	raceErase(t, f, store, c, target, tv.VersionID, func() error {
		return f.Purge(ctx, root, "a", false)
	})
}
