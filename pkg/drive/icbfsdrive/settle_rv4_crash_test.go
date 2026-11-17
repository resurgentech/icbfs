package icbfsdrive

// Round-4 reviewer, probe 4: a dated write killed part way (every request
// from the k-th on fails, its undo's too: a process that died), then the
// daemon goes on after a restart: an on-time write into the folder, a late
// write, a settle. What the folder's views and the settle do.

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func TestRV4CrashLeftover(t *testing.T) {
	if os.Getenv("RV4_CRASH") == "" {
		t.Skip("RV4_CRASH unset")
	}
	bucket := newBuckets(t)
	for _, entries := range []uint32{0, 4} {
		// How many requests the write makes.
		st := newRVStore(bucket(t))
		d := newDriveWith(t, st, icbfs.Settings{Name: fmt.Sprintf("crash%d", entries), Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries}, Config{DeferLateFolders: true})
		base := func(d *Drive) {
			for i := 0; i < 8; i++ {
				if _, err := d.Write(drive.WithSourceTime(system(), rvMonth(2011, 1+i, 1)), fmt.Sprintf("/dir/f%d.txt", i), strings.NewReader("f"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
					t.Fatal(err)
				}
			}
		}
		base(d)
		st.take()
		x1 := func(d *Drive) error {
			_, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), &drive.LateChanges{}), rvMonth(2011, 10, 1)), "/dir/x1.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{})
			return err
		}
		if err := x1(d); err != nil {
			t.Fatal(err)
		}
		n, _, _, _ := st.take()
		counts := map[string]int{}
		for k := 1; k <= n; k++ {
			st := newRVStore(bucket(t))
			name := fmt.Sprintf("cr%d-%d", entries, k)
			d := newDriveWith(t, st, icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries}, Config{DeferLateFolders: true})
			base(d)
			st.setFail(k, "lost")
			err1 := x1(d)
			st.setFail(0, "")
			// The restart: a new drive; the item tried again, then an on-time
			// write and a late one.
			d = newDriveWith(t, st, icbfs.Settings{Name: name}, Config{DeferLateFolders: true})
			_ = x1(d)
			if _, err := d.Write(drive.WithSourceTime(system(), rvMonth(2011, 11, 1)), "/dir/x2.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			late := &drive.LateChanges{}
			if _, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 2, 15)), "/dir/x3.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			rows := late.Take()
			_, serr := settleAt(d, system(), rows, (&keeper{}).opts())
			live := rvNamesAt(t, d, "/dir", ztime)
			now := rvNamesAt(t, d, "/dir", nowish())
			key := fmt.Sprintf("settle %s, view as of now = present %v", rvSettleClass(serr), live == now)
			counts[key]++
			t.Logf("CRASH entries=%d k=%d/%d: x1 %s; rows %d (until %s); %s; present %q, as of now %q", entries, k, n, errShort(err1), len(rows), rowsUntil(rows), key, live, now)
		}
		t.Logf("CRASH entries=%d summary over %d cut points: %v", entries, n, counts)
	}
}

var ztime = func() (z time.Time) { return }()

func nowish() time.Time { return time.Now() }
