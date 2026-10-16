package fuseserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/fslog"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/logd"
	"github.com/resurgentech/icbfs/internal/logserver"
)

// TestMountLogsThroughDaemon is the whole chain, the way `icbfs mount
// -log-socket` runs it: the mount gets its store credentials from the
// mock server through the daemon (it has no others), then a set of
// filesystem actions through the real mount must come out of the
// server's log exactly — synchronously, and with -log-async.
func TestMountLogsThroughDaemon(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			ctx := context.Background()
			store := newMinIO(t)
			dir := t.TempDir()
			logPath := filepath.Join(dir, "events.jsonl")

			srv, err := logserver.New(logserver.Config{
				Secret: "s", Log: logPath, Drives: map[string]fslog.Mount{"logged-drive": {Filesystem: "logged"}},
				Creds: fslog.Creds{Endpoint: store.endpoint, Bucket: store.bucket, Region: "us-east-1", AccessKey: store.accessKey, SecretKey: store.secretKey},
			})
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			hs := &http.Server{Handler: srv.Handler()}
			go hs.Serve(ln)
			defer func() { hs.Close(); srv.Close() }()

			socket := filepath.Join(dir, "logd.sock")
			d, err := logd.New(logd.Config{Socket: socket, Server: "ws://" + ln.Addr().String() + "/ws/daemon", Secret: "s"})
			if err != nil {
				t.Fatal(err)
			}
			dctx, stopDaemon := context.WithCancel(ctx)
			defer stopDaemon()
			go d.Run(dctx)

			client := fslog.NewClient(socket, "logged-drive", async)
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			creds, m, err := client.Mount(cctx)
			cancel()
			if err != nil {
				t.Fatalf("mount: %v", err)
			}
			fsys := icbfs.New(newS3Store(testMinIO{endpoint: creds.Endpoint, bucket: creds.Bucket, accessKey: creds.AccessKey, secretKey: creds.SecretKey}), m.Filesystem)
			if err := fsys.Bootstrap(ctx, 1<<30, 0755, uint32(os.Getuid()), uint32(os.Getgid()), false); err != nil {
				t.Fatalf("bootstrap with the server's credentials: %v", err)
			}
			mnt := t.TempDir()
			server, err := Mount(mnt, Root(fsys, nil, WithLogger(client)), &fs.Options{
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

			// The actions, and what the server must log for them.
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
			client.Close(10 * time.Second) // async: deliver what's queued

			got := readLog(t, logPath)
			var lines []string
			for _, e := range got {
				lines = append(lines, fmt.Sprintf("%s %s %d", e.Op, e.Path, e.Bytes))
				if e.FS != "logged" || e.Drive != "logged-drive" || e.Uid != uint32(os.Getuid()) || e.Pid == 0 || e.Host == "" || e.ID == "" {
					t.Errorf("event %+v: wrong fs, uid, host or id, or no pid", e)
				}
			}
			if strings.Join(lines, "\n") != strings.Join(want, "\n") {
				t.Fatalf("server log:\n  %s\nwant:\n  %s", strings.Join(lines, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

func readLog(t *testing.T, path string) []fslog.Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []fslog.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e fslog.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}
