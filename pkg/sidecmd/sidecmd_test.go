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
