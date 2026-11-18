package icbfsdrive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// blindToHistory is a store whose prefix listings of history find
// nothing: a create through it does what a build before icbfs's
// ErrLeftovers did, which never looked.
type blindToHistory struct{ objstore.Store }

func (blindToHistory) HistoryUnder(context.Context, string) ([]objstore.Object, error) {
	return nil, nil
}

// leftoverItems are a Clio replay's top-level items: dated, into "/".
var leftoverItems = []struct {
	at   time.Time
	path string
}{
	{time.Date(2025, 1, 9, 0, 0, 0, 0, time.UTC), "/A/a.txt"},
	{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "/B/b.txt"},
	{time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), "/c.txt"},
}

// leftoverPast is a date after the items' own and before today.
var leftoverPast = time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

// replayInto writes leftoverItems as a replay does (dated, late changes
// noted), a store second apart when spread (the field's writes were
// minutes apart), and returns the late changes.
func replayInto(t *testing.T, d *Drive, spread bool) []drive.LateChange {
	t.Helper()
	late := &drive.LateChanges{}
	for i, it := range leftoverItems {
		if spread && i > 0 {
			time.Sleep(1100 * time.Millisecond)
		}
		ctx := drive.WithLateChanges(drive.WithSourceTime(system(), it.at), late)
		if _, err := d.Write(ctx, it.path, strings.NewReader(it.path), drive.Condition{}, drive.WriteOptions{Parents: true}); err != nil {
			t.Fatalf("write %s: %v", it.path, err)
		}
	}
	return late.Take()
}

// clioSettings is the filesystem icbfs-clio creates on demand: dated at
// the Clio horizon.
var clioSettings = icbfs.Settings{Name: "clio", Size: 1 << 30, RootMode: 0o755, Created: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}

// madeOverLeftovers is the owner's field case of 2026-11-18: a Clio
// filesystem replayed, every current object removed outside icbfs, and
// "clio" made again by a build that did not look, and replayed. It
// returns the second drive and its late changes.
func madeOverLeftovers(t *testing.T, store objstore.Store) (*Drive, []drive.LateChange) {
	t.Helper()
	ctx := context.Background()
	d1 := newDriveWith(t, store, clioSettings, Config{DeferLateFolders: true})
	if rows := replayInto(t, d1, true); len(rows) != 0 {
		t.Fatalf("the first replay noted %d late changes; want none", len(rows))
	}
	d1.Close()
	all, err := store.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if err := store.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := icbfs.CreateFilesystem(ctx, store, clioSettings); !errors.Is(err, icbfs.ErrLeftovers) {
		t.Fatalf("this build's create over the leftovers: %v; want ErrLeftovers", err)
	}
	if _, err := icbfs.CreateFilesystem(ctx, blindToHistory{store}, clioSettings); err != nil {
		t.Fatalf("the older build's create: %v", err)
	}
	d2 := newDriveWith(t, store, clioSettings, Config{DeferLateFolders: true})
	return d2, replayInto(t, d2, false)
}

// TestFC6_AFilesystemMadeOverLeftoversIsSaidByTheSettleAndFsck: on the
// field case, every top-level item is late; / lists nothing as of a date
// before today; the settle of / is refused, every time, with a message
// that says the earlier filesystem's versions are the cause and the way
// out; fsck reports them (root-leftovers), and RootLeftovers lists the
// first filesystem's root and .metadata versions.
func TestFC6_AFilesystemMadeOverLeftoversIsSaidByTheSettleAndFsck(t *testing.T) {
	store := miniotest.Store(t)
	ctx := system()
	d, rows := madeOverLeftovers(t, store)
	if len(rows) != len(leftoverItems) {
		t.Fatalf("late changes on the second replay: %d; want every top-level item (%d)", len(rows), len(leftoverItems))
	}
	if got := sortedAt(t, d, "/", leftoverPast); got != "" {
		t.Fatalf("/ as of %s: %q; the leftovers make it empty", leftoverPast.Format(time.DateOnly), got)
	}
	for i := 0; i < 3; i++ {
		_, err := d.SettleFolder(ctx, rows, drive.SettleOptions{Path: "/"})
		if !errors.Is(err, drive.ErrSettleRefused) {
			t.Fatalf("settle %d: %v; want refused", i+1, err)
		}
		for _, want := range []string{"removed outside icbfs", "stored before this filesystem was created", "fs prune"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("settle %d: the message lacks %q: %v", i+1, want, err)
			}
		}
		if i == 0 {
			t.Logf("%v", err)
		}
	}
	left, err := d.Filesystem().RootLeftovers(ctx)
	if err != nil || len(left) != 2*len(leftoverItems)+2 {
		t.Fatalf("RootLeftovers: %d, %v; want the first filesystem's root and .metadata versions (%d)", len(left), err, 2*len(leftoverItems)+2)
	}
	rep, err := d.Filesystem().Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 1 || rep.Problems[0].Kind != "root-leftovers" {
		t.Fatalf("fsck: %+v; want one root-leftovers problem", rep.Problems)
	}
	t.Logf("fsck: %s", rep.Problems[0].Detail)
	// What a host says at its start (icbfs-clio: RootLeftovers).
	if err := pkgicbfs.RootLeftovers(ctx, store, "clio"); !errors.Is(err, pkgicbfs.ErrMadeOverLeftovers) {
		t.Fatalf("pkg RootLeftovers: %v; want ErrMadeOverLeftovers", err)
	}
}

// TestFC7_IcbfsOwnEraseOfThePoisonedFilesystemLetsItBeMadeClean: the
// way out of the field case: archive and prune the filesystem made over
// the leftovers (every version under its prefix goes, the earlier
// filesystem's too); "clio" is then made again, the replay notes no late
// change, and / lists its items as of a date before today.
func TestFC7_IcbfsOwnEraseOfThePoisonedFilesystemLetsItBeMadeClean(t *testing.T) {
	store := miniotest.Store(t)
	ctx := context.Background()
	d, _ := madeOverLeftovers(t, store)
	d.Close()
	if err := icbfs.Archive(ctx, store, "clio"); err != nil {
		t.Fatal(err)
	}
	if err := icbfs.Prune(ctx, store, "clio"); err != nil {
		t.Fatal(err)
	}
	if h, err := store.HistoryUnder(ctx, "0000-"); err != nil || len(h) != 0 {
		t.Fatalf("history under 0000- after the prune: %d, %v; want none", len(h), err)
	}
	d3 := newDriveWith(t, store, clioSettings, Config{DeferLateFolders: true})
	if rows := replayInto(t, d3, false); len(rows) != 0 {
		t.Fatalf("the replay after the prune noted %d late changes; want none", len(rows))
	}
	if got := sortedAt(t, d3, "/", leftoverPast); got != "A,B,c.txt" {
		t.Fatalf("/ as of %s: %q; want A,B,c.txt", leftoverPast.Format(time.DateOnly), got)
	}
	if left, err := d3.Filesystem().RootLeftovers(ctx); err != nil || len(left) != 0 {
		t.Fatalf("RootLeftovers after: %d, %v; want none", len(left), err)
	}
	if err := pkgicbfs.RootLeftovers(ctx, store, "clio"); err != nil {
		t.Fatalf("pkg RootLeftovers after: %v; want nil", err)
	}
}
