package icbfsdrive

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// clioApp is app data as the Clio Mirror keeps it on an item: its id at
// Clio, its version, a deletion mark, and a few more, so that a record
// holds a map of several keys (encoded in an order of the encoder's own).
func clioApp(id string, version int) map[string][]byte {
	return map[string][]byte{
		"clio.id": []byte(id), "clio.version": []byte(fmt.Sprint(version)), "clio.deletedAt": []byte(""),
		"clio.etag": []byte(fmt.Sprintf("e%d", version)), "clio.parent": []byte("p-" + id), "clio.kind": []byte("folder"),
	}
}

// appString is an item's app data, sorted, for comparing.
func appString(app map[string][]byte) string {
	var out []string
	for k, v := range app {
		out = append(out, k+"="+string(v))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// TestCH5_SettleKeepsARecordOfManyAppKeys (CH-5, rule 1.6-3; found by
// the Clio lane on a real Clio Mirror: every folder refused, "its record
// is not the one read"): a folder whose own record carries several app
// keys, changed on time at each of its states (2016, 2018, 2020, 2022),
// and files whose records carry several too; late files written into it
// and settled, one at a time and two at once, at both block sizes. Every
// settle goes through (not refused) and hands out its record before its
// first write; each state keeps the folder's own record it had; the late
// files are in the past from their dates; their own records read back.
func TestCH5_SettleKeepsARecordOfManyAppKeys(t *testing.T) {
	for _, entries := range []uint32{0, 4} {
		t.Run(fmt.Sprintf("blocks of %d", entries), func(t *testing.T) {
			store := &failStore{Store: miniotest.Store(t)}
			d := newDriveWith(t, store, icbfs.Settings{Name: fmt.Sprintf("ch5app%d", entries), Size: 1 << 30, RootMode: 0o777, Created: year(2010), DirBlockEntries: entries},
				Config{DeferLateFolders: true})
			ctx := system()
			years := []int{2016, 2018, 2020, 2022}
			for i, y := range years {
				if _, err := d.Write(at(ctx, y), fmt.Sprintf("/dir/f%d.txt", i), strings.NewReader("f"), drive.Condition{},
					drive.WriteOptions{Parents: true, App: clioApp(fmt.Sprintf("doc-%d", i), y)}); err != nil {
					t.Fatal(err)
				}
				if _, err := d.SetInfo(at(ctx, y), drive.ByPath("/dir"), drive.ItemInfo{App: clioApp("folder-1", y)}); err != nil {
					t.Fatal(err)
				}
			}
			folderApp := func(when time.Time) string {
				t.Helper()
				it, err := d.Stat(ctx, drive.ByPath("/dir"), drive.At{Time: when})
				if err != nil {
					t.Fatal(err)
				}
				return appString(it.App)
			}
			want := map[int]string{}
			for _, y := range years {
				want[y] = appString(clioApp("folder-1", y))
			}
			settle := func(label string, rows []drive.LateChange) {
				t.Helper()
				wrote, recorded, early := false, false, false
				store.setWatch(func(op, key string) error {
					if isWrite(op) {
						wrote = true
						early = early || !recorded
					}
					return nil
				})
				_, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/dir", Record: func(drive.SettleRecord) error {
					recorded = true
					return nil
				}})
				store.setWatch(nil)
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				if !wrote || early {
					t.Errorf("%s: wrote %v, a write before the record %v (1.6-3)", label, wrote, early)
				}
				for y, w := range want {
					if got := folderApp(midYear(y)); got != w {
						t.Errorf("%s: the folder's record as of mid-%d: %s, want %s (CH-5)", label, y, got, w)
					}
				}
			}
			late := func(name string, when time.Time, version int) []drive.LateChange {
				t.Helper()
				lc := &drive.LateChanges{}
				if _, err := d.Write(drive.WithSourceTime(drive.WithLateChanges(ctx, lc), when), "/dir/"+name, strings.NewReader(name), drive.Condition{},
					drive.WriteOptions{App: clioApp(name, version)}); err != nil {
					t.Fatal(err)
				}
				return lc.Take()
			}
			settle("x1 alone", late("x1.txt", rvMonth(2017, 3, 1), 1))
			two := append(late("x2.txt", rvMonth(2019, 3, 1), 2), late("x3.txt", rvMonth(2021, 3, 1), 3)...)
			settle("x2 and x3 together", two)
			for _, c := range []struct {
				name string
				from int
				v    int
			}{{"x1.txt", 2017, 1}, {"x2.txt", 2019, 2}, {"x3.txt", 2021, 3}} {
				for _, y := range []int{c.from - 1, c.from} {
					in := strings.Contains(sortedAt(t, d, "/dir", midYear(y)), c.name)
					if in != (y == c.from) {
						t.Errorf("%s as of mid-%d: listed %v", c.name, y, in)
					}
				}
				it, err := d.Stat(ctx, drive.ByPath("/dir/"+c.name), drive.At{Time: midYear(c.from)})
				if err != nil {
					t.Fatal(err)
				}
				if got, w := appString(it.App), appString(clioApp(c.name, c.v)); got != w {
					t.Errorf("%s's own record as of mid-%d: %s, want %s", c.name, c.from, got, w)
				}
			}
		})
	}
}
