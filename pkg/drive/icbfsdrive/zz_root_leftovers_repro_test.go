package icbfsdrive

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestReproRootLeftovers replays the owner's field case of 2026-11-18 on
// MinIO with versioning: a filesystem "clio" made as icbfs-clio makes it
// (dated at the Clio horizon, 1980), a few dated items written into "/" as
// a replay writes them, every current object removed WITHOUT removing
// versions (what azcopy remove or a plain S3 delete does), a filesystem
// "clio" made again and the same items replayed. It logs what is seen;
// run with ICBFS_REPRO=1.
func TestReproRootLeftovers(t *testing.T) {
	if os.Getenv("ICBFS_REPRO") == "" {
		t.Skip("ICBFS_REPRO=1 runs the reproduction")
	}
	store := miniotest.Store(t)
	ctx := system()
	horizon := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	s := icbfs.Settings{Name: "clio", Size: 1 << 30, RootMode: 0o755, Created: horizon}
	cfg := Config{DeferLateFolders: true}
	items := []struct {
		at   time.Time
		path string
	}{
		{time.Date(2025, 1, 9, 0, 0, 0, 0, time.UTC), "/A/a.txt"},
		{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "/B/b.txt"},
		{time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), "/c.txt"},
	}
	past := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	spread := os.Getenv("ICBFS_REPRO_SPREAD") != ""
	var firstWindow time.Time
	replay := func(d *Drive) []drive.LateChange {
		late := &drive.LateChanges{}
		for i, it := range items {
			if spread && i > 0 {
				time.Sleep(1100 * time.Millisecond) // the field's writes were minutes apart: distinct store seconds
			}
			if i == 1 && firstWindow.IsZero() {
				firstWindow = time.Now()
			}
			wctx := drive.WithLateChanges(drive.WithSourceTime(ctx, it.at), late)
			if _, err := d.Write(wctx, it.path, strings.NewReader(it.path), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
				t.Fatalf("write %s: %v", it.path, err)
			}
		}
		return late.Take()
	}
	listAt := func(d *Drive, when time.Time) string {
		fsys, err := d.fs, error(nil)
		if !when.IsZero() {
			fsys, err = d.at(ctx, drive.At{Time: when})
		}
		if err != nil {
			return "open as of " + when.String() + ": " + err.Error()
		}
		entries, err := fsys.ReadDir(ctx, fsys.RootKey())
		if err != nil {
			return "list: " + err.Error()
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		sort.Strings(out)
		return "[" + strings.Join(out, ",") + "]"
	}

	// 1. The first filesystem.
	d1 := newDriveWith(t, store, s, cfg)
	rows1 := replay(d1)
	t.Logf("first filesystem: id %s, late rows %d; / now %s, as of %s %s",
		d1.Filesystem().RootKey(), len(rows1), listAt(d1, time.Time{}), past.Format(time.DateOnly), listAt(d1, past))
	d1.Close()

	// 2. Every current object removed, versions kept.
	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if err := store.Delete(ctx, o.Key); err != nil {
			t.Fatalf("delete %s: %v", o.Key, err)
		}
	}
	left, _ := store.List(ctx, "")
	t.Logf("removed %d current objects; the store now lists %d", len(all), len(left))
	time.Sleep(1100 * time.Millisecond) // a second apart in the store's time, as the field's minutes were
	created2 := time.Now()

	// 3. The second filesystem, same name.
	info, err := icbfs.CreateFilesystem(context.Background(), store, s)
	t.Logf("second create: %+v, err %v", info, err)
	d2 := newDriveWith(t, store, s, cfg)
	rows2 := replay(d2)
	byFolder := map[string]int{}
	for _, r := range rows2 {
		byFolder[r.Folder]++
	}
	t.Logf("second filesystem: late rows %d, by folder %v (root %s)", len(rows2), byFolder, d2.Filesystem().RootKey())
	for _, r := range rows2 {
		t.Logf("  row: folder %s at %s until %s %s %s", r.Folder, r.At.Format(time.RFC3339), r.Until.Format(time.RFC3339Nano), r.Op, r.Name)
	}
	t.Logf("before the settle: / now %s; as of %s %s; /A/a.txt as of then exists=%v; /A now exists=%v",
		listAt(d2, time.Time{}), past.Format(time.DateOnly), listAt(d2, past), exists(t, d2, "/A/a.txt", past), exists(t, d2, "/A", time.Now()))
	t.Logf("before the settle: / as of %s (inside the first filesystem's run) %s; /A/a.txt then exists=%v",
		firstWindow.Format(time.RFC3339Nano), listAt(d2, firstWindow.Add(500*time.Millisecond)), exists(t, d2, "/A/a.txt", firstWindow.Add(500*time.Millisecond)))

	dump := func(when string) {
		for _, key := range []string{"0000-root-clio", "0000-root-clio.metadata"} {
			h, err := store.History(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			dates, err := objstore.ReadDates(ctx, store, "0000-root-clio")
			t.Logf("%s: %s history (newest stored first), .version read err %v, %d dated:", when, key, err, len(dates))
			for _, o := range h {
				d, ok := dates[o.VersionID]
				pa, _ := objstore.PutAt(&o)
				if full, err := store.HeadVersion(ctx, key, o.VersionID); err == nil {
					pa, _ = objstore.PutAt(full)
				}
				t.Logf("  %s stored %s deleted=%v dated-in-.version=%v %s icbfsat=%s first=%v",
					o.VersionID, o.LastModified.Format(time.RFC3339Nano), o.Deleted, ok, d.Format(time.RFC3339Nano), pa.Format(time.RFC3339), o.LastModified.Before(created2))
			}
		}
	}
	dump("before the settle")
	// 4. The settle of /, three times (a daemon's three in a row).
	var rootRows []drive.LateChange
	for _, r := range rows2 {
		if r.Folder == d2.Filesystem().RootKey() {
			rootRows = append(rootRows, r)
		}
	}
	for i := 1; i <= 3; i++ {
		_, err := d2.SettleFolder(ctx, rootRows, drive.SettleOptions{Path: "/"})
		t.Logf("settle of / #%d: refused=%v: %v", i, errors.Is(err, drive.ErrSettleRefused), err)
	}
	t.Logf("after the settles: / now %s; as of %s %s; /A/a.txt as of then exists=%v; as of the first run %s",
		listAt(d2, time.Time{}), past.Format(time.DateOnly), listAt(d2, past), exists(t, d2, "/A/a.txt", past), listAt(d2, firstWindow.Add(500*time.Millisecond)))

	dump("after the settles")
	// 5. What else of the second filesystem has history from the first.
	hist, err := store.HistoryUnder(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	type keyHist struct{ before, after, markers int }
	byKey := map[string]*keyHist{}
	var keys []string
	for _, o := range hist {
		k := byKey[o.Key]
		if k == nil {
			k = &keyHist{}
			byKey[o.Key] = k
			keys = append(keys, o.Key)
		}
		switch {
		case o.Deleted:
			k.markers++
		case o.LastModified.Before(created2.Add(-time.Second)):
			k.before++
		default:
			k.after++
		}
	}
	for _, key := range keys {
		k := byKey[key]
		if k.before > 0 && k.after > 0 {
			t.Logf("SHARED history: %s: %d version(s) of the first filesystem, %d delete marker(s), %d of the second", key, k.before, k.markers, k.after)
		}
	}
	n := 0
	for _, key := range keys {
		if k := byKey[key]; k.before > 0 && k.after == 0 {
			n++
		}
	}
	t.Logf("%d other keys hold only the first filesystem's versions (not current)", n)

	// 6. What fsck says.
	rep, err := d2.Filesystem().Check(ctx)
	t.Logf("fsck: ok=%v problems=%v unreachable=%d err=%v", rep.OK(), rep.Problems, rep.Unreachable, err)
}

// TestReproEraseLeftoversUnderTheLiveFilesystem: step 4 of the brief,
// shown, not built: on the field case, the versions RootLeftovers names
// (the root's and its .metadata's stored before the creation) erased
// one by one with nothing else writing; then the settle of /, and what /
// lists in the past. Run with ICBFS_REPRO=1.
func TestReproEraseLeftoversUnderTheLiveFilesystem(t *testing.T) {
	if os.Getenv("ICBFS_REPRO") == "" {
		t.Skip("ICBFS_REPRO=1 runs the reproduction")
	}
	store := miniotest.Store(t)
	ctx := system()
	d, rows := madeOverLeftovers(t, store)
	left, err := d.Filesystem().RootLeftovers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range left {
		if err := store.Erase(ctx, o.Key, o.VersionID); err != nil {
			t.Fatalf("erase %s %s: %v", o.Key, o.VersionID, err)
		}
	}
	t.Logf("erased %d versions; / as of %s before the settle: [%s]", len(left), leftoverPast.Format(time.DateOnly), sortedAt(t, d, "/", leftoverPast))
	_, err = d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/"})
	t.Logf("settle of / after the erase: %v", err)
	t.Logf("/ as of %s after: [%s]; /A/a.txt then exists=%v; / now [%s]", leftoverPast.Format(time.DateOnly),
		sortedAt(t, d, "/", leftoverPast), exists(t, d, "/A/a.txt", leftoverPast), sortedAt(t, d, "/", time.Now()))
	for _, when := range []time.Time{time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)} {
		t.Logf("/ as of %s: [%s]", when.Format(time.DateOnly), sortedAt(t, d, "/", when))
	}
	left, _ = d.Filesystem().RootLeftovers(ctx)
	rep, err := d.Filesystem().Check(ctx)
	t.Logf("after: RootLeftovers %d; fsck problems %+v %v", len(left), rep.Problems, err)
	hist, _ := store.HistoryUnder(ctx, "0000-")
	t.Logf("history entries still under 0000- (the first filesystem's other objects, not current): %d", len(hist))
}
