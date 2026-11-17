package icbfs

import (
	"fmt"
	"testing"
)

// A record's sum is one sum, whatever order its maps are encoded in: a
// folder of the Clio Mirror carries several app keys, and the settle's
// proof compared a sum taken at the read with one taken again
// (interloc T83-2: "the state at …: its record is not the one read").
func TestRecordSumIsDeterministic(t *testing.T) {
	a := Attr{Mode: 0o755, App: map[string][]byte{}, Xattrs: map[string][]byte{}}
	for i := range 12 {
		a.App[fmt.Sprintf("clio.k%02d", i)] = []byte(fmt.Sprint(i))
		a.Xattrs[fmt.Sprintf("user.x%02d", i)] = []byte(fmt.Sprint(i))
	}
	first, err := recordSum(a)
	if err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if s, _ := recordSum(a); s != first {
			t.Fatalf("two sums of one record: %s and %s", first, s)
		}
	}
}
