package readcache

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// counter is a fetch source that counts its calls.
type counter struct {
	n    atomic.Int64
	data map[string][]byte // version -> bytes
	gate chan struct{}     // if set, each fetch waits for it
}

func (s *counter) fetch(version string) Fetch {
	return func(ctx context.Context) (io.ReadCloser, error) {
		s.n.Add(1)
		if s.gate != nil {
			select {
			case <-s.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		b, ok := s.data[version]
		if !ok {
			return nil, errors.New("no such version")
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

func open(t *testing.T, dir string, max int64) *Cache {
	t.Helper()
	c, err := Open(dir, Options{MaxBytes: max})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readAll(t *testing.T, f *os.File) []byte {
	t.Helper()
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCCE1_FirstGetFetchesOnceAndPublishes(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir, 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("hello")}}
	f, n, err := c.Get(context.Background(), "k", "v1", 5, src.fetch("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f); string(got) != "hello" || n != 5 {
		t.Fatalf("got %q (%d), want hello (5)", got, n)
	}
	if src.n.Load() != 1 {
		t.Fatalf("fetches %d, want 1", src.n.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "data", Name("k", "v1"))); err != nil {
		t.Fatalf("entry not published: %v", err)
	}
	if ents, _ := os.ReadDir(c.procDir); len(ents) != 1 { // owner.lock only: no tmp left
		t.Fatalf("proc dir holds %d names, want only owner.lock", len(ents))
	}
}

func TestCCE1_ShortFetchPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir, 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("hel")}}
	if _, _, err := c.Get(context.Background(), "k", "v1", 5, src.fetch("v1")); !errors.Is(err, ErrShort) {
		t.Fatalf("err %v, want ErrShort", err)
	}
	if c.Has("k", "v1") {
		t.Fatal("a short fetch was published")
	}
	if _, err := os.Stat(filepath.Join(dir, "data", Name("k", "v1"))); !os.IsNotExist(err) {
		t.Fatalf("entry file: %v, want none", err)
	}
	if ents, _ := os.ReadDir(c.procDir); len(ents) != 1 {
		t.Fatalf("proc dir holds %d names, want only owner.lock", len(ents))
	}
	// A failed fetch: the same.
	if _, _, err := c.Get(context.Background(), "k", "v9", -1, src.fetch("v9")); err == nil {
		t.Fatal("a failed fetch answered")
	}
	if c.Stats().Entries != 0 {
		t.Fatal("a failed fetch left an entry")
	}
}

func TestCCE2_SecondGetFetchesNothing(t *testing.T) {
	c := open(t, t.TempDir(), 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("hello")}}
	for i := range 3 {
		f, _, err := c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, f); string(got) != "hello" {
			t.Fatalf("get %d: %q", i, got)
		}
	}
	if src.n.Load() != 1 {
		t.Fatalf("fetches %d, want 1", src.n.Load())
	}
	if s := c.Stats(); s.Hits != 2 || s.Misses != 1 {
		t.Fatalf("stats %+v, want 2 hits 1 miss", s)
	}
}

func TestCCE3_TwoGetsOneFetch(t *testing.T) {
	c := open(t, t.TempDir(), 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("hello")}, gate: make(chan struct{})}
	var wg sync.WaitGroup
	got := make([]string, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, _, err := c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
			errs[i] = err
			if err == nil {
				b, _ := io.ReadAll(f)
				f.Close()
				got[i] = string(b)
			}
		}()
	}
	// Every Get is in: one fetching, three waiting.
	waitFor(t, func() bool { s := c.Stats(); return s.Misses == 1 && s.Waits == 3 })
	close(src.gate)
	wg.Wait()
	for i := range 4 {
		if errs[i] != nil || got[i] != "hello" {
			t.Fatalf("get %d: %q %v", i, got[i], errs[i])
		}
	}
	if src.n.Load() != 1 {
		t.Fatalf("fetches %d, want 1", src.n.Load())
	}
	// A failed fetch fails its waiters too, with one fetch.
	bad := &counter{data: map[string][]byte{}, gate: make(chan struct{})}
	var wg2 sync.WaitGroup
	var failed atomic.Int32
	for range 2 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if _, _, err := c.Get(context.Background(), "k", "v2", -1, bad.fetch("v2")); err != nil {
				failed.Add(1)
			}
		}()
	}
	waitFor(t, func() bool { return c.Stats().Waits == 4 })
	close(bad.gate)
	wg2.Wait()
	if failed.Load() != 2 || bad.n.Load() != 1 {
		t.Fatalf("failed %d fetches %d, want 2 and 1", failed.Load(), bad.n.Load())
	}
}

