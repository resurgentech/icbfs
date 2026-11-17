package icbfsdrive

// Round-4 reviewer, probe 6: one settle of many queued changes, peak heap
// sampled while it runs, against a daemon's 150 MiB.

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func TestRV4SettleHeap(t *testing.T) {
	n0 := rvEnvInt("RV4_HEAP_N0", 0)
	if n0 == 0 {
		t.Skip("RV4_HEAP_N0 unset")
	}
	nl := rvEnvInt("RV4_HEAP_LATE", 2000)
	st := newRVStore(miniotest.Store(t))
	d := newDriveWith(t, st, icbfs.Settings{Name: "heap", Size: 1 << 32, RootMode: 0o777, Created: year(2010)}, Config{DeferLateFolders: true})
	at := year(2011)
	for i := 0; i < n0; i++ {
		at = at.Add(time.Hour)
		if _, err := d.Write(drive.WithSourceTime(system(), at), fmt.Sprintf("/dir/f%05d.txt", i), strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatal(err)
		}
	}
	var rows []drive.LateChange
	behind := at.Add(-time.Duration(rvEnvInt("RV4_HEAP_BEHIND", 3)) * time.Hour)
	st.take()
	t0 := time.Now()
	for i := 0; i < nl; i++ {
		late := &drive.LateChanges{}
		ctx := drive.WithSourceTime(drive.WithLateChanges(system(), late), behind.Add(time.Duration(i)*time.Second))
		if _, err := d.Write(ctx, fmt.Sprintf("/dir/L%05d.txt", i), strings.NewReader("l"), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, late.Take()...)
	}
	wreqs, _, _, _ := st.take()
	t.Logf("HEAP %d late writes: %d requests, %.1f s", nl, wreqs, time.Since(t0).Seconds())
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapInuse
	var mu sync.Mutex
	peak := base
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			mu.Lock()
			if ms.HeapInuse > peak {
				peak = ms.HeapInuse
			}
			mu.Unlock()
		}
	}()
	t1 := time.Now()
	s, err := d.SettleFolder(system(), rows, (&keeper{}).opts())
	close(stop)
	reqs, byts, listed, ops := st.take()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("HEAP folder of %d entries + %d late rows behind its last %d states: settle %+v err %v; %d requests %v, %.1f MiB read, %d listed, %.1f s; heap before %.1f MiB, peak %.1f MiB (+%.1f)",
		n0, len(rows), rvEnvInt("RV4_HEAP_BEHIND", 3), s, err, reqs, ops, float64(byts)/(1<<20), listed, time.Since(t1).Seconds(),
		float64(base)/(1<<20), float64(peak)/(1<<20), float64(peak-base)/(1<<20))
}
