package fuseserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// recorder is a Logger that keeps what the mount logs, as the daemon's
// client would relay it.
type recorder struct {
	mu    sync.Mutex
	lines []string
	uids  map[uint32]bool
	pids  map[uint32]bool
}

func (r *recorder) Log(op, path string, bytes int64, uid, gid, pid uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf("%s %s %d", op, path, bytes))
	if r.uids == nil {
		r.uids, r.pids = map[uint32]bool{}, map[uint32]bool{}
	}
	r.uids[uid], r.pids[pid] = true, true
}

// TestMountLogs is the mount's side of `icbfs mount -log-socket`: a set
// of filesystem actions through a real mount must reach its Logger
// exactly, with the caller's uid and pid.
func TestMountLogs(t *testing.T) {
	ctx := context.Background()
	store := newMinIO(t)
	fsys := icbfs.New(newS3Store(store), "logged")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	mnt := t.TempDir()
	server, err := Mount(mnt, Root(fsys, nil, WithLogger(rec)), &fs.Options{
		MountOptions:    fuse.MountOptions{FsName: "icbfs-log", Name: "icbfs-log", Options: []string{"default_permissions"}},
		NullPermissions: true,
	})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	unmounted := false
	defer func() {
		if !unmounted {
			server.Unmount()
		}
	}()

	// The actions, and what the mount must log for them.
	p := func(rel string) string { return filepath.Join(mnt, rel) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(p("d"), 0755))
	must(os.WriteFile(p("d/a"), []byte("hello"), 0644)) // write /d/a 5
	_, err = os.ReadFile(p("d/a"))                      // read /d/a
	must(err)
	_, err = os.ReadDir(p("d")) // ls /d
	must(err)
	f, err := os.OpenFile(p("d/a"), os.O_WRONLY|os.O_APPEND, 0) // write-only: no read
	must(err)
	_, err = f.Write([]byte(" world"))
	must(err)
	must(f.Close())            // write /d/a 11
	f, err = os.Open(p("d/a")) // read /d/a, even unread
	must(err)
	must(f.Close())                     // nothing written: no write
	must(os.Rename(p("d/a"), p("d/b"))) // not logged
	must(os.Remove(p("d/b")))           // delete /d/b
	must(os.Remove(p("d")))             // delete /d
	_, err = os.ReadDir(mnt)            // ls /
	must(err)
	want := []string{
		"write /d/a 5",
		"read /d/a 0",
		"ls /d 0",
		"write /d/a 11",
		"read /d/a 0",
		"delete /d/b 0",
		"delete /d 0",
		"ls / 0",
	}

	unmounted = true
	if err := server.Unmount(); err != nil {
		t.Fatalf("unmount: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if strings.Join(rec.lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged:\n  %s\nwant:\n  %s", strings.Join(rec.lines, "\n  "), strings.Join(want, "\n  "))
	}
	if !rec.uids[uint32(os.Getuid())] || len(rec.uids) != 1 || rec.pids[0] {
		t.Errorf("uids %v pids %v: want this uid only, and a pid on every event", rec.uids, rec.pids)
	}
}