func TestCCE3_LeaderCancelledWaiterFetches(t *testing.T) {
	c := open(t, t.TempDir(), 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("hello")}, gate: make(chan struct{})}
	lctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := c.Get(lctx, "k", "v1", -1, src.fetch("v1"))
		leaderDone <- err
	}()
	waitFor(t, func() bool { return src.n.Load() == 1 })
	waiter := make(chan string, 1)
	go func() {
		f, _, err := c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
		if err != nil {
			waiter <- "err " + err.Error()
			return
		}
		b, _ := io.ReadAll(f)
		f.Close()
		waiter <- string(b)
	}()
	waitFor(t, func() bool { return c.Stats().Waits == 1 })
	cancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: %v", err)
	}
	close(src.gate)
	if got := <-waiter; got != "hello" {
		t.Fatalf("waiter: %s", got)
	}
}

func TestCCE4_NewVersionIsAMiss(t *testing.T) {
	c := open(t, t.TempDir(), 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("one"), "v2": []byte("two")}}
	f, _, _ := c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
	readAll(t, f)
	f, _, err := c.Get(context.Background(), "k", "v2", -1, src.fetch("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f); string(got) != "two" {
		t.Fatalf("v2 read %q", got)
	}
	if src.n.Load() != 2 {
		t.Fatalf("fetches %d, want 2", src.n.Load())
	}
	f, _, _ = c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
	if got := readAll(t, f); string(got) != "one" {
		t.Fatalf("v1 read %q", got)
	}
}

func TestCCE5_EvictedEntryStillReadsThroughItsFd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an open file is not removed on Windows")
	}
	c := open(t, t.TempDir(), 10)
	src := &counter{data: map[string][]byte{"a": []byte("aaaaaaaa"), "b": []byte("bbbbbbbb")}}
	fa, _, err := c.Get(context.Background(), "k", "a", -1, src.fetch("a"))
	if err != nil {
		t.Fatal(err)
	}
	fb, _, err := c.Get(context.Background(), "k", "b", -1, src.fetch("b"))
	if err != nil {
		t.Fatal(err)
	}
	fb.Close()
	if c.Has("k", "a") {
		t.Fatal("a was not evicted")
	}
	if got := readAll(t, fa); string(got) != "aaaaaaaa" {
		t.Fatalf("evicted entry read %q", got)
	}
	f, _, _ := c.Get(context.Background(), "k", "a", -1, src.fetch("a"))
	readAll(t, f)
	if src.n.Load() != 3 {
		t.Fatalf("fetches %d, want 3 (a fetched again)", src.n.Load())
	}
}

// errReader gives some bytes, then fails: a copy cut (as by a full disk
// or a broken connection).
type errReader struct{ n int }

func (r *errReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, errors.New("no space left on device")
	}
	k := min(len(p), r.n)
	r.n -= k
	return k, nil
}

func TestCCE6_FailedCopyPublishesNothing(t *testing.T) {
	c := open(t, t.TempDir(), 1<<20)
	ok := &counter{data: map[string][]byte{"v1": []byte("kept")}}
	f, _, _ := c.Get(context.Background(), "other", "v1", -1, ok.fetch("v1"))
	readAll(t, f)
	_, _, err := c.Get(context.Background(), "k", "v1", -1, func(ctx context.Context) (io.ReadCloser, error) {
		return io.NopCloser(&errReader{n: 3}), nil
	})
	if err == nil {
		t.Fatal("a cut copy answered")
	}
	if c.Has("k", "v1") {
		t.Fatal("a cut copy was published")
	}
	if ents, _ := os.ReadDir(c.procDir); len(ents) != 1 {
		t.Fatalf("proc dir holds %d names, want only owner.lock", len(ents))
	}
	f, _, err = c.Get(context.Background(), "other", "v1", -1, ok.fetch("v1"))
	if err != nil || string(readAll(t, f)) != "kept" || ok.n.Load() != 1 {
		t.Fatalf("the other entry: %v, fetches %d", err, ok.n.Load())
	}
}

func TestCCE9_DirectoryRemovedUnderTheCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	c := open(t, dir, 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("hello")}}
	f, _, _ := c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
	held := f
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	g, _, err := c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
	if err != nil {
		t.Fatalf("after the directory went: %v", err)
	}
	if got := readAll(t, g); string(got) != "hello" {
		t.Fatalf("read %q", got)
	}
	if src.n.Load() != 2 {
		t.Fatalf("fetches %d, want 2", src.n.Load())
	}
	if runtime.GOOS != "windows" {
		if got := readAll(t, held); string(got) != "hello" {
			t.Fatalf("the fd held across the removal read %q", got)
		}
		st, err := os.Stat(filepath.Join(dir, "data"))
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("data made again: %v %v", st, err)
		}
	} else {
		held.Close()
	}
}

func TestCCE15_RefusesADirectoryOthersCanWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{OwnDir: true}); !errors.Is(err, ErrUnsafeDir) {
		t.Fatalf("OwnDir on a 0777 directory: %v, want ErrUnsafeDir", err)
	}
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{OwnDir: true}); !errors.Is(err, ErrUnsafeDir) {
		t.Fatalf("OwnDir on a 0775 directory: %v, want ErrUnsafeDir", err)
	}
	// Without the option (icbfs mount's -cache-dir) it opens as before.
	c, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("without OwnDir: %v", err)
	}
	c.Close()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err = Open(dir, Options{OwnDir: true})
	if err != nil {
		t.Fatalf("OwnDir on a 0755 directory of the user's: %v", err)
	}
	c.Close()
	if os.Getuid() != 0 {
		// Owned by another user: root's /.
		if err := CheckDir("/"); !errors.Is(err, ErrUnsafeDir) {
			t.Fatalf("CheckDir(/) as uid %d: %v, want ErrUnsafeDir", os.Getuid(), err)
		}
	}
}

func TestCCE15_ModesAreOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	dir := filepath.Join(t.TempDir(), "new")
	c := open(t, dir, 1<<20)
	src := &counter{data: map[string][]byte{"v1": []byte("hello")}}
	f, _, _ := c.Get(context.Background(), "k", "v1", -1, src.fetch("v1"))
	readAll(t, f)
	s, err := c.NewScratch()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			return fmt.Errorf("%s: %04o, want %04o", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCCE16_LargerThanTheBudgetIsNotCached(t *testing.T) {
	c := open(t, t.TempDir(), 10)
	src := &counter{data: map[string][]byte{"small": []byte("12345678"), "big": bytes.Repeat([]byte("x"), 20)}}
	f, _, _ := c.Get(context.Background(), "s", "small", -1, src.fetch("small"))
	readAll(t, f)
	for range 2 {
		f, n, err := c.Get(context.Background(), "b", "big", 20, src.fetch("big"))
		if err != nil || n != 20 {
			t.Fatalf("big: %d %v", n, err)
		}
		if got := readAll(t, f); len(got) != 20 {
			t.Fatalf("big read %d bytes", len(got))
		}
	}
	if c.Has("b", "big") || !c.Has("s", "small") {
		t.Fatal("the big content was cached, or the small one evicted for it")
	}
	if src.n.Load() != 3 {
		t.Fatalf("fetches %d, want 3 (big each time)", src.n.Load())
	}
	if err := c.Put("b", "big2", bytes.NewReader(make([]byte, 20)), 20); err != nil || c.Has("b", "big2") {
		t.Fatalf("Put of more than the bound: %v, cached %v", err, c.Has("b", "big2"))
	}
}

func TestCCB1_EvictsOldestToBothBounds(t *testing.T) {
	c := open(t, t.TempDir(), 30)
	src := &counter{data: map[string][]byte{}}
	for i := range 5 {
		v := strconv.Itoa(i)
		src.data[v] = bytes.Repeat([]byte{byte('a' + i)}, 10)
	}
	get := func(v string) {
		f, _, err := c.Get(context.Background(), "k", v, -1, src.fetch(v))
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, f)
	}
	get("0")
	get("1")
	get("2")
	get("0") // used: 1 is the oldest now
	get("3")
	if c.Has("k", "1") || !c.Has("k", "0") || !c.Has("k", "2") || !c.Has("k", "3") {
		t.Fatal("not the least recently used evicted")
	}
	if s := c.Stats(); s.Bytes > 30 || s.Entries != 3 {
		t.Fatalf("stats %+v, want at most 30 bytes in 3 entries", s)
	}
	// The entry bound: a cache of empty files.
	e := open(t, t.TempDir(), 1)
	if e.MaxEntries() != minMaxEntries {
		t.Fatalf("max entries %d", e.MaxEntries())
	}
	for i := range minMaxEntries + 10 {
		if err := e.Put("k", strconv.Itoa(i), bytes.NewReader(nil), 0); err != nil {
			t.Fatal(err)
		}
	}
	if s := e.Stats(); s.Entries != minMaxEntries {
		t.Fatalf("%d empty entries, want %d", s.Entries, minMaxEntries)
	}
}

