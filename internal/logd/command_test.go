package logd_test

import (
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
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/logserver"
	"github.com/resurgentech/icbfs/internal/storeconfig"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestCommandsThroughTheDaemon: icbfs-cli's side commands go through the
// daemon to the server, which runs them on its store as the account the
// daemon saw on its socket — whatever the client claimed — logs them, and
// refuses the ones it doesn't allow.
func TestCommandsThroughTheDaemon(t *testing.T) {
	_, srv := miniotest.Start(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	s, err := logserver.New(logserver.Config{
		Secret: secret, Log: logPath,
		Creds:         fslog.Creds{S3: storeconfig.S3{Endpoint: srv.Endpoint, Bucket: srv.Bucket, Region: "us-east-1", AccessKey: srv.AccessKey, SecretKey: srv.SecretKey}},
		AllowCommands: []string{"fs create", "names create", "names list"},
		Drives:        map[string]fslog.Mount{"team": {Filesystem: "viadaemon"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.Handler()}
	go hs.Serve(ln)
	defer func() { hs.Close(); s.Close() }()
	socket := filepath.Join(dir, "logd.sock")
	stop := daemon(t, socket, "ws://"+ln.Addr().String()+"/ws/daemon")
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := fslog.NewClient(socket, "", false)

	res, err := c.Command(ctx, "fs create", map[string]string{"fs": "viadaemon", "size": "1073741824", "root-mode": "0755"})
	if err != nil {
		t.Fatalf("fs create: %v", err)
	}
	var info icbfs.Info
	if err := json.Unmarshal(res, &info); err != nil || info.Name != "viadaemon" {
		t.Fatalf("fs create result %s: %v", res, err)
	}
	time.Sleep(1100 * time.Millisecond)
	// Drive commands name the drive; the server knows its filesystem.
	if _, err := c.Command(ctx, "names create", map[string]string{"drive": "team", "name": "first"}); err != nil {
		t.Fatalf("names create: %v", err)
	}
	res, err = c.Command(ctx, "names list", map[string]string{"drive": "team"})
	if err != nil {
		t.Fatal(err)
	}
	var names []drive.Name
	if err := json.Unmarshal(res, &names); err != nil || len(names) != 1 {
		t.Fatalf("names: %s, %v", res, err)
	}
	host, _ := os.Hostname()
	if want := fmt.Sprintf("%s/uid:%d", host, os.Getuid()); names[0].CreatedBy != want {
		t.Fatalf("name made by %q; want the daemon's account %q", names[0].CreatedBy, want)
	}

	if _, err := c.Command(ctx, "fs archive", map[string]string{"fs": "viadaemon"}); err == nil || !strings.Contains(err.Error(), "may not fs archive") {
		t.Fatalf("a command the server doesn't allow: %v", err)
	}
	if _, err := c.Command(ctx, "names create", map[string]string{"drive": "nope", "name": "x"}); err == nil || !strings.Contains(err.Error(), `no drive "nope"`) {
		t.Fatalf("a drive not in the registry: %v", err)
	}

	var cmds []string
	for _, e := range events(t, logPath) {
		if e.Op == fslog.EventCommand {
			cmds = append(cmds, e.Path)
			if !strings.HasSuffix(e.Actor, fmt.Sprintf("/uid:%d", os.Getuid())) {
				t.Errorf("logged actor %q", e.Actor)
			}
		}
	}
	if got := strings.Join(cmds, ","); got != "fs create,names create,names list,names create" {
		t.Fatalf("logged commands: %s", got)
	}
}
