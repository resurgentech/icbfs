package icbfsdrive

// Round-4 reviewer, probes 1 and 3: an undated write (an operator's Undo; any
// writer without a source time) into a folder written in source time, then
// a late write into it. The undated write's block versions are dated by the
// store's listing (milliseconds on MinIO), its .metadata by the put's answer
// (whole seconds); the late write is dated at the folder's newest state.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func TestRV4UndatedThenLate(t *testing.T) {
	if os.Getenv("RV4_UNDATED") == "" {
		t.Skip("RV4_UNDATED unset")
	}
	bucket := newBuckets(t)
	for ci, tc := range []struct {
		name    string
		entries uint32
		undated string // the undated write's name
		late    string // the late write's name
	}{
		{"one block", 0, "/dir/f3u.txt", "/dir/a-late.txt"},
		{"blocks of 4, other leaf", 4, "/dir/f6u.txt", "/dir/a-late.txt"},
		{"blocks of 4, same leaf", 4, "/dir/f6u.txt", "/dir/f6v.txt"},
		{"blocks of 4, late splits", 4, "/dir/f6u.txt", "/dir/f0a.txt"},
		{"room: blocks of 4, other leaf", 4, "/dir/f6u.txt", "/dir/a-late.txt"},
		{"room: blocks of 4, same leaf", 4, "/dir/f6u.txt", "/dir/f6v.txt"},
		{"room: blocks of 4, late splits", 4, "/dir/f6u.txt", "/dir/f0a.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newRVStore(bucket(t))
			d := newDriveWith(t, st, icbfs.Settings{Name: fmt.Sprintf("und%d", ci), Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: tc.entries},
				Config{DeferLateFolders: true})
			for i := 0; i < 8; i++ {
				if _, err := d.Write(drive.WithSourceTime(system(), rvMonth(2011, 1+i, 1)), fmt.Sprintf("/dir/f%d.txt", i), strings.NewReader("f"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(tc.name, "room") {
				// Room in the leaves: two on-time removals (no leaf merges), so the
				// undated write changes a leaf and not the root.
				for i, p := range []string{"/dir/f5.txt", "/dir/f7.txt", "/dir/f1.txt", "/dir/f3.txt"} {
					if err := d.Delete(drive.WithSourceTime(system(), rvMonth(2011, 9+i, 1)), drive.ByPath(p), false, drive.Condition{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			// The undated write.
			if _, err := d.Write(drive.WithLateChanges(system(), &drive.LateChanges{}), tc.undated, strings.NewReader("u"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			key := rvKeyOf(t, d, "/dir")
			ctx := context.Background()
			dates, _ := objstore.ReadDates(ctx, st.Store, key)
			var newest time.Time
			for _, x := range dates {
				if x.After(newest) {
					newest = x
				}
			}
			// The late write.
			late := &drive.LateChanges{}
			if _, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 3, 15)), tc.late, strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			rows := late.Take()
			// Every block's versions: key, date by its .version or listing, stored order.
			all, _ := st.Store.HistoryUnder(ctx, key[:strings.LastIndex(key, "-")])
			_ = all
			var lines []string
			hist, _ := st.Store.HistoryUnder(ctx, "")
			byKey := map[string][]objstore.Object{}
			for _, o := range hist {
				if strings.HasSuffix(o.Key, ".version") || strings.HasSuffix(o.Key, ".metadata") && o.Key != key+".metadata" {
					continue
				}
				byKey[o.Key] = append(byKey[o.Key], o)
			}
			lw := ""
			for _, r := range rows {
				lw = r.Until.Format(time.RFC3339Nano)
			}
			t.Logf("UNDATED %s: .version newest after the undated write %s; the late write's Until %s", tc.name, newest.Format(time.RFC3339Nano), lw)
			// Views: just before, at and after the undated write's dates, and now.
			for _, at := range []time.Time{newest.Add(-time.Second), newest, newest.Add(500 * time.Millisecond), newest.Add(time.Second), newest.Add(2 * time.Second), time.Now()} {
				got := rvNamesAt(t, d, "/dir", at)
				names := strings.Split(got, ",")
				twice := ""
				for i := 1; i < len(names); i++ {
					if names[i] == names[i-1] {
						twice += " TWICE:" + names[i]
					}
				}
				hasU := strings.Contains(got, strings.TrimPrefix(tc.undated, "/dir/"))
				hasL := strings.Contains(got, strings.TrimPrefix(tc.late, "/dir/"))
				t.Logf("UNDATED %s: before the settle, as of %s: %d names, undated file %v, late file %v%s", tc.name, at.Format(time.RFC3339Nano), len(names), hasU, hasL, twice)
			}
			_, err := settleAt(d, system(), rows, (&keeper{}).opts())
			t.Logf("UNDATED %s: settle: %v", tc.name, errShort(err))
			for _, at := range []time.Time{newest, newest.Add(time.Second), time.Now()} {
				got := rvNamesAt(t, d, "/dir", at)
				t.Logf("UNDATED %s: after the settle, as of %s: %s", tc.name, at.Format(time.RFC3339Nano), got)
			}
			_ = lines
			_ = sort.Strings
		})
	}
}
