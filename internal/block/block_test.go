package block

import (
	"fmt"
	"testing"
)

func TestInsertFindRemoveSortedOrder(t *testing.T) {
	b := &Block{Kind: Leaf}
	names := []string{"delta", "alpha", "charlie", "bravo"}
	for _, n := range names {
		if err := b.Insert(Entry{Name: n, UUID: n + "-uuid"}); err != nil {
			t.Fatalf("insert %q: %v", n, err)
		}
	}

	want := []string{"alpha", "bravo", "charlie", "delta"}
	if len(b.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(b.Entries), len(want))
	}
	for i, n := range want {
		if b.Entries[i].Name != n {
			t.Fatalf("entries not sorted: got %v, want %v", b.Entries, want)
		}
	}

	if err := b.Insert(Entry{Name: "alpha", UUID: "dup"}); err == nil {
		t.Fatal("expected inserting a duplicate name to fail")
	}

	e, ok := b.Find("charlie")
	if !ok || e.UUID != "charlie-uuid" {
		t.Fatalf("find charlie: got %v, %v", e, ok)
	}
	if _, ok := b.Find("missing"); ok {
		t.Fatal("expected missing name to not be found")
	}

	removed, err := b.Remove("bravo")
	if err != nil || removed.UUID != "bravo-uuid" {
		t.Fatalf("remove bravo: got %v, %v", removed, err)
	}
	if _, err := b.Remove("bravo"); err == nil {
		t.Fatal("expected removing an already-removed name to fail")
	}
}

func TestSplitLeafPreservesOrderAndPartition(t *testing.T) {
	b := &Block{Kind: Leaf}
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("file-%02d", i)
		if err := b.Insert(Entry{Name: name, UUID: name}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	left, right, sep := b.SplitLeaf()
	if len(left.Entries)+len(right.Entries) != 8 {
		t.Fatalf("split lost entries: left=%d right=%d", len(left.Entries), len(right.Entries))
	}
	for _, e := range left.Entries {
		if e.Name >= sep {
			t.Fatalf("left entry %q >= separator %q", e.Name, sep)
		}
	}
	for _, e := range right.Entries {
		if e.Name < sep {
			t.Fatalf("right entry %q < separator %q", e.Name, sep)
		}
	}
	if right.Entries[0].Name != sep {
		t.Fatalf("separator %q should equal right half's first key %q", sep, right.Entries[0].Name)
	}
}

func TestInternalNodeChildForAndInsertChild(t *testing.T) {
	b := &Block{Kind: Internal, Children: []Child{
		{MinKey: "", UUID: "left"},
	}}
	b.InsertChild(Child{MinKey: "m", UUID: "right"})

	cases := []struct {
		name string
		want string
	}{
		{"a", "left"},
		{"l", "left"},
		{"m", "right"},
		{"z", "right"},
	}
	for _, c := range cases {
		got := b.ChildFor(c.name)
		if got.UUID != c.want {
			t.Errorf("ChildFor(%q) = %q, want %q", c.name, got.UUID, c.want)
		}
	}

	// A third child, inserted out of order, must land in sorted position.
	b.InsertChild(Child{MinKey: "f", UUID: "middle"})
	want := []string{"", "f", "m"}
	for i, c := range b.Children {
		if c.MinKey != want[i] {
			t.Fatalf("children not sorted: got %+v, want MinKeys %v", b.Children, want)
		}
	}
	if got := b.ChildFor("g"); got.UUID != "middle" {
		t.Fatalf("ChildFor(%q) = %q, want %q", "g", got.UUID, "middle")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	b := &Block{Kind: Internal, Children: []Child{
		{MinKey: "", UUID: "u1"},
		{MinKey: "m", UUID: "u2"},
	}}
	data, err := b.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Kind != Internal || len(decoded.Children) != 2 {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
}

func TestDecodeEmptyIsLeaf(t *testing.T) {
	b, err := Decode(nil)
	if err != nil {
		t.Fatalf("decode nil: %v", err)
	}
	if b.Kind != Leaf || len(b.Entries) != 0 {
		t.Fatalf("got %+v, want an empty leaf", b)
	}
}
