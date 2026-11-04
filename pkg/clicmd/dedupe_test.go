package clicmd

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/storeflags"
)

// fakeStore is a command's store flags that name a store already open.
type fakeStore struct{ store objstore.Store }

func (fakeStore) AddFlags(*flag.FlagSet)                         {}
func (fakeStore) Check() error                                   { return nil }
func (s fakeStore) Open(context.Context) (objstore.Store, error) { return s.store, nil }
func (fakeStore) String() string                                 { return "test" }
func (fakeStore) Identity() string                               { return "test" }

// capture runs Main with args, returning its status and what it wrote to
// stdout and stderr.
func capture(t *testing.T, store objstore.Store, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	status = Main(args, func() storeflags.Store { return fakeStore{store} })
	outW.Close()
	errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	return status, string(o), string(e)
}

// TestFsDedupe: the CLI writes the dump file (or stdout for "-", the
// summary then going to stderr), prints the dry run's numbers; without
// -dry-run it replaces the duplicates and prints what it wrote and what
// it skipped, and why; -history rewrites a duplicate in a file's past.
func TestFsDedupe(t *testing.T) {
	store := miniotest.Store(t)
	ctx := context.Background()
	if st, _, e := capture(t, store, "fs", "create", "-fs", "t"); st != 0 {
		t.Fatal(e)
	}
	f := icbfs.New(store, "t")
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		key, _, etag, err := f.Create(ctx, f.RootKey(), name, 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteFile(ctx, key, []byte(strings.Repeat("a", 300)), etag); err != nil {
			t.Fatal(err)
		}
	}

	dump := filepath.Join(t.TempDir(), "dump.jsonl")
	st, out, e := capture(t, store, "fs", "dedupe", "-fs", "t", "-dry-run", "-min-size", "100", "-dump", dump)
	if st != 0 {
		t.Fatalf("status %d: %s", st, e)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 { // a, b: Create's empty first versions are below the minimum
		t.Fatalf("dump has %d lines, want 2:\n%s", len(lines), data)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil || rec["size"] != float64(300) {
		t.Errorf("first line %s: %v", lines[0], err)
	}
	if !strings.Contains(out, "1 groups") || !strings.Contains(out, "300 bytes") {
		t.Errorf("summary:\n%s\nwant 1 group and 300 bytes", out)
	}

	// -dump - : the lines on stdout, the summary on stderr.
	st, out, e = capture(t, store, "fs", "dedupe", "-fs", "t", "-dry-run", "-min-size", "100", "-current", "-dump", "-")
	if st != 0 {
		t.Fatalf("status %d: %s", st, e)
	}
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 2 || strings.Contains(out, "groups") {
		t.Errorf("stdout:\n%s\nwant 2 JSON lines only", out)
	}
	if !strings.Contains(e, "1 groups") {
		t.Errorf("stderr:\n%s\nwant the summary", e)
	}

	// -json: the report.
	st, out, e = capture(t, store, "fs", "dedupe", "-fs", "t", "-dry-run", "-min-size", "100", "-json")
	var rep struct {
		Files   int
		Current struct{ Groups int }
	}
	if st != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.Files != 2 || rep.Current.Groups != 1 {
		t.Errorf("-json: status %d, %q, %s", st, out, e)
	}

	// Without -dry-run: b (or a) is replaced; a rerun skips it, a stub.
	st, out, e = capture(t, store, "fs", "dedupe", "-fs", "t", "-min-size", "100")
	if st != 0 || !strings.Contains(out, "replaced 1 versions (300 bytes)") || !strings.Contains(out, "skipped 0") {
		t.Errorf("a writing run: status %d, %q, %s; want 1 version of 300 bytes replaced", st, out, e)
	}
	st, out, e = capture(t, store, "fs", "dedupe", "-fs", "t", "-min-size", "100")
	if st != 0 || !strings.Contains(out, "replaced 0 versions") || !strings.Contains(out, "skipped 1") || !strings.Contains(out, "/b: stub") {
		t.Errorf("a rerun: status %d, %q, %s; want /b skipped as a stub", st, out, e)
	}
	// -history: c's past version is a's content; it becomes a stub.
	key, _, etag, err := f.Create(ctx, f.RootKey(), "c", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := f.WriteFile(ctx, key, []byte(strings.Repeat("a", 300)), etag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteFile(ctx, key, []byte(strings.Repeat("z", 300)), obj.ETag); err != nil {
		t.Fatal(err)
	}
	st, out, e = capture(t, store, "fs", "dedupe", "-fs", "t", "-min-size", "100", "-history")
	if st != 0 || !strings.Contains(out, "replaced 1 versions (300 bytes)") || !strings.Contains(out, "re-pointed 0 stubs") {
		t.Errorf("-history: status %d, %q, %s; want 1 version of 300 bytes replaced", st, out, e)
	}
	if st, _, e = capture(t, store, "fs", "dedupe", "-dry-run"); st == 0 || !strings.Contains(e, "-fs") {
		t.Errorf("no -fs: status %d, %q", st, e)
	}
	if st, _, _ = capture(t, store, "fs", "dedupe", "-fs", "t", "-dry-run", "-current", "-history"); st == 0 {
		t.Error("-current -history: no error")
	}
}
