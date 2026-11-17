package icbfsdrive

import (
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestLatestState: a folder's newest state is its latest entry change or
// its own record's, whichever is later, and a change dated at or after it
// is not late (no history rewritten); a file's is its latest version or
// record write.
func TestLatestState(t *testing.T) {
	d, _ := sourceDrive(t, "latest")
	ctx := system()
	latest := func(p string) time.Time {
		t.Helper()
		got, err := d.LatestState(ctx, drive.ByPath(p))
		if err != nil {
			t.Fatalf("latest state of %s: %v", p, err)
		}
		return got
	}
	if _, err := d.Mkdir(at(ctx, 2016), "/F", false); err != nil {
		t.Fatal(err)
	}
	if got := latest("/F"); !got.Equal(year(2016)) {
		t.Errorf("/F made in 2016: latest state %v", got)
	}
	if _, err := d.Write(at(ctx, 2018), "/F/a.txt", strings.NewReader("a"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := latest("/F"); !got.Equal(year(2018)) {
		t.Errorf("/F after an entry in 2018: latest state %v", got)
	}
	// Its own record, later than any entry: a Stat's Modified does not
	// see it; the latest state does.
	if _, err := d.SetInfo(at(ctx, 2020), drive.ByPath("/F"), drive.ItemInfo{App: map[string][]byte{"clio.locked": []byte("1")}}); err != nil {
		t.Fatal(err)
	}
	st, err := d.Stat(ctx, drive.ByPath("/F"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if got := latest("/F"); !got.Equal(year(2020)) {
		t.Errorf("/F after its record in 2020: latest state %v (Modified %v)", got, st.Modified)
	}
	if _, err := d.Write(at(ctx, 2021), "/F/a.txt", strings.NewReader("a2"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := latest("/F/a.txt"); !got.Equal(year(2021)) {
		t.Errorf("/F/a.txt's second version in 2021: latest state %v", got)
	}
	// A change dated just after the folder's latest state is in order:
	// the present has it, and a view of just before does not.
	next := latest("/F").Add(time.Millisecond)
	if _, err := d.Write(drive.WithSourceTime(ctx, next), "/F/b.txt", strings.NewReader("b"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(t, d, "/F/b.txt", time.Time{}) || exists(t, d, "/F/b.txt", year(2020)) {
		t.Errorf("/F/b.txt written just after the latest state: not where it belongs in the history")
	}
	if got := latest("/F"); !got.Equal(next) {
		t.Errorf("/F's latest state after it: %v, want %v", got, next)
	}
}
