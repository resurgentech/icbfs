package icbfs

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestME6_ClosedFilesystemsLeaveNoCacheDirectory: docs/STATES.md ME-6 (in
// interloc), its cache directory: N Filesystems each open a file (which
// makes the disk cache's directory) and are closed; the temp directory is
// left empty. A Filesystem that never opened a file makes none, and a
// directory a caller gave (SetCacheOptions) is never removed.
func TestME6_ClosedFilesystemsLeaveNoCacheDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	fsys, store := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "a.txt", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fsys.Close()

	const n = 20
	for i := range n {
		f := New(store, "test")
		if err := f.Attach(ctx); err != nil {
			t.Fatal(err)
		}
		of, err := f.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if got := cacheDirsIn(t, tmp); len(got) != 1 {
				t.Fatalf("an open file: %d cache directories in the temp directory, want 1: %v", len(got), got)
			}
		}
		of.Close()
		f.Close()
		f.Close() // twice: nothing more
	}
	if got := cacheDirsIn(t, tmp); len(got) != 0 {
		t.Fatalf("after %d Filesystems opened and closed: %d cache directories left: %v", n, len(got), got)
	}

	// Never used: no directory at all.
	idle := New(store, "test")
	if got := cacheDirsIn(t, tmp); len(got) != 0 {
		t.Fatalf("a Filesystem that opened nothing made %v", got)
	}
	idle.Close()

	// A closed Filesystem's cache makes no new directory.
	if _, err := idle.cache.newScratchFile(); err == nil {
		t.Fatal("a closed Filesystem's cache made a scratch file")
	}
	if got := cacheDirsIn(t, tmp); len(got) != 0 {
		t.Fatalf("a closed Filesystem made %v", got)
	}

	// A caller's directory stays.
	given := filepath.Join(t.TempDir(), "given")
	f := New(store, "test")
	f.SetCacheOptions(given, 1<<20)
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	of, err := f.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	of.Close()
	f.Close()
	if _, err := os.Stat(given); err != nil {
		t.Fatalf("the caller's cache directory: %v", err)
	}
}

// TestME6_KilledProcessCacheDirectoryGoneAtNextStart: a process that made
// a cache directory and was killed (SIGKILL: no Close, no deferred code)
// leaves it; the next process's first New removes it. A live process's
// directory, one made by older code (no lock file), and anything else in
// the temp directory are left.
func TestME6_KilledProcessCacheDirectoryGoneAtNextStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper is killed with SIGKILL")
	}
	tmp := t.TempDir()
	env := append(os.Environ(), "TMPDIR="+tmp, "ICBFS_CACHEDIR_HELPER=1")

	start := func(mode string) (*exec.Cmd, string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCacheDirHelper$")
		cmd.Env = append(env, "ICBFS_CACHEDIR_MODE="+mode)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil {
			t.Fatalf("helper %s: %v", mode, err)
		}
		return cmd, strings.TrimSpace(strings.TrimPrefix(line, "DIR "))
	}

	dead, deadDir := start("hold")
	live, liveDir := start("hold")
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("the helper's directory: %v", err)
	}
	if err := dead.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = dead.Wait()
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("a killed process's directory is gone before any start: %v", err)
	}
	old := filepath.Join(tmp, cacheDirPrefix+"older-code")
	if err := os.MkdirAll(filepath.Join(old, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(tmp, "not-icbfs")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}

	// The next start: a process whose first New sweeps.
	next := exec.Command(os.Args[0], "-test.run=^TestCacheDirHelper$")
	next.Env = append(env, "ICBFS_CACHEDIR_MODE=new")
	if out, err := next.CombinedOutput(); err != nil {
		t.Fatalf("next start: %v\n%s", err, out)
	}

	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("the killed process's directory after the next start: %v, want gone", err)
	}
	for _, keep := range []string{liveDir, old, other} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%s after the next start: %v, want kept", keep, err)
		}
	}
	// In this process too: a live Filesystem's directory is locked by its
	// own open file, so a sweep here leaves it.
	if n := removeDeadCacheDirs(tmp); n != 0 {
		t.Fatalf("a sweep with only live and older directories removed %d", n)
	}
	_ = live.Process.Kill()
	_ = live.Wait()
	if n := removeDeadCacheDirs(tmp); n != 1 {
		t.Fatalf("after the live helper is killed a sweep removed %d, want 1", n)
	}
}

// TestCacheDirHelper is TestME6_KilledProcessCacheDirectoryGoneAtNextStart's
// other process: "hold" makes a Filesystem's cache directory, prints it
// and waits to be killed; "new" calls New once (the next start's sweep).
func TestCacheDirHelper(t *testing.T) {
	if os.Getenv("ICBFS_CACHEDIR_HELPER") != "1" {
		t.Skip("run by TestME6_KilledProcessCacheDirectoryGoneAtNextStart")
	}
	f := New(nil, "helper")
	switch os.Getenv("ICBFS_CACHEDIR_MODE") {
	case "hold":
		sf, err := f.cache.newScratchFile()
		if err != nil {
			t.Fatal(err)
		}
		defer sf.Close()
		fmt.Printf("DIR %s\n", f.cache.dir)
		time.Sleep(time.Minute)
	case "new":
	}
}

// cacheDirsIn is the icbfs-cache-* entries of dir.
func cacheDirsIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), cacheDirPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}
