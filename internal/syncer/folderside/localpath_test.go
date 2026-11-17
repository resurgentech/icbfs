package folderside

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// winResolve is how Windows reads a path: "\" and "/" separate, "." and
// ".." resolve, from the volume root (C:\) of an absolute path. Enough of
// Win32's normalization to show where a joined path leads.
func winResolve(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	vol, rest, _ := strings.Cut(p, `\`)
	var out []string
	for _, n := range strings.Split(rest, `\`) {
		switch n {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, n)
		}
	}
	return vol + `\` + strings.Join(out, `\`)
}

// TestLocalPathWindows (T82-14): on Windows, the sync client's old join
// (filepath.Join(root, filepath.FromSlash(p))) of a drive path whose
// stored name holds "..\" leads out of the folder; localPath refuses
// such a name, and every path it gives stays under the folder.
func TestLocalPathWindows(t *testing.T) {
	root := `C:\Users\a\Matter`
	escapes := []string{`..\..\evil.txt`, `sub/..\..\..\evil.txt`, `..\Other\real.pdf`, "..", "a/../../x", `a\..\..\b`, `\Windows\x`, "... ", ". ."}
	for _, p := range escapes {
		// The old join, as Windows reads it.
		old := winResolve(root + `\` + strings.ReplaceAll(p, "/", `\`))
		if strings.Contains(p, `..`) && strings.Contains(p, `\`) && strings.HasPrefix(old, root+`\`) {
			t.Errorf("old join of %q = %q: expected it to escape (the finding)", p, old)
		}
		got, err := localPath(root, p, true)
		if !errors.Is(err, drive.ErrInvalidName) {
			t.Errorf("localPath(%q) = %q, %v; want refused", p, got, err)
		}
	}
	for _, p := range []string{"a/b.txt", "x..y", "a.b", "dir/sub/f"} {
		got, err := localPath(root, p, true)
		if err != nil || !strings.HasPrefix(winResolve(got), root+`\`) {
			t.Errorf("localPath(%q) = %q, %v; want under %s", p, got, err, root)
		}
	}
	if old := winResolve(root + `\` + `..\..\evil.txt`); old != `C:\Users\evil.txt` {
		t.Fatalf("old join leads to %q", old)
	}
}

// TestLocalPathPOSIX: on Linux a backslash is a name's character, so
// "..\..\x" is one file in the folder; ".." itself is refused.
func TestLocalPathPOSIX(t *testing.T) {
	root := "/home/a/Matter"
	got, err := localPath(root, `..\..\evil.txt`, false)
	if err != nil || got != `/home/a/Matter/..\..\evil.txt` {
		t.Fatalf("localPath = %q, %v", got, err)
	}
	for _, p := range []string{"..", "../x", "a/../../x", "a//b"} {
		if got, err := localPath(root, p, false); !errors.Is(err, drive.ErrInvalidName) {
			t.Errorf("localPath(%q) = %q, %v; want refused", p, got, err)
		}
	}
}

// TestSideNoEscape: the folder side writes, makes, moves and removes
// nothing outside its root, whatever path the drive side hands it; a
// name with a backslash is one file inside the root on Linux.
func TestSideNoEscape(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, p := range []string{"../outside.txt", "../evil.txt", "a/../../evil.txt"} {
		if _, err := s.Write(ctx, p, strings.NewReader("EVIL"), 4, now, time.Time{}); err == nil {
			t.Errorf("write %q: nil error", p)
		}
		if _, err := s.Mkdir(ctx, p+"d"); err == nil {
			t.Errorf("mkdir %q: nil error", p+"d")
		}
		if err := s.Remove(ctx, p); err == nil {
			t.Errorf("remove %q: nil error", p)
		}
		if rc, err := s.Read(ctx, p); err == nil {
			rc.Close()
			t.Errorf("read %q: nil error", p)
		}
	}
	if b, err := os.ReadFile(filepath.Join(parent, "outside.txt")); err != nil || string(b) != "OUTSIDE" {
		t.Fatalf("outside.txt = %q, %v", b, err)
	}
	es, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("the folder's parent holds %v", es)
	}
	if _, err := s.Write(ctx, `..\..\evil.txt`, strings.NewReader("ONE"), 3, now, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, `..\..\evil.txt`)); err != nil || string(b) != "ONE" {
		t.Fatalf(`root/..\..\evil.txt = %q, %v`, b, err)
	}
}
