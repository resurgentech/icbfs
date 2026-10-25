package main

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/syncer/icbfsside"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

func TestConfigValidation(t *testing.T) {
	good := template()
	if err := good.validate(); err != nil {
		t.Fatalf("the template: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no endpoint": func(c *Config) { c.Store.S3.Endpoint = "" },
		"no secret":   func(c *Config) { c.Store.S3.SecretKey = "" },
		"no account":  func(c *Config) { c.Store.Kind, c.Store.Azure = "azure", pkgicbfs.Azure{Container: "c", Key: "k"} },
		"no folder":   func(c *Config) { c.Pairs[0].Folder = "" },
		"bad mode":    func(c *Config) { c.Pairs[0].Mode = "sideways" },
		"same name":   func(c *Config) { c.Pairs = append(c.Pairs, c.Pairs[0]) },
	} {
		c := template()
		mutate(c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIconsAreIcoFiles(t *testing.T) {
	for _, ico := range [][]byte{iconIdle, iconBusy, iconProblem, iconPaused} {
		if binary.LittleEndian.Uint16(ico[2:]) != 1 || binary.LittleEndian.Uint16(ico[4:]) != 1 || ico[6] != 32 {
			t.Fatal("not a one-image 32×32 .ico")
		}
		size, off := binary.LittleEndian.Uint32(ico[14:]), binary.LittleEndian.Uint32(ico[18:])
		if int(off+size) != len(ico) {
			t.Fatalf("image %d+%d, file %d", off, size, len(ico))
		}
	}
}

// TestOnce: a settings file, a folder and a bucket; icbfs-sync once puts
// the folder on the drive, and a second run has nothing to do.
func TestOnce(t *testing.T) {
	ctx := context.Background()
	store, srv := miniotest.Start(t)
	if _, err := icbfs.CreateFilesystem(ctx, store, icbfs.Settings{Name: "docs", Size: 1 << 30, RootMode: 0o755}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	folder := filepath.Join(dir, "folder")
	for p, data := range map[string]string{"A/one.txt": "one", "B/two.txt": "two", "~$lock.docx": "x"} {
		full := filepath.Join(folder, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(data), 0o644)
	}
	paths := Paths{Config: filepath.Join(dir, "config.json"), Data: filepath.Join(dir, "data")}
	cfg := &Config{
		Store: Store{StoreConfig: pkgicbfs.StoreConfig{S3: pkgicbfs.S3{Endpoint: srv.Endpoint, Bucket: srv.Bucket, AccessKey: srv.AccessKey, SecretKey: srv.SecretKey}}},
		Pairs: []Pair{{Name: "docs", Folder: folder, Filesystem: "docs", Root: "/", Mode: "one-way"}},
	}
	if err := saveConfig(paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	if err := once(paths, "", false); err != nil {
		t.Fatal(err)
	}
	remote, err := icbfsside.Open(ctx, store, "docs", "/", "test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	es, err := remote.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range es {
		got = append(got, e.Path)
	}
	if strings.Join(sorted(got), " ") != "A A/one.txt B B/two.txt" {
		t.Fatalf("drive %v", got)
	}
	// A second run, a new process: the state was kept, nothing to do.
	logBefore, _ := os.ReadFile(logPath(paths))
	if err := once(paths, "", false); err != nil {
		t.Fatal(err)
	}
	logAfter, _ := os.ReadFile(logPath(paths))
	if added := string(logAfter[len(logBefore):]); !strings.Contains(added, "0 of 0 done") {
		t.Fatalf("second run: %s", added)
	}
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}

// TestSyncNowListsTheDrive: in one-way mode a pass lists the drive only
// every remoteEvery, but Sync now's does, so a file put on the drive
// behind the folder's back goes at once.
func TestSyncNowListsTheDrive(t *testing.T) {
	ctx := context.Background()
	store, srv := miniotest.Start(t)
	if _, err := icbfs.CreateFilesystem(ctx, store, icbfs.Settings{Name: "docs", Size: 1 << 30, RootMode: 0o755}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	folder := filepath.Join(dir, "folder")
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, "kept.txt"), []byte("kept"), 0o644)
	paths := Paths{Config: filepath.Join(dir, "config.json"), Data: filepath.Join(dir, "data")}
	cfg := &Config{
		Store: Store{StoreConfig: pkgicbfs.StoreConfig{S3: pkgicbfs.S3{Endpoint: srv.Endpoint, Bucket: srv.Bucket, AccessKey: srv.AccessKey, SecretKey: srv.SecretKey}}},
		Pairs: []Pair{{Name: "docs", Folder: folder, Filesystem: "docs", Root: "/", Mode: "one-way", Interval: "1h", RemoteEvery: "1h"}},
	}
	log, err := openLog(logPath(paths), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	r, err := newRunner(cfg, paths, log)
	if err != nil {
		t.Fatal(err)
	}
	r.Start(ctx)
	defer r.Stop()
	remote, err := icbfsside.Open(ctx, store, "docs", "/", "test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	onDrive := func() string {
		es, err := remote.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range es {
			got = append(got, e.Path)
		}
		return strings.Join(sorted(got), " ")
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: drive has %q", what, onDrive())
			}
		}
	}
	until("the first pass", func() bool { return onDrive() == "kept.txt" })
	until("the first pass to finish", func() bool { return strings.HasPrefix(r.Status()[0].State, "up to date") })

	if _, err := remote.Write(ctx, "stray.txt", strings.NewReader("stray"), 5, time.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	r.SyncNow("docs")
	until("Sync now", func() bool { return onDrive() == "kept.txt" })
}
