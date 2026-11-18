package icbfsdrive

// interloc docs/STATES.md IT-28: a dated write killed after its puts and
// before its dating. DateLeftovers, at the next start, lists what it left
// at the write's own date, so every view from that date shows what the
// present shows. A kill at every request of a move, a removal, an add and
// a new version, on time, in a folder of one block and in blocks of 4
// (ICBFS_PRE_MERGE=1, about two minutes); in ci, one block and the kill
// points the views were wrong at before DateLeftovers (IT28_NO_DATING=1
// shows them wrong).

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// it28Names are dir's names as of when (zero: now).
func it28Names(t *testing.T, d *Drive, dir string, when time.Time) []string {
	t.Helper()
	l, err := d.List(system(), drive.ByPath(dir), drive.ListOptions{At: drive.At{Time: when}})
	if err != nil {
		t.Fatalf("list %s as of %v: %v", dir, when, err)
	}
	var out []string
	for _, it := range l.Items {
		out = append(out, it.Name)
	}
	slices.Sort(out)
	return out
}

// it28Version is p's content version as of when ("" if not there).
func it28Version(d *Drive, p string, when time.Time) string {
	it, err := d.Stat(system(), drive.ByPath(p), drive.At{Time: when})
	if err != nil || it.Version == nil {
		return ""
	}
	return it.Version.ID
}

func TestIT28_DatedWriteKilledIsDatedAtTheNextStart(t *testing.T) {
	bucket := newBuckets(t)
	when := rvMonth(2011, 6, 1)
	type op struct {
		name string
		do   func(d *Drive) error
	}
	ops := []op{
		{"move", func(d *Drive) error {
			_, err := d.Move(drive.WithSourceTime(system(), when), drive.ByPath("/A/x.txt"), "/B/x.txt", false)
			return err
		}},
		{"removal", func(d *Drive) error {
			return d.Delete(drive.WithSourceTime(system(), when), drive.ByPath("/A/x.txt"), false, drive.Condition{})
		}},
		{"add", func(d *Drive) error {
			_, err := d.Write(drive.WithSourceTime(system(), when), "/A/n.txt", strings.NewReader("n"), drive.Condition{}, drive.WriteOptions{})
			return err
		}},
		{"version", func(d *Drive) error {
			_, err := d.Write(drive.WithSourceTime(system(), when), "/A/x.txt", strings.NewReader("x2"), drive.Condition{}, drive.WriteOptions{})
			return err
		}},
	}
	skip := os.Getenv("IT28_NO_DATING") != "" // the red: no recovery
	full := os.Getenv("ICBFS_PRE_MERGE") != ""
	// The kill points (requests, from 1) inside each change's dating, in
	// a folder of one block: a move after its new folder's block (17) and
	// after both (25), a removal after its folder's block (20), an add
	// (11) and a new version (10) before their dates.
	ciKs := map[string][]int{"move": {17, 25}, "removal": {20}, "add": {11}, "version": {10}}
	sizes := []uint32{0}
	if full {
		sizes = []uint32{0, 4}
	}
	for _, entries := range sizes {
		for _, o := range ops {
			t.Run(fmt.Sprintf("%s/blocks%d", o.name, entries), func(t *testing.T) {
				base := func(d *Drive) {
					for i, p := range []string{"/A/x.txt", "/A/a0.txt", "/A/a1.txt", "/A/a2.txt", "/A/a3.txt", "/B/b0.txt"} {
						if _, err := d.Write(drive.WithSourceTime(system(), rvMonth(2011, 1, 1+i)), p, strings.NewReader("v"), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
							t.Fatal(err)
						}
					}
				}
				settings := func(name string) icbfs.Settings {
					return icbfs.Settings{Name: name, Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries}
				}
				// The uncut write's requests.
				st := newRVStore(bucket(t))
				d := newDriveWith(t, st, settings(fmt.Sprintf("it28%s%dn", o.name, entries)), Config{DeferLateFolders: true})
				base(d)
				st.take()
				if err := o.do(d); err != nil {
					t.Fatal(err)
				}
				n, _, _, _ := st.take()
				bad := 0
				ks := ciKs[o.name]
				if full {
					ks = nil
					for k := 1; k <= n; k++ {
						ks = append(ks, k)
					}
				}
				for _, k := range ks {
					st := newRVStore(bucket(t))
					name := fmt.Sprintf("it28%s%d-%d", o.name, entries, k)
					d := newDriveWith(t, st, settings(name), Config{DeferLateFolders: true})
					base(d)
					before := map[string][]string{"/A": it28Names(t, d, "/A", time.Time{}), "/B": it28Names(t, d, "/B", time.Time{})}
					vBefore := it28Version(d, "/A/x.txt", time.Time{})
					st.setFail(k, "lost")
					_ = o.do(d)
					st.setFail(0, "")
					// The next start: a new drive; its recovery dates what the
					// killed write left, for the folders and the item.
					d = newDriveWith(t, st, settings(name), Config{DeferLateFolders: true})
					a, _ := d.Stat(system(), drive.ByPath("/A"), drive.At{})
					b, _ := d.Stat(system(), drive.ByPath("/B"), drive.At{})
					items := []drive.Item{a, b}
					for _, p := range []string{"/A/x.txt", "/B/x.txt", "/A/n.txt"} {
						if it, err := d.Stat(system(), drive.ByPath(p), drive.At{}); err == nil {
							items = append(items, it)
						}
					}
					if !skip {
						got, err := d.DateLeftovers(system(), items)
						if err != nil {
							t.Fatalf("k=%d: DateLeftovers: %v", k, err)
						}
						if got.Undatable != 0 {
							t.Errorf("k=%d: %d versions could not be dated", k, got.Undatable)
						}
						again, err := d.DateLeftovers(system(), items)
						if err != nil || again.Dated != 0 {
							t.Errorf("k=%d: run again: %+v %v; want nothing", k, again, err)
						}
					}
					for _, dir := range []string{"/A", "/B"} {
						now := it28Names(t, d, dir, time.Time{})
						if got := it28Names(t, d, dir, when.Add(500*time.Millisecond)); !slices.Equal(got, now) {
							bad++
							t.Logf("k=%d: %s as of the write's date + 0.5 s %v; the present %v", k, dir, got, now)
						}
						if got := it28Names(t, d, dir, when.Add(-500*time.Millisecond)); !slices.Equal(got, before[dir]) {
							bad++
							t.Logf("k=%d: %s as of the write's date - 0.5 s %v; before the write %v", k, dir, got, before[dir])
						}
					}
					if now := it28Version(d, "/A/x.txt", time.Time{}); now != "" {
						if got := it28Version(d, "/A/x.txt", when.Add(500*time.Millisecond)); got != now {
							bad++
							t.Logf("k=%d: /A/x.txt's version as of the write's date + 0.5 s %q; the present %q", k, got, now)
						}
						if got := it28Version(d, "/A/x.txt", when.Add(-500*time.Millisecond)); got != vBefore {
							bad++
							t.Logf("k=%d: /A/x.txt's version as of the write's date - 0.5 s %q; before %q", k, got, vBefore)
						}
					}
				}
				if bad > 0 {
					t.Errorf("%s, blocks of %d, %d of its %d kill points: %d views differ from the present's or the past's", o.name, entries, len(ks), n, bad)
				}
			})
		}
	}
}
