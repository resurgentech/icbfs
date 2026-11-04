package icbfs

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestReplaceFileHistorySelfRef: a stub state may name a version of the
// file itself when that version is kept, earlier in the new history, and
// not a stub; the stub then names the kept copy's new version id, and the
// file's own .refs lists it. A file whose only referrers are its own
// stubs is rewritten again (its kept self-stubs re-aimed at the new
// copies, the old entries dropped), and purged whole. fsck is clean
// after each.
func TestReplaceFileHistorySelfRef(t *testing.T) {
	ctx := context.Background()
	f, store := newTestFilesystem(t)
	root := f.RootKey()
	A := strings.Repeat("a", 300)
	B := strings.Repeat("b", 200)
	c := writeNew(t, ctx, f, root, "c", A)
	if _, err := f.WriteFile(ctx, c, []byte(B), ""); err != nil {
		t.Fatal(err)
	}
	h, err := store.History(ctx, c)
	if err != nil || len(h) != 3 {
		t.Fatalf("c's history %v, %v", h, err)
	}
	vB, vA := h[0].VersionID, h[1].VersionID
	at := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }

	// Refused: a self-stub naming a version not kept, one kept only
	// later, and (below) one that is a stub.
	for name, states := range map[string][]FileState{
		"not kept":   {{At: at(2020), Version: vB}, {At: at(2021), RefKey: c, RefVersion: vA}},
		"kept later": {{At: at(2020), RefKey: c, RefVersion: vA}, {At: at(2021), Version: vA}},
	} {
		if err := f.ReplaceFileHistory(ctx, c, states, ""); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: %v; want ErrInvalidArgument", name, err)
		}
	}
	if after, _ := store.History(ctx, c); len(after) != 3 {
		t.Fatalf("a refused rewrite changed c: %d versions", len(after))
	}
	// Refused: a self-stub naming a kept version that is a stub (of
	// another file): a stub to a stub.
	d := writeNew(t, ctx, f, root, "d", A)
	dh, err := store.History(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	e := writeNew(t, ctx, f, root, "e", "e's own")
	eStub, err := f.PutRef(ctx, e, d, dh[0].VersionID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ReplaceFileHistory(ctx, e, []FileState{{At: at(2020), Version: eStub.VersionID}, {At: at(2021), RefKey: e, RefVersion: eStub.VersionID}}, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a self-stub naming a kept stub: %v; want ErrInvalidArgument", err)
	}

	if err := f.ReplaceFileHistory(ctx, c, []FileState{
		{At: at(2020), Version: vA},
		{At: at(2021), Version: vB},
		{At: at(2022), RefKey: c, RefVersion: vA},
	}, ""); err != nil {
		t.Fatalf("rewrite with a self-stub: %v", err)
	}
	hist, _, err := f.FileHistory(ctx, c)
	if err != nil || len(hist) != 3 {
		t.Fatalf("c's history %+v, %v", hist, err)
	}
	cur := hist[0]
	if cur.Ref == nil || cur.Ref.Key != c || cur.Ref.VersionID != hist[2].VersionID || cur.Size != int64(len(A)) {
		t.Fatalf("c's current %+v; want a stub naming its 2020 copy %s", cur, hist[2].VersionID)
	}
	if data, _, _, err := f.ReadFile(ctx, c); err != nil || string(data) != A {
		t.Fatalf("c reads %d bytes, %v; want A", len(data), err)
	}
	refs := readRefs(t, ctx, store, c).GetReferrers()
	if len(refs) != 1 || refs[0].ReferrerKey != c || refs[0].ReferrerVersion != cur.VersionID || refs[0].TargetVersion != hist[2].VersionID || refs[0].Pending {
		t.Fatalf("c's .refs %+v; want one finalized entry, its stub naming its copy", refs)
	}
	checkClean(t, f)

	if err := f.ReplaceFileHistory(ctx, c, []FileState{
		{At: at(2020), Version: hist[2].VersionID},
		{At: at(2021), Version: hist[0].VersionID},
		{At: at(2022), RefKey: c, RefVersion: hist[0].VersionID},
	}, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a self-stub naming a stub: %v; want ErrInvalidArgument", err)
	}

	// A later rewrite of the self-referencing file: its own entry isn't a
	// referrer; the kept self-stub names the new copy.
	if err := f.ReplaceFileHistory(ctx, c, []FileState{
		{At: at(2020), Version: hist[2].VersionID},
		{At: at(2022), Version: cur.VersionID},
	}, cur.VersionID); err != nil {
		t.Fatalf("a later rewrite of a self-referencing file: %v", err)
	}
	hist2, _, err := f.FileHistory(ctx, c)
	if err != nil || len(hist2) != 2 {
		t.Fatalf("c's history %+v, %v", hist2, err)
	}
	if r := hist2[0].Ref; r == nil || r.Key != c || r.VersionID != hist2[1].VersionID || !hist2[0].At.Equal(at(2022)) {
		t.Fatalf("c's current %+v; want a stub naming its new 2020 copy %s", hist2[0], hist2[1].VersionID)
	}
	if data, _, _, err := f.ReadFile(ctx, c); err != nil || !bytes.Equal(data, []byte(A)) {
		t.Fatalf("c reads %d bytes, %v; want A", len(data), err)
	}
	refs = readRefs(t, ctx, store, c).GetReferrers()
	if len(refs) != 1 || refs[0].ReferrerVersion != hist2[0].VersionID || refs[0].TargetVersion != hist2[1].VersionID {
		t.Fatalf("c's .refs %+v; want the old entry gone, the new one there", refs)
	}
	checkClean(t, f)

	// A purge erases it whole: nothing else names it.
	rep, err := f.PurgeWithReport(ctx, root, "c", false)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(rep.Retired) != 0 {
		t.Fatalf("purge retired %+v; want c erased whole", rep.Retired)
	}
	left, err := store.HistoryUnder(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("after the purge %d objects of c are left: %+v", len(left), left)
	}
	if r := checkReport(t, f); r.RetiredHolders != 0 {
		t.Fatalf("%d retired holders after the purge", r.RetiredHolders)
	}
}

func checkReport(t *testing.T, f *Filesystem) CheckReport {
	t.Helper()
	r, err := f.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("fsck: %+v", r.Problems)
	}
	return r
}
