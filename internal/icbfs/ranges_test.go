package icbfs

import (
	"reflect"
	"testing"
)

func TestByteRangesAddJoinsOverlappingAndTouching(t *testing.T) {
	var b byteRanges
	b.add(10, 20)
	b.add(40, 50)
	b.add(0, 5)
	b.add(5, 6)   // touches [0,5): joins it
	b.add(15, 42) // overlaps both [10,20) and [40,50)
	b.add(60, 60) // empty: ignored
	want := [][2]int64{{0, 6}, {10, 50}}
	if got := b.ranges(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges %v, want %v", got, want)
	}
	if lo, hi := b.span(); lo != 0 || hi != 50 {
		t.Fatalf("span [%d,%d), want [0,50)", lo, hi)
	}
}

func TestByteRangesOverlaps(t *testing.T) {
	var b byteRanges
	b.add(0, 10)
	b.add(1000, 1010)
	for _, tc := range []struct {
		lo, hi int64
		want   bool
	}{
		{500, 600, false}, // between them: the point of keeping them apart
		{5, 6, true},
		{10, 1000, false}, // ends are exclusive
		{9, 11, true},
		{1009, 2000, true},
		{2000, 3000, false},
	} {
		if got := b.overlaps(tc.lo, tc.hi); got != tc.want {
			t.Errorf("overlaps(%d,%d) = %v, want %v", tc.lo, tc.hi, got, tc.want)
		}
	}
}

func TestByteRangesBoundedByJoiningClosest(t *testing.T) {
	var b byteRanges
	for k := range int64(maxByteRanges) {
		b.add(k*100, k*100+1)
	}
	// One past the bound: every gap is 99, so the first pair joins.
	b.add(50_000, 50_001)
	// Gap 2 from [25500,25501): this time the closest pair is that one.
	b.add(25_503, 25_504)
	if n := len(b.ranges()); n != maxByteRanges {
		t.Fatalf("%d ranges, want %d", n, maxByteRanges)
	}
	if !b.overlaps(25_502, 25_503) {
		t.Fatal("the closest pair wasn't the one joined")
	}
	if b.overlaps(1_001, 1_100) {
		t.Fatal("a farther gap was filled")
	}
}
