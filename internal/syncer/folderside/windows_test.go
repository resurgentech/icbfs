//go:build windows

package folderside_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/syncer/folderside"
)

// TestNTFSVolume: on NTFS the folder side has stable ids, real ACLs and
// 100ns times; forcing them off gives what a WebDAV drive would.
func TestNTFSVolume(t *testing.T) {
	root := t.TempDir()
	s, err := folderside.Open(root, folderside.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := s.Caps()
	if !c.IDs || !c.ACLs || c.TimeResolution != 100*time.Nanosecond {
		t.Fatalf("NTFS caps %+v", c)
	}
	put(t, root, "a.txt", "aaa", t0)
	ctx := context.Background()
	es, err := s.List(ctx)
	if err != nil || len(es) != 1 {
		t.Fatalf("list %+v, %v", es, err)
	}
	if !strings.HasPrefix(es[0].ACL, "O:") || es[0].ID == "" {
		t.Fatalf("entry %+v", es[0])
	}
	moved, err := s.Move(ctx, "a.txt", "sub/b.txt")
	if err != nil || moved.ID != es[0].ID {
		t.Fatalf("moved %+v (was %s), %v", moved, es[0].ID, err)
	}
	// An ACL set from SDDL reads back.
	sddl := es[0].ACL
	if err := s.SetACL(ctx, "sub/b.txt", sddl); err != nil {
		t.Fatal(err)
	}

	w, err := folderside.Open(root, folderside.Options{IDs: "off", ACLs: "off", TimeResolution: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c := w.Caps(); c.IDs || c.ACLs || c.TimeResolution != time.Second {
		t.Fatalf("forced caps %+v", c)
	}
}

// TestReadWhileOpenForWriting: a file an editor holds open can still be
// read, and replaced.
func TestReadWhileOpenForWriting(t *testing.T) {
	root := t.TempDir()
	put(t, root, "open.docx", "being edited", t0)
	held, err := os.OpenFile(filepath.Join(root, "open.docx"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	s, _ := folderside.Open(root, folderside.Options{})
	rc, err := s.Read(context.Background(), "open.docx")
	if err != nil {
		t.Fatalf("read while held: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "being edited" {
		t.Fatalf("read %q", data)
	}
}
