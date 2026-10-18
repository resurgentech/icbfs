package fuseserver

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestRewinddirShowsTheDirectoryNow: rewinding an open directory and
// reading it again shows it as it is now, as POSIX says rewinddir(3) does:
// a name removed since the open is gone, one added is there
// (Connectathon's basic test6 removes a file per pass and rereads).
func TestRewinddirShowsTheDirectoryNow(t *testing.T) {
	mnt := mountTestFS(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(mnt, n), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	d, err := os.Open(mnt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	names := func() []string {
		t.Helper()
		if _, err := d.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, err := d.Readdirnames(-1)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		return got
	}
	if got := names(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("first listing %v", got)
	}
	if err := os.Remove(filepath.Join(mnt, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "d"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if got := names(); !slices.Equal(got, []string{"b", "c", "d"}) {
		t.Fatalf("after a rewind: %v, want [b c d]", got)
	}
}
