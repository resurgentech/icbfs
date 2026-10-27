package icbfsdrive

import (
	"errors"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// TestTrustFor: a drive that trusts the folders it read (TrustFor) sees
// another drive's changes to them only after the window, and its own at
// once; one that doesn't (TrustFor 0) sees another's at once, a change
// to a folder's .metadata alone (its record, here; its permissions
// likewise) as well as to its entries.
func TestTrustFor(t *testing.T) {
	const window = 2 * time.Second
	store := miniotest.Store(t)
	s := icbfs.Settings{Name: "trust", Size: 1 << 30, RootMode: 0o777}
	other := newDrive(t, store, s, "")
	trusting := newDriveWith(t, store, s, Config{TrustFor: window})
	checking := newDrive(t, store, s, "")
	ctx := system()
	write(t, ctx, other, "/P/one.txt", "one", drive.Condition{})

	// seen is whether d sees two.txt in /P, and /P's tag.
	seen := func(d *Drive) (bool, string) {
		t.Helper()
		_, err := d.Stat(ctx, drive.ByPath("/P/two.txt"), drive.At{})
		if err != nil && !errors.Is(err, drive.ErrNotFound) {
			t.Fatal(err)
		}
		p, perr := d.Stat(ctx, drive.ByPath("/P"), drive.At{})
		if perr != nil {
			t.Fatal(perr)
		}
		return err == nil, string(p.App["test.tag"])
	}
	for _, d := range []*Drive{trusting, checking} {
		if two, tag := seen(d); two || tag != "" {
			t.Fatalf("before: two.txt %v, tag %q", two, tag)
		}
	}
	start := time.Now()
	write(t, ctx, other, "/P/two.txt", "two", drive.Condition{})
	if _, err := other.SetInfo(ctx, drive.ByPath("/P"), drive.ItemInfo{App: map[string][]byte{"test.tag": []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if two, tag := seen(checking); !two || tag != "x" {
		t.Fatalf("checking drive: two.txt %v, tag %q; want both at once", two, tag)
	}
	if two, tag := seen(trusting); (two || tag != "") && time.Since(start) < window {
		t.Fatalf("trusting drive, within its window: two.txt %v, tag %q; want neither yet", two, tag)
	}
	time.Sleep(window - time.Since(start) + 100*time.Millisecond)
	if two, tag := seen(trusting); !two || tag != "x" {
		t.Fatalf("trusting drive, after its window: two.txt %v, tag %q; want both", two, tag)
	}

	// Its own writes it sees at once, in a window just begun.
	write(t, ctx, trusting, "/P/three.txt", "three", drive.Condition{})
	if _, err := trusting.Stat(ctx, drive.ByPath("/P/three.txt"), drive.At{}); err != nil {
		t.Fatalf("its own new file: %v", err)
	}
	if _, err := trusting.SetInfo(ctx, drive.ByPath("/P"), drive.ItemInfo{App: map[string][]byte{"test.tag": []byte("y")}}); err != nil {
		t.Fatal(err)
	}
	if _, tag := seen(trusting); tag != "y" {
		t.Fatalf("its own record: tag %q, want y", tag)
	}
}
