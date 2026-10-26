package icbfsdrive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestReadOnlyDrive: a Config.ReadOnly drive on a filesystem another
// drive writes reads everything and refuses every change with
// ErrReadOnly, system profile or not. DefaultSettings makes the
// filesystem; AsSystem is the writer's context.
func TestReadOnlyDrive(t *testing.T) {
	store := miniotest.Store(t)
	s := icbfs.DefaultSettings("ro")
	if s.Size != icbfs.DefaultSize || s.RootMode != icbfs.DefaultRootMode || s.PrimaryWindows {
		t.Fatalf("DefaultSettings: %+v", s)
	}
	writer := newDrive(t, store, s, "")
	ctx := drive.AsSystem(context.Background(), "mirror")
	if p, ok := drive.ProfileFrom(ctx); !ok || !p.System || drive.Actor(ctx) != "mirror" {
		t.Fatalf("AsSystem: %+v, %v, %q", p, ok, drive.Actor(ctx))
	}
	doc := write(t, ctx, writer, "/a/doc.txt", "hello", drive.Condition{})
	if err := writer.SetAppObject(ctx, "tool.table", []byte("{}")); err != nil {
		t.Fatal(err)
	}

	ro, err := New(context.Background(), Config{Store: store, Filesystem: "ro", ID: "ro", CacheDir: t.TempDir(), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	// Reads work.
	if it, err := ro.Stat(ctx, drive.ByPath("/a/doc.txt"), drive.At{}); err != nil || it.ID != doc.ID {
		t.Fatalf("stat: %+v, %v", it, err)
	}
	rc, _, err := ro.Read(ctx, drive.ByPath("/a/doc.txt"), drive.At{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if page, err := ro.List(ctx, drive.ByPath("/"), drive.ListOptions{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("list: %+v, %v", page.Items, err)
	}
	if got, err := ro.AppObject(ctx, "tool.table"); err != nil || string(got) != "{}" {
		t.Fatalf("app object: %q, %v", got, err)
	}

	// Every change is refused, as the system too.
	changes := map[string]func() error{
		"write": func() error {
			_, err := ro.Write(ctx, "/b.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{})
			return err
		},
		"mkdir":  func() error { _, err := ro.Mkdir(ctx, "/c", false); return err },
		"move":   func() error { _, err := ro.Move(ctx, drive.ByPath("/a/doc.txt"), "/moved.txt", false); return err },
		"delete": func() error { return ro.Delete(ctx, drive.ByPath("/a/doc.txt"), false, drive.Condition{}) },
		"set info": func() error {
			_, err := ro.SetInfo(ctx, drive.ByPath("/a/doc.txt"), drive.ItemInfo{Modified: time.Now()})
			return err
		},
		"set origin": func() error {
			return ro.SetOrigin(ctx, drive.ByPath("/a/doc.txt"), drive.Origin{Drive: "x", ID: "1"})
		},
		"create name":       func() error { _, err := ro.CreateName(ctx, "v1", time.Time{}, false); return err },
		"delete name":       func() error { return ro.DeleteName(ctx, "v1") },
		"set app object":    func() error { return ro.SetAppObject(ctx, "tool.table", []byte("no")) },
		"delete app object": func() error { return ro.DeleteAppObject(ctx, "tool.table") },
		"purge":             func() error { return ro.Purge(ctx, drive.ByID(doc.ID), false) },
	}
	for what, change := range changes {
		if err := change(); !errors.Is(err, drive.ErrReadOnly) {
			t.Errorf("%s on a read-only drive: %v; want ErrReadOnly", what, err)
		}
	}
	// Nothing changed.
	if page, err := writer.List(ctx, drive.ByPath("/"), drive.ListOptions{Depth: 2}); err != nil || len(page.Items) != 2 {
		t.Fatalf("after: %+v, %v", page.Items, err)
	}
	if got, err := writer.AppObject(ctx, "tool.table"); err != nil || string(got) != "{}" {
		t.Fatalf("app object after: %q, %v", got, err)
	}
}
