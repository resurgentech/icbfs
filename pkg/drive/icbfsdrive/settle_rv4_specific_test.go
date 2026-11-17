package icbfsdrive

// Round-4 reviewer, probes 2 and 3: named scenarios, each logged in
// mechanical terms.

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func TestRV4Specific(t *testing.T) {
	if os.Getenv("RV4_SPECIFIC") == "" {
		t.Skip("RV4_SPECIFIC unset")
	}
	bucket := newBuckets(t)
	newD := func(t *testing.T, name string, entries uint32) *Drive {
		return newDriveWith(t, bucket(t), icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries}, Config{DeferLateFolders: true})
	}
	w := func(t *testing.T, d *Drive, p string, at time.Time) []drive.LateChange {
		late := &drive.LateChanges{}
		if _, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), at), p, strings.NewReader(p), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return late.Take()
	}
	rm := func(t *testing.T, d *Drive, p string, at time.Time) []drive.LateChange {
		late := &drive.LateChanges{}
		if err := d.Delete(drive.WithSourceTime(drive.WithLateChanges(system(), late), at), drive.ByPath(p), false, drive.Condition{}); err != nil {
			t.Fatalf("rm %s: %v", p, err)
		}
		return late.Take()
	}
	settle := func(t *testing.T, d *Drive, rows []drive.LateChange) string {
		_, err := settleAt(d, system(), rows, (&keeper{}).opts())
		return errShort(err)
	}
	until := func(rows []drive.LateChange) string {
		var s []string
		for _, r := range rows {
			s = append(s, fmt.Sprintf("%s %s at %s until %s", r.Op, r.Name, r.At.Format("2006-01-02"), r.Until.Format("2006-01-02")))
		}
		return strings.Join(s, "; ")
	}

	t.Run("same name, another item, removed on time inside [At, Until)", func(t *testing.T) {
		d := newD(t, "samename", 0)
		w(t, d, "/dir/a.txt", rvMonth(2011, 1, 1))
		w(t, d, "/dir/n.txt", rvMonth(2011, 2, 1)) // item A
		rm(t, d, "/dir/n.txt", rvMonth(2011, 6, 1))
		w(t, d, "/dir/b.txt", rvMonth(2011, 9, 1))
		rows := w(t, d, "/dir/n.txt", rvMonth(2011, 3, 1)) // item B, late, the same name
		t.Logf("SPECIFIC samename rows: %s", until(rows))
		t.Logf("SPECIFIC samename settle: %s", settle(t, d, rows))
		for _, at := range []time.Time{rvMonth(2011, 2, 15), rvMonth(2011, 4, 1), rvMonth(2011, 7, 1), rvMonth(2011, 10, 1)} {
			t.Logf("SPECIFIC samename as of %s: %s", at.Format("2006-01-02"), rvNamesAt(t, d, "/dir", at))
		}
	})

	t.Run("a late move between two folders, settled one at a time", func(t *testing.T) {
		d := newD(t, "move2", 0)
		w(t, d, "/f/x.txt", rvMonth(2011, 1, 1))
		w(t, d, "/f/a.txt", rvMonth(2011, 8, 1))
		w(t, d, "/g/b.txt", rvMonth(2011, 4, 1))
		late := &drive.LateChanges{}
		if _, err := d.Move(drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 3, 1)), drive.ByPath("/f/x.txt"), "/g/x.txt", false); err != nil {
			t.Fatal(err)
		}
		rows := late.Take()
		t.Logf("SPECIFIC move rows: %s", until(rows))
		byF := map[string][]drive.LateChange{}
		for _, r := range rows {
			byF[r.Folder] = append(byF[r.Folder], r)
		}
		gkey := rvKeyOf(t, d, "/g")
		t.Logf("SPECIFIC move settle /g first: %s", settle(t, d, byF[gkey]))
		for _, at := range []time.Time{rvMonth(2011, 5, 1)} {
			t.Logf("SPECIFIC move between the settles, as of %s: /f %s; /g %s", at.Format("2006-01-02"), rvNamesAt(t, d, "/f", at), rvNamesAt(t, d, "/g", at))
		}
		fkey := rvKeyOf(t, d, "/f")
		t.Logf("SPECIFIC move settle /f: %s", settle(t, d, byF[fkey]))
		for _, at := range []time.Time{rvMonth(2011, 2, 1), rvMonth(2011, 5, 1), rvMonth(2011, 9, 1)} {
			t.Logf("SPECIFIC move after both, as of %s: /f %s; /g %s", at.Format("2006-01-02"), rvNamesAt(t, d, "/f", at), rvNamesAt(t, d, "/g", at))
		}
	})

	t.Run("the folder's own record in the states a settle rewrites", func(t *testing.T) {
		d := newD(t, "record", 0)
		w(t, d, "/dir/a.txt", rvMonth(2011, 1, 1))
		w(t, d, "/dir/b.txt", rvMonth(2011, 3, 1))
		mode := uint32(0o700)
		if _, err := d.SetInfo(drive.WithSourceTime(system(), rvMonth(2011, 6, 1)), drive.ByPath("/dir"), drive.ItemInfo{Mode: &mode, App: map[string][]byte{"rv.k": []byte("june")}}); err != nil {
			t.Fatal(err)
		}
		w(t, d, "/dir/c.txt", rvMonth(2011, 9, 1))
		stat := func(at time.Time) string {
			it, err := d.Stat(system(), drive.ByPath("/dir"), drive.At{Time: at})
			if err != nil {
				return errShort(err)
			}
			return fmt.Sprintf("mode %o app %q", it.Mode, it.App["rv.k"])
		}
		before := map[string]string{}
		for _, at := range []time.Time{rvMonth(2011, 2, 1), rvMonth(2011, 4, 1), rvMonth(2011, 7, 1), rvMonth(2011, 10, 1)} {
			before[at.Format("2006-01-02")] = stat(at)
		}
		rows := w(t, d, "/dir/x.txt", rvMonth(2011, 2, 15))
		t.Logf("SPECIFIC record settle: %s", settle(t, d, rows))
		for _, at := range []time.Time{rvMonth(2011, 2, 1), rvMonth(2011, 4, 1), rvMonth(2011, 7, 1), rvMonth(2011, 10, 1)} {
			k := at.Format("2006-01-02")
			t.Logf("SPECIFIC record /dir as of %s: before the settle %s; after %s", k, before[k], stat(at))
		}
	})

	t.Run("a late record change of the folder, then a late entry change", func(t *testing.T) {
		d := newD(t, "laterecord", 0)
		w(t, d, "/dir/a.txt", rvMonth(2011, 1, 1))
		w(t, d, "/dir/b.txt", rvMonth(2011, 9, 1))
		mode := uint32(0o700)
		late := &drive.LateChanges{}
		_, err := d.SetInfo(drive.WithSourceTime(drive.WithLateChanges(system(), late), rvMonth(2011, 5, 1)), drive.ByPath("/dir"), drive.ItemInfo{Mode: &mode})
		t.Logf("SPECIFIC laterecord late SetInfo: %s, rows %d", errShort(err), len(late.Take()))
		it, _ := d.Stat(system(), drive.ByPath("/dir"), drive.At{})
		t.Logf("SPECIFIC laterecord present mode after the late SetInfo: %o", it.Mode)
		rows := w(t, d, "/dir/x.txt", rvMonth(2011, 3, 1))
		t.Logf("SPECIFIC laterecord settle: %s", settle(t, d, rows))
		for _, at := range []time.Time{rvMonth(2011, 4, 1), rvMonth(2011, 6, 1), rvMonth(2011, 10, 1), {}} {
			it, err := d.Stat(system(), drive.ByPath("/dir"), drive.At{Time: at})
			if err != nil {
				t.Logf("SPECIFIC laterecord as of %s: %s", fmtT(at), errShort(err))
				continue
			}
			t.Logf("SPECIFIC laterecord as of %s: mode %o, names %s", fmtT(at), it.Mode, rvNamesAt(t, d, "/dir", at))
		}
	})

	t.Run("a date in the future", func(t *testing.T) {
		d := newD(t, "future", 0)
		w(t, d, "/dir/a.txt", rvMonth(2011, 1, 1))
		late := &drive.LateChanges{}
		_, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(system(), late), time.Now().Add(time.Hour)), "/dir/f.txt", strings.NewReader("f"), drive.Condition{}, drive.WriteOptions{})
		t.Logf("SPECIFIC future write: %s", errShort(err))
		bad := []drive.LateChange{{Folder: rvKeyOf(t, d, "/dir"), At: rvMonth(2011, 1, 1), Until: time.Now().Add(time.Hour), Op: drive.LateAdd, Name: "z", Item: "0000-0000", Type: "file"}}
		t.Logf("SPECIFIC future until: %s", settle(t, d, bad))
	})
}
