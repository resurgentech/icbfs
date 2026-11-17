package drive_test

import (
	"context"
	"testing"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// hintDrive finds by origin and takes a hint: it records how each lookup
// came, and holds nothing.
type hintDrive struct {
	*listingDrive
	plain  int
	hinted []string // the hints given, in order
}

func (h *hintDrive) FindByOrigin(ctx context.Context, originDrive, originID string) (drive.Item, *drive.TrashEntry, error) {
	h.plain++
	return drive.Item{}, nil, drive.ErrNotFound
}

func (h *hintDrive) FindByOriginHint(ctx context.Context, originDrive, originID, hint string) (drive.Item, *drive.TrashEntry, error) {
	h.hinted = append(h.hinted, originID+"@"+hint)
	return drive.Item{}, nil, drive.ErrNotFound
}

// TestMirrorHints (T82-13): a mirror given hints (WithHints) asks its
// function where each source item is and passes the answer to a drive
// that takes one (HintedOriginFinder); with no answer ("") the lookup is
// FindByOrigin's, as it is on a mirror without hints and on a drive that
// takes none (whose mirror never asks the function).
func TestMirrorHints(t *testing.T) {
	ctx := drive.WithProfile(context.Background(), drive.Profile{System: true})
	where := map[string]string{"a": "/x/a", "b": ""}
	asked := 0
	hints := func(_ context.Context, id string) string { asked++; return where[id] }

	h := &hintDrive{listingDrive: &listingDrive{}}
	m := drive.NewMirror(h, "src").WithHints(hints)
	for _, id := range []string{"a", "b", "c"} {
		if _, _, found, err := m.Known(ctx, id); err != nil || found {
			t.Fatalf("Known(%s): found %v, %v", id, found, err)
		}
	}
	if len(h.hinted) != 1 || h.hinted[0] != "a@/x/a" || h.plain != 2 {
		t.Errorf("with hints: hinted %v, plain %d; want [a@/x/a] and 2", h.hinted, h.plain)
	}
	if _, err := m.Remove(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if len(h.hinted) != 2 {
		t.Errorf("Remove's lookup took no hint: %v", h.hinted)
	}

	// Without hints: every lookup is FindByOrigin's.
	h2 := &hintDrive{listingDrive: &listingDrive{}}
	if _, _, _, err := drive.NewMirror(h2, "src").Known(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if len(h2.hinted) != 0 || h2.plain != 1 {
		t.Errorf("no hints: hinted %v, plain %d", h2.hinted, h2.plain)
	}

	// A drive that takes no hint: the function isn't asked.
	asked = 0
	fm := drive.NewMirror(finderDrive{&listingDrive{}}, "src").WithHints(hints)
	if _, _, _, err := fm.Known(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if asked != 0 {
		t.Errorf("a drive with no FindByOriginHint: the hint function was asked %d times", asked)
	}
}
