package logd_test

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

	"github.com/resurgentech/icbfs/internal/fslog"
	"github.com/resurgentech/icbfs/internal/logd"
	"github.com/resurgentech/icbfs/internal/logserver"
)

const secret = "s3cret"

// server runs a logserver on addr ("127.0.0.1:0" for any port) until
// the returned stop is called.
func server(t *testing.T, addr, logPath string, allow ...string) (url string, stop func()) {
	t.Helper()
	s, err := logserver.New(logserver.Config{
		Secret: secret, Log: logPath, Allow: allow,
		Creds: fslog.Creds{Endpoint: "http://store", Bucket: "b", AccessKey: "ak", SecretKey: "sk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.Handler()}
	go hs.Serve(ln)
	return "ws://" + ln.Addr().String() + "/ws/daemon", func() {
		hs.Close()
		s.Close()
	}
}

func daemon(t *testing.T, socket, url string) (stop func()) {
	t.Helper()
	d, err := logd.New(logd.Config{Socket: socket, Server: url, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func events(t *testing.T, logPath string) []fslog.Event {
	t.Helper()
	f, err := os.Open(logPath)
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

func paths(evs []fslog.Event) string {
	var p []string
	for _, e := range evs {
		p = append(p, e.Op+" "+e.Path)
	}
	return strings.Join(p, ", ")
}

func TestCredentials(t *testing.T) {
	dir := t.TempDir()
	url, stop := server(t, "127.0.0.1:0", filepath.Join(dir, "log"), "allowed")
	defer stop()
	socket := filepath.Join(dir, "sock")
	defer daemon(t, socket, url)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := fslog.NewClient(socket, "allowed", false)
	defer c.Close(time.Second)
	creds, err := c.Credentials(ctx)
	if err != nil || creds.AccessKey != "ak" || creds.Endpoint != "http://store" {
		t.Fatalf("credentials: %+v, %v", creds, err)
	}
	other := fslog.NewClient(socket, "other", false)
	defer other.Close(time.Second)
	if _, err := other.Credentials(ctx); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("credentials for a disallowed filesystem: %v", err)
	}

	// No daemon at all: the mount gets no credentials, after ctx.
	none := fslog.NewClient(filepath.Join(dir, "nobody"), "allowed", false)
	short, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	if _, err := none.Credentials(short); err == nil {
		t.Fatal("credentials without a daemon succeeded")
	}
}

// TestSyncSurvivesServerAndDaemonRestarts: a synchronous Log blocks
// while the server is down and completes once it's back; a daemon
// restart is invisible; nothing is logged twice.
func TestSyncSurvivesServerAndDaemonRestarts(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	url, stopServer := server(t, "127.0.0.1:0", logPath)
	addr := strings.TrimSuffix(strings.TrimPrefix(url, "ws://"), "/ws/daemon")
	socket := filepath.Join(dir, "sock")
	stopDaemon := daemon(t, socket, url)

	c := fslog.NewClient(socket, "fs", false)
	defer c.Close(time.Second)
	c.Log(fslog.EventWrite, "/a", 3, 1, 2, 3)

	stopServer()
	done := make(chan struct{})
	go func() { c.Log(fslog.EventDelete, "/a", 0, 1, 2, 3); close(done) }()
	select {
	case <-done:
		t.Fatal("a synchronous Log returned while the server was down")
	case <-time.After(time.Second):
	}
	_, stopServer = server(t, addr, logPath)
	defer stopServer()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Log didn't complete after the server came back")
	}

	stopDaemon()
	defer daemon(t, socket, url)()
	c.Log(fslog.EventList, "/", 0, 1, 2, 3)

	got := events(t, logPath)
	if paths(got) != "write /a, delete /a, ls /" {
		t.Fatalf("log = %s", paths(got))
	}
	if got[0].Bytes != 3 || got[0].Uid != 1 || got[0].Gid != 2 || got[0].Pid != 3 || got[0].FS != "fs" {
		t.Fatalf("first event = %+v", got[0])
	}
}

// TestAsyncQueuesThenCountsDrops: async Log never blocks; with no
// daemon, events queue up to the limit and the rest are counted, and
// once a daemon appears everything queued is delivered along with the
// count.
func TestAsyncQueuesThenCountsDrops(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	socket := filepath.Join(dir, "sock")
	c := fslog.NewClient(socket, "fs", true)

	const n = 5000
	start := time.Now()
	for i := 0; i < n; i++ {
		c.Log(fslog.EventRead, fmt.Sprintf("/f%d", i), 0, 0, 0, 0)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("async Log blocked: %d events took %v with no daemon", n, time.Since(start))
	}

	url, stop := server(t, "127.0.0.1:0", logPath)
	defer stop()
	defer daemon(t, socket, url)()
	c.Close(30 * time.Second)

	got := events(t, logPath)
	var reads, dropped int64
	for _, e := range got {
		switch e.Op {
		case fslog.EventRead:
			reads++
		case fslog.EventDropped:
			dropped += e.Bytes
		}
	}
	if dropped == 0 || reads+dropped != n {
		t.Fatalf("%d logged + %d counted as dropped, want %d in all, some dropped", reads, dropped, n)
	}
}
