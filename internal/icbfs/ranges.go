package icbfs

import "sort"

// byteRanges is a set of byte ranges, [lo, hi), sorted and apart: what an
// open file has written since its last flush. A rebase re-applies exactly
// these bytes onto another writer's newer version, so the other writer's
// changes between two of ours survive, as they would on NFS, which writes
// back only the dirty pages. One span from the first written byte to the
// last would carry our stale copy of everything in between.
//
// It holds at most maxByteRanges ranges: past that, the two closest are
// joined, which can only make a rebase carry, and a lock check or a
// multipart flush treat as written, a little more than was.
type byteRanges struct {
	r [][2]int64
}

// maxByteRanges bounds a byteRanges: enough for scattered small edits, and
// a rebase's or flush's walk over them stays trivial.
const maxByteRanges = 256

func (b *byteRanges) empty() bool { return len(b.r) == 0 }

func (b *byteRanges) clear() { b.r = b.r[:0] }

// add adds [lo, hi), joining it with every range it overlaps or touches.
func (b *byteRanges) add(lo, hi int64) {
	if lo >= hi {
		return
	}
	// The first range that ends at or after lo, and the first that starts
	// after hi: everything from i up to j joins the new one.
	i := sort.Search(len(b.r), func(k int) bool { return b.r[k][1] >= lo })
	j := sort.Search(len(b.r), func(k int) bool { return b.r[k][0] > hi })
	if i < j {
		lo = min(lo, b.r[i][0])
		hi = max(hi, b.r[j-1][1])
	}
	b.r = append(b.r[:i], append([][2]int64{{lo, hi}}, b.r[j:]...)...)
	if len(b.r) > maxByteRanges {
		b.joinClosest()
	}
}

// joinClosest joins the two neighbouring ranges with the smallest gap.
func (b *byteRanges) joinClosest() {
	best := 0
	for k := 1; k < len(b.r)-1; k++ {
		if b.r[k+1][0]-b.r[k][1] < b.r[best+1][0]-b.r[best][1] {
			best = k
		}
	}
	b.r[best][1] = b.r[best+1][1]
	b.r = append(b.r[:best+1], b.r[best+2:]...)
}

// span is the smallest single range covering them all.
func (b *byteRanges) span() (lo, hi int64) {
	if len(b.r) == 0 {
		return 0, 0
	}
	return b.r[0][0], b.r[len(b.r)-1][1]
}

// overlaps reports whether any range overlaps [lo, hi).
func (b *byteRanges) overlaps(lo, hi int64) bool {
	i := sort.Search(len(b.r), func(k int) bool { return b.r[k][1] > lo })
	return i < len(b.r) && b.r[i][0] < hi
}

// ranges returns them, in order: the caller mustn't change them.
func (b *byteRanges) ranges() [][2]int64 { return b.r }
