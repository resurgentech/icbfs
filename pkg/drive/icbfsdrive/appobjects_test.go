package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestAppObjects: a tool's objects beside the drive round-trip, are
// listed by tool, show nowhere in the tree, survive an item's purge and
// the trash's, are read as of a past time, and are known to fsck.
func TestAppObjects(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "appobj", Size: 1 << 30, RootMode: 0o755}, "") // the root: only its owner writes
	ctx := system()
	if !d.Capabilities().AppObjects {
		t.Fatal("the drive doesn't say it keeps app objects")
	}
	if err := d.SetAppObject(ctx, "clio.names", []byte(`{"1":"first"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	between, _ := d.fs.Store().ServerTime(context.Background())
	time.Sleep(1100 * time.Millisecond)
	if err := d.SetAppObject(ctx, "clio.names", []byte(`{"1":"first","2":"second"}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAppObject(ctx, "clio.users", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAppObject(ctx, "other.thing", []byte(`x`)); err != nil {
		t.Fatal(err)
	}
	got, err := d.AppObject(ctx, "clio.names")
	if err != nil || !bytes.Equal(got, []byte(`{"1":"first","2":"second"}`)) {
		t.Fatalf("get: %q, %v", got, err)
	}
	if names, err := d.ListAppObjects(ctx, "clio."); err != nil || strings.Join(names, ",") != "clio.names,clio.users" {
		t.Fatalf("list clio: %v, %v", names, err)
	}
	if names, err := d.ListAppObjects(ctx, ""); err != nil || len(names) != 3 {
		t.Fatalf("list all: %v, %v", names, err)
	}
	for _, bad := range []string{"nodot", "drive.x", "Clio.x"} {
		if err := d.SetAppObject(ctx, bad, []byte("v")); !errors.Is(err, drive.ErrInvalid) {
			t.Fatalf("%q: %v; want ErrInvalid", bad, err)
		}
	}
	if err := d.SetAppObject(ctx, "clio.big", make([]byte, icbfs.MaxAppObjectSize+1)); !errors.Is(err, drive.ErrTooLarge) {
		t.Fatalf("too big: %v; want ErrTooLarge", err)
	}
	// Nothing in the tree.
	write(t, ctx, d, "/doc.txt", "d", drive.Condition{})
	if page, err := d.List(ctx, drive.ByPath("/"), drive.ListOptions{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("the tree: %+v, %v", page.Items, err)
	}
	// As of between the two puts: the first.
	v, err := d.at(ctx, drive.At{Time: between})
	if err != nil {
		t.Fatal(err)
	}
	if old, _, err := v.GetAppObject(ctx, "clio.names"); err != nil || string(old) != `{"1":"first"}` {
		t.Fatalf("as of between: %q, %v", old, err)
	}
	// A purge of an item, and of the trash, leave them.
	if err := d.Delete(ctx, drive.ByPath("/doc.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	page, err := d.Trash(ctx, drive.TrashOptions{})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("trash: %+v, %v", page, err)
	}
	if err := d.Purge(ctx, drive.ByID(page.Entries[0].ItemID), false); err != nil {
		t.Fatal(err)
	}
	if names, err := d.ListAppObjects(ctx, ""); err != nil || len(names) != 3 {
		t.Fatalf("after a purge: %v, %v", names, err)
	}
	report, err := d.fs.Check(context.Background())
	if err != nil || report.Unreachable != 0 || len(report.Problems) != 0 {
		t.Fatalf("fsck: unreachable %d, problems %+v, %v", report.Unreachable, report.Problems, err)
	}
	// Gone when deleted; a second delete is not found.
	if err := d.DeleteAppObject(ctx, "other.thing"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AppObject(ctx, "other.thing"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := d.DeleteAppObject(ctx, "other.thing"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}
	// A reader without write access can read, not write.
	reader := as(1001, 1001)
	if _, err := d.AppObject(reader, "clio.names"); err != nil {
		t.Fatalf("a reader: %v", err)
	}
	if err := d.SetAppObject(reader, "clio.names", []byte("no")); err == nil {
		t.Fatal("a reader wrote an app object")
	}
}
