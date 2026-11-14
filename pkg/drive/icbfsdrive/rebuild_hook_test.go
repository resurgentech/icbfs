package icbfsdrive

import (
	"context"
	"errors"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// versionsSpy is a drive (and a Walker) whose Versions waits a little,
// fails for the paths in fail, and records how many calls are in flight
// at once, and in how many folders.
type versionsSpy struct {
	*Drive
	fail map[string]error

	mu            sync.Mutex
	inFlight      map[string]int // by folder
	most, folders int            // the most calls at once, and folders at once
	calls         int
}

func (s *versionsSpy) Versions(ctx context.Context, ref drive.ItemRef) ([]drive.Version, error) {
	dir := path.Dir(ref.Path)
	s.mu.Lock()
	s.calls++
	s.inFlight[dir]++
	n := 0
	for _, c := range s.inFlight {
		n += c
	}
	s.most, s.folders = max(s.most, n), max(s.folders, len(s.inFlight))
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.inFlight[dir]--; s.inFlight[dir] == 0 {
			delete(s.inFlight, dir)
		}
		s.mu.Unlock()
	}()
	time.Sleep(20 * time.Millisecond)
	if err := s.fail[ref.Path]; err != nil {
		return nil, err
	}
	return s.Drive.Versions(ctx, ref)
}

// TestRebuildHashesProgress: RebuildHashes asks for 8 files' versions at
// once, in one folder at a time; its progress hook sees the items and
// files go up to all of them, the last call saying all; a file gone
// since the walk is skipped, and another error is the file's, with the
// map left as it was.
func TestRebuildHashesProgress(t *testing.T) {
	_, d, files := rebuildTree(t, "rebuild-progress")
	ctx := system()
	items := len(listAll(t, ctx, d, drive.ByPath("/"), drive.ListOptions{Depth: 1 << 30}))

	spy := &versionsSpy{Drive: d, inFlight: map[string]int{}, fail: map[string]error{"/one/b.txt": drive.ErrNotFound}}
	want, _, err := listRebuild(ctx, spy, srcDrive, nil)
	if err != nil {
		t.Fatal(err)
	}
	spy.calls, spy.most, spy.folders = 0, 0, 0
	type call struct{ items, files int }
	var calls []call
	fresh := drive.NewMirror(spy, srcDrive).OnRebuildProgress(func(items, files int) { calls = append(calls, call{items, files}) })
	if err := fresh.RebuildHashes(ctx); err != nil {
		t.Fatal(err)
	}
	if spy.most != 8 || spy.folders != 1 || spy.calls != files {
		t.Fatalf("%d calls, at most %d at once, in %d folders at once; want %d, 8, in 1", spy.calls, spy.most, spy.folders, files)
	}
	// b gone since the walk: skipped, as the listing skipped it.
	if got := fresh.Hashes().Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt without b %+v\nlisted %+v", got, want)
	}
	if len(calls) < 6 {
		t.Fatalf("progress %+v: want a call a folder", calls)
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].items < calls[i-1].items || calls[i].files < calls[i-1].files {
			t.Fatalf("progress went back: %+v", calls)
		}
	}
	if last := calls[len(calls)-1]; last != (call{items, files}) {
		t.Fatalf("the last progress %+v, want %d items and %d files", last, items, files)
	}

	// Another error: the file's, and the map as it was.
	boom := errors.New("boom")
	spy.fail = map[string]error{"/wide/w03.txt": boom, "/wide/w11.txt": boom}
	err = drive.NewMirror(spy, srcDrive).RebuildHashes(ctx)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "versions of /wide/w03.txt") {
		t.Fatalf("a failing Versions: %v, want w03's boom", err)
	}
	m2 := drive.NewMirror(spy, srcDrive)
	m2.Hashes().Add(sha256Of("kept"), drive.HashEntry{ID: "k", Version: "1", Size: 4})
	kept := m2.Hashes().Entries()
	spy.fail = map[string]error{"/one/two/c.txt": boom}
	if err := m2.RebuildHashes(ctx); !errors.Is(err, boom) {
		t.Fatalf("a failing Versions: %v, want boom", err)
	}
	if got := m2.Hashes().Entries(); !reflect.DeepEqual(got, kept) {
		t.Fatalf("after a failed rebuild the map is %+v, want %+v", got, kept)
	}
}

// TestRebuildHashesCancelled: cancelled part way, RebuildHashes stops
// between items with the context's cause, the map left as it was; one
// cancelled before it starts asks for no versions.
func TestRebuildHashesCancelled(t *testing.T) {
	_, d, files := rebuildTree(t, "rebuild-cancel")
	spy := &versionsSpy{Drive: d, inFlight: map[string]int{}}
	m := drive.NewMirror(spy, srcDrive)
	m.Hashes().Add(sha256Of("kept"), drive.HashEntry{ID: "k", Version: "1", Size: 4})
	kept := m.Hashes().Entries()
	cause := errors.New("the daemon is stopping")
	ctx, cancel := context.WithCancelCause(system())
	defer cancel(nil)
	n := 0
	m.OnRebuildProgress(func(items, files int) {
		if n++; n == 2 {
			cancel(cause)
		}
	})
	err := m.RebuildHashes(ctx)
	if !errors.Is(err, cause) {
		t.Fatalf("cancelled: %v, want the cause", err)
	}
	if got := m.Hashes().Entries(); !reflect.DeepEqual(got, kept) {
		t.Fatalf("after a cancelled rebuild the map is %+v, want %+v", got, kept)
	}
	if n != 2 || spy.calls >= files {
		t.Fatalf("went on after the cancel: %d progress calls, %d of %d files read", n, spy.calls, files)
	}

	ctx, cancel = context.WithCancelCause(system())
	cancel(cause)
	spy.calls = 0
	if err := m.RebuildHashes(ctx); !errors.Is(err, cause) || spy.calls != 0 {
		t.Fatalf("cancelled before: %v after %d files, want the cause after none", err, spy.calls)
	}
}
