package sidecmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
)

// TestFsCheckRepair: fs check reports a dead retired holder and leaves it;
// fs check with repair erases it, and a check after finds nothing.
func TestFsCheckRepair(t *testing.T) {
	store := miniotest.Store(t)
	ctx := context.Background()
	env := Env{Store: store}
	if _, err := Run(ctx, env, Request{Name: "fs create", Args: map[string]string{"fs": "t"}}); err != nil {
		t.Fatal(err)
	}
	f := icbfs.New(store, "t")
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) string {
		t.Helper()
		key, _, etag, err := f.Create(ctx, f.RootKey(), name, 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteFile(ctx, key, []byte(data), etag); err != nil {
			t.Fatal(err)
		}
		return key
	}
	holder := write("holder", strings.Repeat("held ", 100))
	hv, err := store.Head(ctx, holder)
	if err != nil {
		t.Fatal(err)
	}
	z := write("z", "z")
	stub, err := f.PutRef(ctx, z, holder, hv.VersionID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Purge(ctx, f.RootKey(), "holder", false); err != nil {
		t.Fatal(err)
	}
	// z's stub goes underneath: the holder has no live referrer.
	if _, err := f.WriteFile(ctx, z, []byte("z again"), ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Erase(ctx, z, stub.VersionID); err != nil {
		t.Fatal(err)
	}

	check := func(args map[string]string) icbfs.CheckReport {
		t.Helper()
		out, err := Run(ctx, env, Request{Name: "fs check", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		var r icbfs.CheckReport
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	found := func(r icbfs.CheckReport, kind string) (n, repaired int) {
		for _, p := range r.Problems {
			if p.Kind == kind {
				n++
				if p.Repaired {
					repaired++
				}
			}
		}
		return n, repaired
	}
	r := check(map[string]string{"fs": "t"})
	if n, rep := found(r, "ref-holder-dead"); n != 1 || rep != 0 {
		t.Fatalf("fs check: ref-holder-dead %d (%d repaired); want 1, unrepaired: %+v", n, rep, r.Problems)
	}
	r = check(map[string]string{"fs": "t", "repair": "true"})
	if n, rep := found(r, "ref-holder-dead"); n != 1 || rep != 1 {
		t.Fatalf("fs check -repair: ref-holder-dead %d (%d repaired); want 1, repaired: %+v", n, rep, r.Problems)
	}
	if r = check(map[string]string{"fs": "t"}); !r.OK() {
		t.Fatalf("after repair: %+v", r.Problems)
	}
}

// dedupeFS is a filesystem "t" with two twins of 300 bytes and a small
// file, made through fs create.
func dedupeFS(t *testing.T) (Env, *icbfs.Filesystem) {
	t.Helper()
	store := miniotest.Store(t)
	ctx := context.Background()
	env := Env{Store: store}
	if _, err := Run(ctx, env, Request{Name: "fs create", Args: map[string]string{"fs": "t"}}); err != nil {
		t.Fatal(err)
	}
	f := icbfs.New(store, "t")
	if err := f.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"a": strings.Repeat("a", 300), "b": strings.Repeat("a", 300), "small": "s"} {
		key, _, etag, err := f.Create(ctx, f.RootKey(), name, 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteFile(ctx, key, []byte(data), etag); err != nil {
			t.Fatal(err)
		}
	}
	return env, f
}

// TestFsDedupe: fs dedupe returns the dry run's report, with the records
// only when asked for the dump; without -dry-run it replaces the
// duplicates and reports what it wrote; it refuses contradictory flags.
func TestFsDedupe(t *testing.T) {
	env, _ := dedupeFS(t)
	ctx := context.Background()
	run := func(args map[string]string) (Deduped, error) {
		t.Helper()
		if _, ok := args["fs"]; !ok {
			args["fs"] = "t"
		}
		out, err := Run(ctx, env, Request{Name: "fs dedupe", Args: args})
		if err != nil {
			return Deduped{}, err
		}
		var d Deduped
		if err := json.Unmarshal(out, &d); err != nil {
			t.Fatal(err)
		}
		return d, nil
	}

	d, err := run(map[string]string{"dry-run": "true", "min-size": "100"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Report.Files != 3 || d.Report.Current.Groups != 1 || d.Report.Current.Duplicates != 1 || d.Report.Current.Reclaimable != 300 || d.Report.History == nil || len(d.Records) != 0 {
		t.Errorf("dry run: %+v, %d records; want 3 files, 1 group, 300 bytes, history numbers, no records", d.Report, len(d.Records))
	}

	d, err = run(map[string]string{"dry-run": "true", "min-size": "100", "dump": "true", "current": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Records) != 2 || d.Report.History != nil {
		t.Errorf("dump -current: %d records, history %+v; want 2, none", len(d.Records), d.Report.History)
	}

	// The default minimum is 2 MiB: nothing here reaches it.
	d, err = run(map[string]string{"dry-run": "true"})
	if err != nil || d.Report.Current.Groups != 0 || d.Report.Current.BelowMin.Versions != 3 {
		t.Errorf("default minimum: %+v, %v; want 3 below it", d.Report, err)
	}

	for name, args := range map[string]map[string]string{
		"-current -history":    {"dry-run": "true", "current": "true", "history": "true"},
		"a bad minimum":        {"dry-run": "true", "min-size": "lots"},
		"a path that isn't":    {"dry-run": "true", "path": "/nothing"},
		"a missing filesystem": {"dry-run": "true", "fs": ""},
	} {
		if _, err := run(args); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if d, err := run(map[string]string{"dry-run": "true", "min-size": "100"}); err != nil || d.Report.Writes != nil {
		t.Errorf("a dry run reports writes: %+v, %v", d.Report.Writes, err)
	}

	// Without -dry-run: the twin is replaced; with -dump, the records too.
	d, err = run(map[string]string{"min-size": "100", "dump": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if w := d.Report.Writes; w == nil || w.Replaced.Versions != 1 || w.Replaced.Bytes != 300 || len(w.Skipped) != 0 || len(d.Records) == 0 {
		t.Errorf("a writing run: %+v, %d records; want 1 version of 300 bytes replaced", w, len(d.Records))
	}
	d, err = run(map[string]string{"min-size": "100"})
	if w := d.Report.Writes; err != nil || w == nil || w.Replaced.Versions != 0 || len(w.Skipped) != 1 || w.Skipped[0].Reason != "stub" {
		t.Errorf("a rerun: %+v, %v; want nothing replaced, the stub skipped", w, err)
	}
}

// TestFsDedupeHistory: -history without -dry-run rewrites histories: a
// duplicate in a file's past becomes a stub; a rerun replaces nothing.
func TestFsDedupeHistory(t *testing.T) {
	env, f := dedupeFS(t)
	ctx := context.Background()
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
	run := func() Deduped {
		t.Helper()
		out, err := Run(ctx, env, Request{Name: "fs dedupe", Args: map[string]string{"fs": "t", "history": "true", "min-size": "100"}})
		if err != nil {
			t.Fatal(err)
		}
		var d Deduped
		if err := json.Unmarshal(out, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	// a, b and c's past version are one content: two of them become stubs.
	if w := run().Report.Writes; w == nil || w.Replaced.Versions != 2 || w.Replaced.Bytes != 600 || len(w.Skipped) != 0 {
		t.Errorf("-history: %+v; want 2 versions of 300 bytes replaced", w)
	}
	if w := run().Report.Writes; w == nil || w.Replaced.Versions != 0 || w.Repointed != 0 {
		t.Errorf("a rerun: %+v; want nothing", w)
	}
}
