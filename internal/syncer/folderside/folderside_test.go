package folderside_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/syncer"
	"github.com/resurgentech/icbfs/internal/syncer/folderside"
	"github.com/resurgentech/icbfs/internal/syncer/memside"
)

var t0 = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

func put(t *testing.T, root, p, data string, mod time.Time) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestFolderSide(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	put(t, root, "a/b.txt", "bee", t0)
	put(t, root, "c.txt", "see", t0)
	put(t, root, "a/"+folderside.TempPrefix+"x", "half", t0)
	s, err := folderside.Open(root, folderside.Options{})
	if err != nil {
		t.Fatal(err)
	}
	es, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]syncer.Entry{}
	for _, e := range es {
		got[e.Path] = e
	}
	if len(got) != 3 || !got["a"].Dir || got["a/b.txt"].Size != 3 || !got["c.txt"].ModTime.Equal(t0) {
		t.Fatalf("listing %+v", got)
	}
	if s.Caps().IDs && got["a/b.txt"].ID == "" {
		t.Fatal("no id")
	}

	rc, err := s.Read(ctx, "a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "bee" {
		t.Fatalf("read %q", data)
	}

	e, err := s.Write(ctx, "new/deep/d.txt", strings.NewReader("dee"), 3, t0.Add(time.Hour), time.Time{})
	if err != nil || e.Size != 3 || !e.ModTime.Equal(t0.Add(time.Hour)) {
		t.Fatalf("write %+v, %v", e, err)
	}
	moved, err := s.Move(ctx, "a/b.txt", "elsewhere/b2.txt")
	if err != nil {
		t.Fatal(err)
	}
	if s.Caps().IDs && moved.ID != got["a/b.txt"].ID {
		t.Fatalf("the id changed on a rename: %s vs %s", moved.ID, got["a/b.txt"].ID)
	}
	if err := s.Remove(ctx, "c.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "new"); err == nil {
		t.Fatal("removed a directory that isn't empty")
	}
}

// TestFolderToDrive: a real folder through the engine, its renames told
// by ids (no hashing needed).
func TestFolderToDrive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	put(t, root, "Reports/brief.pdf", "the brief", t0)
	put(t, root, "Reports/exhibit.pdf", "exhibit A", t0)
	local, err := folderside.Open(root, folderside.Options{})
	if err != nil {
		t.Fatal(err)
	}
	remote := memside.NewDrive()
	e := syncer.New(syncer.Config{Name: "t", Local: local, Remote: remote.Drive(), State: &syncer.MemState{}, CaseInsensitive: true, SpoolDir: t.TempDir()})
	pass := func() syncer.Report {
		t.Helper()
		rep := e.Pass(ctx)
		if len(rep.Errors) > 0 {
			t.Fatal(rep.Errors)
		}
		return rep
	}
	pass()
	if tree := remote.Tree(); tree != "Reports/ Reports/brief.pdf=the brief Reports/exhibit.pdf=exhibit A" {
		t.Fatalf("drive %s", tree)
	}
	if err := os.Rename(filepath.Join(root, "Reports", "brief.pdf"), filepath.Join(root, "Reports", "Brief (filed).pdf")); err != nil {
		t.Fatal(err)
	}
	rep := pass()
	if rep.Renames != 1 || remote.Reads["Reports/brief.pdf"] != 0 {
		t.Fatalf("renames %d, drive reads %v", rep.Renames, remote.Reads)
	}
	if tree := remote.Tree(); tree != "Reports/ Reports/Brief (filed).pdf=the brief Reports/exhibit.pdf=exhibit A" {
		t.Fatalf("drive %s", tree)
	}
}
