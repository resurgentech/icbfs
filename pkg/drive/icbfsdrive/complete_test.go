package icbfsdrive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// allDated fails unless every version of item key's content and .metadata
// (delete markers aside) has a date in its .version, and returns them.
func allDated(t *testing.T, store objstore.Store, key string) objstore.Dates {
	t.Helper()
	ctx := context.Background()
	dates, err := objstore.ReadDates(ctx, store, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key, key + ".metadata"} {
		history, err := store.History(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range history {
			if _, ok := dates[o.VersionID]; !ok && !o.Deleted {
				t.Fatalf("%s version %s, written %v, has no date", k, o.VersionID, o.LastModified)
			}
		}
	}
	return dates
}

// newestMetadata is the newest version of key's .metadata.
func newestMetadata(t *testing.T, store objstore.Store, key string) objstore.Object {
	t.Helper()
	history, err := store.History(context.Background(), key+".metadata")
	if err != nil || len(history) == 0 {
		t.Fatalf("%s.metadata history: %v (%d)", key, err, len(history))
	}
	return history[0]
}

// TestEveryVersionDated: in a filesystem written in a source's time, every
// version of a file has a date, whatever wrote it: the mirror's writes,
// checksums recorded (at the date of the version they amend),
// an ordinary write, a delete and a restore.
func TestEveryVersionDated(t *testing.T) {
	d, store := sourceDrive(t, "complete")
	ctx := system()
	write := func(ctx context.Context, content string) drive.Item {
		t.Helper()
		it, err := d.Write(ctx, "/doc.txt", strings.NewReader(content), drive.Condition{}, drive.WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	it := write(at(ctx, 2021), "v1")
	write(at(ctx, 2022), "v2")
	before := newestMetadata(t, store, it.ID)

	// Bookkeeping, as a read that computes a version's checksums records
	// them.
	if err := d.fs.SetChecksums(ctx, it.ID, "an-etag", icbfs.Checksums{MD5: []byte("0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	after := newestMetadata(t, store, it.ID)
	if after.VersionID == before.VersionID {
		t.Fatal("reading the checksums recorded nothing")
	}
	dates := allDated(t, store, it.ID)
	if !dates[after.VersionID].Equal(dates[before.VersionID]) {
		t.Fatalf("the checksums' .metadata is dated %v, want the version it amends' %v", dates[after.VersionID], dates[before.VersionID])
	}

	write(ctx, "v3") // an ordinary write, now
	if err := d.Delete(ctx, drive.ByID(it.ID), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	trash, err := d.Trash(ctx, drive.TrashOptions{})
	if err != nil || len(trash.Entries) != 1 {
		t.Fatalf("trash: %v (%d)", err, len(trash.Entries))
	}
	if _, err := d.Restore(ctx, trash.Entries[0].ID, drive.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	allDated(t, store, it.ID)
}

// TestFirstDatesListEarlier: a file written before its filesystem was
// first written in a source's time has its earlier versions listed in its
// .version once one is, at the times the store wrote them (to the
// second: a write's own description of itself has no finer).
func TestFirstDatesListEarlier(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "earlier", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	var old drive.Item
	for _, content := range []string{"a", "b"} {
		var err error
		if old, err = d.Write(ctx, "/old.txt", strings.NewReader(content), drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Write(at(ctx, 2021), "/other.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !d.fs.SourceTime() {
		t.Fatal("a change in a source's time didn't mark the filesystem")
	}
	if _, err := d.Write(ctx, "/old.txt", strings.NewReader("c"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	dates := allDated(t, store, old.ID)
	history, err := store.History(context.Background(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range history {
		if !dates[o.VersionID].Truncate(time.Second).Equal(o.LastModified.Truncate(time.Second)) {
			t.Fatalf("version %s is dated %v, written %v", o.VersionID, dates[o.VersionID], o.LastModified)
		}
	}
}
