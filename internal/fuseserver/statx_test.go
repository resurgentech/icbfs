package fuseserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// TestStatxReportsCreationTime: statx(2) on the mount gives an item's
// creation time, the one its key records for a new item, and one set
// from elsewhere (a Windows mount, the sync) once it's set.
func TestStatxReportsCreationTime(t *testing.T) {
	dir, fsys := mountTestFSWithFsys(t)
	p := filepath.Join(dir, "brief.txt")
	before := time.Now().Add(-time.Second)
	if err := os.WriteFile(p, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	birth := func(p string) time.Time {
		t.Helper()
		var st unix.Statx_t
		if err := unix.Statx(unix.AT_FDCWD, p, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &st); err != nil {
			t.Fatal(err)
		}
		if st.Mask&unix.STATX_BTIME == 0 {
			t.Fatalf("%s: no creation time in statx (mask %#x)", p, st.Mask)
		}
		return time.Unix(st.Btime.Sec, int64(st.Btime.Nsec)).UTC()
	}
	if got := birth(p); got.Before(before) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("a new file's creation time %v, not about now", got)
	}

	born := time.Date(2019, 3, 4, 5, 6, 7, 800, time.UTC)
	set := func(name string, typ icbfs.EntryType) {
		t.Helper()
		entry, _, err := fsys.Lookup(context.Background(), fsys.RootKey(), name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fsys.SetAttr(context.Background(), entry.UUID, typ, nil, nil, nil, nil, nil, nil, &born); err != nil {
			t.Fatal(err)
		}
	}
	set("brief.txt", icbfs.TypeFile)
	if err := os.Mkdir(filepath.Join(dir, "Matters"), 0o755); err != nil {
		t.Fatal(err)
	}
	set("Matters", icbfs.TypeDir)
	// Past the kernel's attribute cache.
	time.Sleep(2 * time.Second)
	for _, name := range []string{"brief.txt", "Matters"} {
		if got := birth(filepath.Join(dir, name)); !got.Equal(born) {
			t.Fatalf("%s: creation time %v, want %v", name, got, born)
		}
	}
}

// TestTimesSetBeforeCloseStay: cp -a, tar and rsync's in-place mode
// write a file and set its times on the open descriptor before closing
// it; the close's flush mustn't take the modified time back. A write
// after the time is set moves it, as anywhere.
func TestTimesSetBeforeCloseStay(t *testing.T) {
	dir := mountTestFS(t)
	old := time.Date(2012, 12, 12, 12, 12, 12, 345678900, time.UTC)
	write := func(name string, after bool) {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("content")); err != nil {
			t.Fatal(err)
		}
		ts := []unix.Timespec{unix.NsecToTimespec(old.UnixNano()), unix.NsecToTimespec(old.UnixNano())}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, f.Name(), ts, 0); err != nil {
			t.Fatal(err)
		}
		if after {
			if _, err := f.Write([]byte(" and more")); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write("kept", false)
	write("moved", true)
	time.Sleep(2 * time.Second) // past the kernel's attribute cache
	for name, want := range map[string]bool{"kept": true, "moved": false} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.ModTime().Equal(old); got != want {
			t.Fatalf("%s: modified %v; the time set before close kept: %v, want %v", name, st.ModTime().UTC(), got, want)
		}
	}
}
