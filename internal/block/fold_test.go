package block

import (
	"fmt"
	"sort"
	"testing"
)

func TestCompareFoldMatchesNTFS(t *testing.T) {
	same := [][2]string{
		{"readme.txt", "README.TXT"},
		{"ReadMe.Txt", "rEADmE.tXT"},
		{"straße", "STRAßE"}, // ß has no single-character uppercase, so it only matches itself
		{"ǅ", "Ǆ"},           // titlecase digraph folds to its uppercase
		{"ı", "I"},           // dotless i, as NTFS's table has it
		{"ſ", "S"},           // long s
		{"ÿ", "Ÿ"},           // a lowercase whose uppercase is outside Latin-1
		{"Ωmega", "ωMEGA"},
		{"😀a", "😀A"}, // outside the BMP: kept as is, the rest still folds
		{"", ""},
	}
	for _, p := range same {
		if CompareFold(p[0], p[1]) != 0 || CompareFold(p[1], p[0]) != 0 {
			t.Errorf("CompareFold(%q, %q) != 0, want a case-insensitive match", p[0], p[1])
		}
	}
	different := [][2]string{
		{"straße", "STRASSE"}, // no multi-character folding, same as NTFS
		{"a", "b"},
		{"a", "aa"},
		{"K", "K"}, // KELVIN SIGN is already uppercase: Go's EqualFold would match it, NTFS doesn't
		{"\xff", "\xfe"},
		{"\xc3", "é"}, // a stray byte never matches a character starting with it
		{"\U0001d41a", "\U0001d400"}, // mathematical bold a/A: outside the BMP, no case
	}
	for _, p := range different {
		if CompareFold(p[0], p[1]) == 0 {
			t.Errorf("CompareFold(%q, %q) == 0, want different names", p[0], p[1])
		}
		if CompareFold(p[0], p[1]) != -CompareFold(p[1], p[0]) {
			t.Errorf("CompareFold(%q, %q) isn't antisymmetric", p[0], p[1])
		}
	}
}

func TestUpcaseTableIsPinnedAndIdempotent(t *testing.T) {
	// The table must not drift: folded directories on disk are sorted by
	// it. Regenerating it from a newer Unicode is a format change.
	if n := len(upcasePairs); n != 1198 {
		t.Fatalf("upcase table has %d mappings, want the pinned 1198", n)
	}
	for _, p := range upcasePairs {
		if upcase[p[1]] != 0 {
			t.Errorf("U+%04X folds to U+%04X, which folds again to U+%04X", p[0], p[1], upcase[p[1]])
		}
	}
}

// foldedNames is every case variant of "abc" plus names sorting around
// them, shuffled the way a directory would receive them.
func foldedNames() []string {
	var names []string
	for i := 0; i < 8; i++ {
		b := []byte("abc")
		for j := 0; j < 3; j++ {
			if i&(1<<j) != 0 {
				b[j] -= 'a' - 'A'
			}
		}
		names = append(names, string(b))
	}
	names = append(names, "ab", "ABB", "abd", "Ab", "abcd", "ABCA", "a", "B")
	sort.Slice(names, func(i, j int) bool { return (i*7)%5 < (j*7)%5 })
	return names
}

func TestFoldedLeafKeepsCaseVariantsContiguous(t *testing.T) {
	b := &Block{Kind: Leaf, Collation: Folded}
	for _, n := range foldedNames() {
		if err := b.Insert(Entry{Name: n}); err != nil {
			t.Fatalf("insert %q: %v", n, err)
		}
	}
	for i := 1; i < len(b.Entries); i++ {
		if Folded.Compare(b.Entries[i-1].Name, b.Entries[i].Name) >= 0 {
			t.Fatalf("entries out of order at %d: %q, %q", i, b.Entries[i-1].Name, b.Entries[i].Name)
		}
	}
	start := b.SeekFold("ABC")
	n := 0
	for _, e := range b.Entries[start:] {
		if !EqualFold(e.Name, "abc") {
			break
		}
		n++
	}
	if n != 8 {
		t.Fatalf("found %d contiguous case variants of abc from SeekFold, want 8 (entries %v)", n, b.Entries)
	}
	// Exact operations still work on every name in a folded leaf.
	for _, name := range foldedNames() {
		if _, ok := b.Find(name); !ok {
			t.Fatalf("find %q in folded leaf failed", name)
		}
	}
	if _, ok := b.Find("ABc "); ok {
		t.Fatal("found a name that isn't there")
	}
	if err := b.Insert(Entry{Name: "aBc"}); err == nil {
		t.Fatal("inserting an exact duplicate into a folded leaf should fail")
	}
	if _, err := b.Remove("AbC"); err != nil {
		t.Fatalf("remove AbC: %v", err)
	}
	if _, ok := b.Find("AbC"); ok {
		t.Fatal("AbC still there after remove")
	}
}

func TestChildIndexFold(t *testing.T) {
	b := &Block{Kind: Internal, Collation: Folded, Children: []Child{
		{MinKey: "", UUID: "c0"},
		{MinKey: "ABC", UUID: "c1"}, // first variant of abc starts here
		{MinKey: "abC", UUID: "c2"}, // the run continues
		{MinKey: "abd", UUID: "c3"},
	}}
	for _, tc := range []struct {
		name string
		want int
	}{
		{"abc", 0}, // the run could start at the very end of c0
		{"ABB", 0},
		{"abca", 2},
		{"ABD", 2}, // abd itself starts c3, but a variant sorting before it ("ABD") would be in c2
		{"zzz", 3},
		{"", 0},
	} {
		if got := b.ChildIndexFold(tc.name); got != tc.want {
			t.Errorf("ChildIndexFold(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestFoldedSplitsKeepTheirCollation(t *testing.T) {
	b := &Block{Kind: Leaf, Collation: Folded}
	for i := 0; i < 8; i++ {
		_ = b.Insert(Entry{Name: fmt.Sprintf("n%d", i)})
	}
	l, r, _ := b.SplitLeaf()
	if l.Collation != Folded || r.Collation != Folded {
		t.Fatal("SplitLeaf dropped the collation")
	}
	in := &Block{Kind: Internal, Collation: Folded, Children: []Child{{}, {MinKey: "b"}, {MinKey: "c"}, {MinKey: "d"}}}
	l, r, _ = in.SplitInternal()
	if l.Collation != Folded || r.Collation != Folded {
		t.Fatal("SplitInternal dropped the collation")
	}
}
