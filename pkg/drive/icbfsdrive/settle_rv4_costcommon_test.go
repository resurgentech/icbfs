package icbfsdrive

// Round-4 reviewer, probe 6: what a write costs in a folder of 2,100 entries
// and about 2,000 states, and a first pass of 2,000 files into one folder,
// on this tree (the same file runs on icbfs main and on the branch; the
// drive's config and context come from zz_rv4_costcfg_test.go).

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func rvHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

func rvAlloc() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.TotalAlloc
}

func TestRV4Cost(t *testing.T) {
	n := rvEnvInt("RV4_COST_N", 0)
	if n == 0 {
		t.Skip("RV4_COST_N unset")
	}
	for _, shape := range strings.Split(os.Getenv("RV4_COST_SHAPES"), ",") {
		t.Run(shape, func(t *testing.T) {
			st := newRVStore(miniotest.Store(t))
			d := newDriveWith(t, st, icbfs.Settings{Name: "cost", Size: 1 << 32, RootMode: 0o777, Created: year(2010)}, rvCostConfig())
			if _, err := d.Mkdir(drive.WithSourceTime(system(), year(2011)), "/dir", false); err != nil {
				t.Fatal(err)
			}
			// The first pass: n files into /dir, oldest first. "hours": an
			// hour apart; "bulk": ten to a second (a Clio import), every write
			// within a second of the folder's newest state.
			at := year(2011)
			step := time.Hour
			if shape == "bulk" {
				step = 100 * time.Millisecond
				at = at.Add(time.Second)
			}
			st.take()
			a0 := rvAlloc()
			start := time.Now()
			var mid time.Duration
			for i := 0; i < n; i++ {
				at = at.Add(step)
				at = at.Truncate(step)
				if shape == "bulk" {
					at = at.Truncate(time.Second) // Clio's whole seconds
					if i%10 == 0 {
						at = at.Add(time.Second)
					}
				}
				ctx, _ := rvCostCtx(at)
				if _, err := d.Write(ctx, fmt.Sprintf("/dir/f%05d.txt", i), strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
					t.Fatalf("write %d: %v", i, err)
				}
				if i == n/2 {
					mid = time.Since(start)
				}
			}
			el := time.Since(start)
			reqs, byts, listed, ops := st.take()
			t.Logf("COST %s first pass %d files: %d requests (%v), %.1f MiB read, %d versions listed, %.1f s (first half %.1f s), %.0f MiB allocated",
				shape, n, reqs, ops, float64(byts)/(1<<20), listed, el.Seconds(), mid.Seconds(), float64(rvAlloc()-a0)/(1<<20))
			key := rvKeyOf(t, d, "/dir")
			dates, _ := objstore.ReadDates(context.Background(), st.Store, key)
			vh, _ := st.Store.History(context.Background(), key+objstore.VersionSuffix)
			rh, _ := st.Store.History(context.Background(), key)
			var vsize int64
			if len(vh) > 0 {
				vsize = vh[0].Size
			}
			t.Logf("COST %s folder: %d dated versions in .version (%d KiB), root block %d versions", shape, len(dates), vsize/1024, len(rh))
			// One more write of each kind.
			newest := at
			for _, w := range []struct {
				name string
				at   time.Time
			}{
				{"on time, a day after", newest.Add(24 * time.Hour)},
				{"on time, newest+1ms (T84-1)", newest.Add(24*time.Hour + time.Millisecond)},
				{"late, a year behind", year(2011).Add(500 * time.Hour)},
			} {
				ctx, take := rvCostCtx(w.at)
				h0 := rvHeap()
				a0 := rvAlloc()
				st.take()
				t0 := time.Now()
				_, err := d.Write(ctx, "/dir/w-"+strings.ReplaceAll(w.name, " ", "_")+".txt", strings.NewReader("w"), drive.Condition{}, drive.WriteOptions{})
				el := time.Since(t0)
				reqs, byts, listed, ops := st.take()
				t.Logf("COST %s one write %-28s: err %v, %d requests %v, %.0f KiB read, %d versions listed, %.2f s, %.1f MiB allocated, heap %+.1f MiB, %s",
					shape, w.name, err, reqs, ops, float64(byts)/1024, listed, el.Seconds(), float64(rvAlloc()-a0)/(1<<20), (float64(rvHeap())-float64(h0))/(1<<20), take())
				if w.name == "on time, a day after" {
					newest = w.at
				}
			}
		})
	}
}
