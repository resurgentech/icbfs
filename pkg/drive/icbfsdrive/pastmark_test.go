package icbfsdrive

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestPastMarkedOncePerSecond: a burst of changes in source time sends
// one past-changed mark at once and puts the rest off to the second's
// end, as one; closing the drive sends one still put off.
func TestPastMarkedOncePerSecond(t *testing.T) {
	store := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d := newDrive(t, store, icbfs.Settings{Name: "pastmark", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	marks := func() int {
		store.mu.Lock()
		defer store.mu.Unlock()
		n := store.n["PUT past"]
		delete(store.n, "PUT past")
		return n
	}
	start := time.Now()
	for i := range 10 {
		if _, err := d.Write(at(ctx, 2021), fmt.Sprintf("/f%d.txt", i), strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if n, most := marks(), int(time.Since(start)/pastMarkEvery)+1; n == 0 || n > most {
		t.Fatalf("10 changes in a burst sent %d marks, want 1 to %d", n, most)
	}

	// A change within a second of the last mark is put off, and closing
	// sends it.
	d.past.mu.Lock()
	d.past.last = time.Now()
	d.past.mu.Unlock()
	d.past.changed(ctx)
	if n := marks(); n != 0 {
		t.Fatalf("a change within a second of the last mark sent %d at once, want none", n)
	}
	d.Close()
	if n := marks(); n != 1 {
		t.Fatalf("closing sent %d marks put off, want 1", n)
	}
	d.Close()
	if n := marks(); n != 0 {
		t.Fatalf("closing again sent %d marks, want none", n)
	}
}
