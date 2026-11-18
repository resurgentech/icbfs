package icbfs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// Proposed rows (the report root leftovers, 2026-11-18): a filesystem is
// never created on top of history a store kept under its fixed names.

// madeAndUsed creates filesystem name in store and writes a folder and a
// file into its root, as any first use does.
func madeAndUsed(t *testing.T, store objstore.Store, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := CreateFilesystem(ctx, store, DefaultSettings(name)); err != nil {
		t.Fatal(err)
	}
	f := New(store, name)
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Mkdir(ctx, f.RootKey(), "Matters", 0o755, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.Create(ctx, f.RootKey(), "a.txt", 0o644, 0, 0); err != nil {
		t.Fatal(err)
	}
}

// removedOutside deletes every current object under prefix the way a
// tool outside icbfs does (azcopy remove, aws s3 rm, a console): each key
// ends, and the store keeps every version.
func removedOutside(t *testing.T, store objstore.Store, prefix string) int {
	t.Helper()
	ctx := context.Background()
	all, err := store.List(ctx, prefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if err := store.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}
	return len(all)
}

// historyCount is how many entries (versions and deletions) the whole
// bucket's history holds.
func historyCount(t *testing.T, store objstore.Store) int {
	t.Helper()
	h, err := store.HistoryUnder(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(h)
}

// TestFC1_CreateWhereTheStoreKeptAnEarlierFilesystemsVersionsIsRefused:
// every current object of the bucket removed outside icbfs (the master
// block too), a filesystem created by the same name, or by another (the
// slot is the same, 0000, so are the prefix and its fixed names), is
// refused before anything is written, with a message that says what is
// there and the ways out.
func TestFC1_CreateWhereTheStoreKeptAnEarlierFilesystemsVersionsIsRefused(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	madeAndUsed(t, store, "clio")
	removedOutside(t, store, "")
	before := historyCount(t, store)
	for _, name := range []string{"clio", "other"} {
		_, err := CreateFilesystem(ctx, store, DefaultSettings(name))
		if !errors.Is(err, ErrLeftovers) {
			t.Fatalf("create %q over the leftovers: %v; want ErrLeftovers", name, err)
		}
		for _, want := range []string{`"0000-"`, "0000-root-clio", "removed outside icbfs", "nothing was written", "empty container"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("create %q: the message lacks %q: %v", name, want, err)
			}
		}
		t.Logf("create %q: %v", name, err)
	}
	if after := historyCount(t, store); after != before {
		t.Fatalf("the refused creates wrote: the history held %d entries, now %d", before, after)
	}
	if all, err := ListFilesystems(ctx, store); err != nil || len(all) != 0 {
		t.Fatalf("filesystems after the refusals: %+v, %v; want none", all, err)
	}
}

// TestFC2_CreateOfARegisteredNameWhoseRootWasRemovedOutsideIsRefused:
// the master block kept, the filesystem's own objects removed outside
// icbfs: a create of its name (which today finishes "an interrupted
// create" by writing a root on top of the old one's history) is refused,
// nothing written.
func TestFC2_CreateOfARegisteredNameWhoseRootWasRemovedOutsideIsRefused(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	madeAndUsed(t, store, "clio")
	removedOutside(t, store, "0000-")
	before := historyCount(t, store)
	_, err := CreateFilesystem(ctx, store, DefaultSettings("clio"))
	if !errors.Is(err, ErrLeftovers) {
		t.Fatalf("create over a removed root: %v; want ErrLeftovers", err)
	}
	for _, want := range []string{"0000-root-clio", "fs archive", "fs prune", "another name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message lacks %q: %v", want, err)
		}
	}
	t.Logf("%v", err)
	if after := historyCount(t, store); after != before {
		t.Fatalf("the refused create wrote: %d entries before, %d after", before, after)
	}
}

// TestFC3_AnInterruptedCreateStillFinishes: a name registered whose root
// never landed (the create was cut after the master block's write) is
// finished by the next create of the name, as before.
func TestFC3_AnInterruptedCreateStillFinishes(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	s := DefaultSettings("clio")
	if _, _, _, err := registerFilesystem(ctx, store, entryOf(s)); err != nil {
		t.Fatal(err)
	}
	if err := New(store, "clio").Attach(ctx); err == nil {
		t.Fatal("attach before the root landed: want an error")
	}
	if _, err := CreateFilesystem(ctx, store, s); err != nil {
		t.Fatalf("the create finishing an interrupted one: %v", err)
	}
	if err := New(store, "clio").Attach(ctx); err != nil {
		t.Fatalf("attach after: %v", err)
	}
}

// TestFC4_AfterIcbfsOwnEraseTheNameIsCreatedClean: archive and prune
// (icbfs's own erase) leave nothing under the filesystem's prefix, so a
// create of the same name succeeds and its root's history is its own.
func TestFC4_AfterIcbfsOwnEraseTheNameIsCreatedClean(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	madeAndUsed(t, store, "clio")
	if err := Archive(ctx, store, "clio"); err != nil {
		t.Fatal(err)
	}
	if err := Prune(ctx, store, "clio"); err != nil {
		t.Fatal(err)
	}
	if h, err := store.HistoryUnder(ctx, "0000-"); err != nil || len(h) != 0 {
		t.Fatalf("history under 0000- after the prune: %d entries, %v; want none", len(h), err)
	}
	info, err := CreateFilesystem(ctx, store, DefaultSettings("clio"))
	if err != nil || info.ID != "0000" {
		t.Fatalf("create after the prune: %+v, %v; want slot 0000", info, err)
	}
	f := New(store, "clio")
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := store.History(ctx, f.RootKey())
	if err != nil || len(h) != 1 {
		t.Fatalf("the new root's history: %+v, %v; want its creation only", h, err)
	}
	if left, err := f.RootLeftovers(ctx); err != nil || len(left) != 0 {
		t.Fatalf("leftovers found on a clean filesystem: %+v, %v", left, err)
	}
}

// historyFails fails every history listing, and has no sampler of its
// own: what the creation's look at the prefix meets on a store error.
type historyFails struct{ objstore.Store }

var errHistory = errors.New("the listing failed")

func (s historyFails) HistoryUnder(context.Context, string) ([]objstore.Object, error) {
	return nil, errHistory
}

// TestFC5_AStoreErrorOnTheLookIsTheCreatesError: the look at the slot's
// prefix fails: the create fails with that error and writes nothing (no
// master block in a fresh bucket).
func TestFC5_AStoreErrorOnTheLookIsTheCreatesError(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := CreateFilesystem(ctx, historyFails{store}, DefaultSettings("clio")); !errors.Is(err, errHistory) {
		t.Fatalf("create with the listing failing: %v; want its error", err)
	}
	if n := historyCount(t, store); n != 0 {
		t.Fatalf("the failed create wrote %d entries; want none", n)
	}
}
