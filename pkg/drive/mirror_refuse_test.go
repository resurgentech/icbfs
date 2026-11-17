package drive_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// listingDrive is a drive that can't find by origin: an empty drive and
// trash that count how often they are listed. Any other call panics.
type listingDrive struct {
	drive.Drive
	lists atomic.Int64
}

func (l *listingDrive) List(ctx context.Context, dir drive.ItemRef, opts drive.ListOptions) (drive.Page, error) {
	l.lists.Add(1)
	return drive.Page{}, nil
}

func (l *listingDrive) Trash(ctx context.Context, opts drive.TrashOptions) (drive.TrashPage, error) {
	l.lists.Add(1)
	return drive.TrashPage{}, nil
}

// finderDrive is listingDrive with a lookup by origin: it holds nothing.
type finderDrive struct{ *listingDrive }

func (f finderDrive) FindByOrigin(ctx context.Context, originDrive, originID string) (drive.Item, *drive.TrashEntry, error) {
	return drive.Item{}, nil, drive.ErrNotFound
}

// noPanic is call's error, or, if it panicked (it called what
// listingDrive doesn't have), that as an error.
func noPanic(call func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("called the drive: %v", r)
		}
	}()
	return call()
}

// TestMirrorRefusesDriveWithoutFindByOrigin (2026-11-17): a mirror's
// destination must find by origin. One that can't is refused when the
// mirror is made (Err), in words that name the interface, and every call
// after gives that refusal without touching the drive: no listing of the
// whole drive per lookup, which grows with the square of its size.
func TestMirrorRefusesDriveWithoutFindByOrigin(t *testing.T) {
	ctx := drive.WithProfile(context.Background(), drive.Profile{System: true})
	plain := &listingDrive{}
	m := drive.NewMirror(plain, "src")
	err := m.Err()
	if !errors.Is(err, drive.ErrNoOriginFinder) {
		t.Errorf("NewMirror on a drive without FindByOrigin: Err() = %v, want ErrNoOriginFinder", err)
	}
	for _, words := range []string{"drive.OriginFinder", "FindByOrigin", "destination"} {
		if err == nil || !strings.Contains(err.Error(), words) {
			t.Errorf("the refusal %q doesn't say %q", err, words)
		}
	}
	calls := map[string]func() error{
		"Known": func() error { _, _, _, err := m.Known(ctx, "a"); return err },
		"Apply": func() error {
			_, err := m.Apply(ctx, drive.SourceItem{Origin: drive.Origin{ID: "a"}, Path: "/a", Type: drive.TypeDir})
			return err
		},
		"Remove":        func() error { _, err := m.Remove(ctx, "a"); return err },
		"Undo":          func() error { _, err := m.Undo(ctx, "pass"); return err },
		"RebuildHashes": func() error { return m.RebuildHashes(ctx) },
		"LoadHashes":    func() error { return m.LoadHashes(ctx) },
		"SaveHashes":    func() error { return m.SaveHashes(ctx) },
	}
	for name, call := range calls {
		if err := noPanic(call); !errors.Is(err, drive.ErrNoOriginFinder) {
			t.Errorf("%s on the refused mirror: %v, want ErrNoOriginFinder", name, err)
		}
	}
	if n := plain.lists.Load(); n != 0 {
		t.Errorf("the refused mirror listed the drive or its trash %d times, want never", n)
	}

	// A drive that finds by origin is taken, and a lookup is one call.
	l := &listingDrive{}
	fm := drive.NewMirror(finderDrive{l}, "src")
	if err := fm.Err(); err != nil {
		t.Fatalf("NewMirror on an OriginFinder: Err() = %v", err)
	}
	if _, _, found, err := fm.Known(ctx, "a"); err != nil || found {
		t.Fatalf("Known on an empty OriginFinder: found %v, %v", found, err)
	}
	if n := l.lists.Load(); n != 0 {
		t.Errorf("Known on an OriginFinder listed the drive %d times, want never", n)
	}
}