// TestCCE8_StartRebuildsIndexAndSweepsTheDead: a process that fetched
// entries and was killed mid-fetch (SIGKILL) leaves its proc dir and a
// tmp-*; the next Open removes them, keeps a live process's, reads the
// entries into its index (a Get of each fetches nothing) and evicts down
// to its own bound.
func TestCCE8_StartRebuildsIndexAndSweepsTheDead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper is killed with SIGKILL")
	}
	dir := t.TempDir()
	dead, deadProc := startHelper(t, dir, "fill-and-hang")
	_, liveProc := startHelper(t, dir, "hold")
	if err := dead.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = dead.Wait()
	if tmps, _ := filepath.Glob(filepath.Join(deadProc, "tmp-*")); len(tmps) != 1 {
		t.Fatalf("the killed helper left %d tmp files, want 1", len(tmps))
	}

	c := open(t, dir, 1<<20)
	if _, err := os.Stat(deadProc); !os.IsNotExist(err) {
		t.Fatalf("the killed process's dir after Open: %v, want gone", err)
	}
	if _, err := os.Stat(liveProc); err != nil {
		t.Fatalf("the live process's dir after Open: %v, want kept", err)
	}
	src := &counter{data: map[string][]byte{}}
	for i := range 3 {
		v := strconv.Itoa(i)
		f, _, err := c.Get(context.Background(), "k", v, -1, src.fetch(v))
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, f); string(got) != "entry "+v {
			t.Fatalf("entry %s read %q", v, got)
		}
	}
	if src.n.Load() != 0 {
		t.Fatalf("fetches %d after a restart, want 0", src.n.Load())
	}
	c.Close()
	// A smaller bound at the next start: evicted down to it at once.
	small := open(t, dir, 8)
	if s := small.Stats(); s.Entries != 1 || s.Bytes > 8 {
		t.Fatalf("after Open with a bound of 8: %+v", s)
	}
	if n := len(must(filepath.Glob(filepath.Join(dir, "data", "*")))); n != 1 {
		t.Fatalf("%d entry files left, want 1", n)
	}
}

// TestCCB2_TwoProcessesOneDirectoryWithinTheBound (rows CC-E3b and
// CC-B2): two processes on one
// directory fetch the same entries and more than the bound together; each
// gets right bytes, and the directory ends within the bound.
func TestCCB2_TwoProcessesOneDirectoryWithinTheBound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper processes")
	}
	dir := t.TempDir()
	const bound = 4096
	var wg sync.WaitGroup
	outs := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestReadcacheHelper$")
			cmd.Env = append(os.Environ(), "READCACHE_HELPER=churn", "READCACHE_DIR="+dir, "READCACHE_BOUND="+strconv.Itoa(bound))
			out, err := cmd.CombinedOutput()
			outs[i] = string(out)
			if err != nil {
				outs[i] += " ERR " + err.Error()
			}
		}()
	}
	wg.Wait()
	for i, o := range outs {
		if !strings.Contains(o, "CHURN OK") {
			t.Fatalf("helper %d: %s", i, o)
		}
	}
	var total int64
	for _, p := range must(filepath.Glob(filepath.Join(dir, "data", "*"))) {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	if total > bound {
		t.Fatalf("the directory holds %d bytes after both, bound %d", total, bound)
	}
}

func startHelper(t *testing.T, dir, mode string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReadcacheHelper$")
	cmd.Env = append(os.Environ(), "READCACHE_HELPER="+mode, "READCACHE_DIR="+dir)
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
	return cmd, strings.TrimSpace(strings.TrimPrefix(line, "PROC "))
}

// TestReadcacheHelper is the other process of the tests above.
func TestReadcacheHelper(t *testing.T) {
	mode := os.Getenv("READCACHE_HELPER")
	if mode == "" {
		t.Skip("run by other tests")
	}
	dir := os.Getenv("READCACHE_DIR")
	bound, _ := strconv.ParseInt(os.Getenv("READCACHE_BOUND"), 10, 64)
	c, err := Open(dir, Options{MaxBytes: bound})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	switch mode {
	case "fill-and-hang":
		for i := range 3 {
			v := strconv.Itoa(i)
			b := []byte("entry " + v)
			f, _, err := c.Get(ctx, "k", v, -1, func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		// A fetch that never ends: killed in it.
		go c.Get(ctx, "k", "hang", -1, func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(io.MultiReader(strings.NewReader("part"), blockReader{})), nil
		})
		for {
			if tmps, _ := filepath.Glob(filepath.Join(c.procDir, "tmp-*")); len(tmps) == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		fmt.Printf("PROC %s\n", c.procDir)
		time.Sleep(time.Minute)
	case "hold":
		fmt.Printf("PROC %s\n", c.procDir)
		time.Sleep(time.Minute)
	case "churn":
		for i := range 200 {
			v := strconv.Itoa(i % 40)
			want := bytes.Repeat([]byte{byte('A' + i%40)}, 300)
			f, _, err := c.Get(ctx, "k", v, 300, func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(want)), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(f)
			f.Close()
			if !bytes.Equal(got, want) {
				t.Fatalf("entry %s: wrong bytes", v)
			}
		}
		c.Close()
		fmt.Println("CHURN OK")
	}
}

type blockReader struct{}

func (blockReader) Read([]byte) (int, error) { select {} }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 20 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
