package icbfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/resurgentech/icbfs/internal/block"
	"github.com/resurgentech/icbfs/internal/objstore"
)

// countingStore counts Gets, to tell a seek from a scan.
type countingStore struct {
	objstore.Store
	gets atomic.Int64
}

func (s *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, *objstore.Object, error) {
	s.gets.Add(1)
	return s.Store.Get(ctx, key)
}

// caseVariants returns all 2^len(s) upper/lower spellings of an ASCII
// lowercase s.
func caseVariants(s string) []string {
	var out []string
	for i := 0; i < 1<<len(s); i++ {
		b := []byte(s)
		for j := range b {
			if i&(1<<j) != 0 {
				b[j] -= 'a' - 'A'
			}
		}
		out = append(out, string(b))
	}
	return out
}

// TestLookupFold checks case-insensitive lookup on both collations, in
// a directory big enough to be several levels deep, with 16 names that
// differ only in case — more than one leaf holds (testDirBlockEntries),
// so their run crosses leaf boundaries.
func TestLookupFold(t *testing.T) {
	for _, primaryWindows := range []bool{true, false} {
		t.Run(fmt.Sprintf("primaryWindows=%v", primaryWindows), func(t *testing.T) {
			store := &countingStore{Store: newTestStore(t)}
			fsys := New(store, "test")
			ctx := context.Background()
			if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, primaryWindows); err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			root := fsys.RootKey()

			variants := caseVariants("abcd")
			var names []string
			for i := 0; i < 40; i++ {
				names = append(names, fmt.Sprintf("aaa-%02d", i), fmt.Sprintf("ZZZ-%02d", i))
			}
			names = append(names, variants...)
			names = append(names, "abc", "ABCDE", "abce")
			for _, n := range names {
				if _, _, _, err := fsys.Create(ctx, root, n, 0644, 0, 0); err != nil {
					t.Fatalf("create %q: %v", n, err)
				}
			}

			// ReadDir walks the tree in order, so it shows the collation.
			before := store.gets.Load()
			entries, err := fsys.ReadDir(ctx, root)
			if err != nil {
				t.Fatalf("readdir: %v", err)
			}
			fullScan := store.gets.Load() - before
			if len(entries) != len(names) {
				t.Fatalf("readdir: %d entries, want %d", len(entries), len(names))
			}
			want := block.Exact
			if primaryWindows {
				want = block.Folded
			}
			for i := 1; i < len(entries); i++ {
				if want.Compare(entries[i-1].Name, entries[i].Name) >= 0 {
					t.Fatalf("entries out of %v order at %d: %q, %q", want, i, entries[i-1].Name, entries[i].Name)
				}
			}

			// Exact names always win, wherever they sit in the run.
			for _, v := range variants {
				e, _, err := fsys.LookupFold(ctx, root, v)
				if err != nil || e.Name != v {
					t.Fatalf("LookupFold(%q) = %q, %v; want the exact match", v, e.Name, err)
				}
				if e, _, err := fsys.Lookup(ctx, root, v); err != nil || e.Name != v {
					t.Fatalf("Lookup(%q) = %q, %v", v, e.Name, err)
				}
			}
			if e, _, err := fsys.LookupFold(ctx, root, "AAA-07"); err != nil || e.Name != "aaa-07" {
				t.Fatalf("LookupFold(AAA-07) = %q, %v; want aaa-07", e.Name, err)
			}
			if e, _, err := fsys.LookupFold(ctx, root, "zzz-39"); err != nil || e.Name != "ZZZ-39" {
				t.Fatalf("LookupFold(zzz-39) = %q, %v; want ZZZ-39", e.Name, err)
			}
			for _, missing := range []string{"abcdf", "ab", "aaa-40", "", "\xff"} {
				if _, _, err := fsys.LookupFold(ctx, root, missing); !errors.Is(err, ErrNotFound) {
					t.Fatalf("LookupFold(%q): %v, want ErrNotFound", missing, err)
				}
			}

			// Leave one variant from the middle of the run; the leaves
			// that held the rest are now empty or hold other names.
			keep := "aBcD"
			for _, v := range variants {
				if v != keep {
					if err := fsys.Unlink(ctx, root, v); err != nil {
						t.Fatalf("unlink %q: %v", v, err)
					}
				}
			}
			for _, v := range variants {
				e, _, err := fsys.LookupFold(ctx, root, v)
				if err != nil || e.Name != keep {
					t.Fatalf("after unlinks, LookupFold(%q) = %q, %v; want %q", v, e.Name, err, keep)
				}
			}
			if _, _, err := fsys.Lookup(ctx, root, "ABCD"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("exact Lookup(ABCD) after unlinking it: %v, want ErrNotFound", err)
			}

			// A primary-Windows lookup with no exact match seeks: it reads
			// a few blocks, not the whole directory like a scan does.
			before = store.gets.Load()
			if _, _, err := fsys.LookupFold(ctx, root, "abcd"); err != nil {
				t.Fatalf("LookupFold(abcd): %v", err)
			}
			lookup := store.gets.Load() - before
			if primaryWindows && lookup >= fullScan/2 {
				t.Fatalf("folded LookupFold read %d objects; a full directory read is %d — it's scanning, not seeking", lookup, fullScan)
			}
			t.Logf("full directory read: %d gets; case-mismatched LookupFold: %d gets", fullScan, lookup)

			if err := fsys.Unlink(ctx, root, keep); err != nil {
				t.Fatalf("unlink %q: %v", keep, err)
			}
			if _, _, err := fsys.LookupFold(ctx, root, "ABCD"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("LookupFold(ABCD) with no variants left: %v, want ErrNotFound", err)
			}
			for _, n := range []string{"abc", "ABCDE", "abce", "aaa-00", "ZZZ-00"} {
				if e, _, err := fsys.LookupFold(ctx, root, n); err != nil || e.Name != n {
					t.Fatalf("neighbour %q: %q, %v", n, e.Name, err)
				}
			}
		})
	}
}
