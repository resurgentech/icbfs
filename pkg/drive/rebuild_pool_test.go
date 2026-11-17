package drive_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/pkg/drive"
)

// poolDrive is a drive of folders folders of perFolder of the source's
// files each, with no store: a Walker whose Versions waits a little and
// records how many calls are in flight at once. Nothing else of a Drive
// is there: a rebuild that calls it panics.
type poolDrive struct {
	drive.Drive
	folders, perFolder int

	mu                     sync.Mutex
	inFlight, peak         int
	versionsCalls, ofCalls int
}

func (p *poolDrive) Walk(ctx context.Context, dir drive.ItemRef, opts drive.WalkOptions, fn drive.WalkFunc) error {
	top := make([]drive.Item, p.folders)
	for i := range top {
		top[i] = drive.Item{ID: fmt.Sprintf("dir%04d", i), Name: fmt.Sprintf("f%04d", i), Path: fmt.Sprintf("/f%04d", i), Type: drive.TypeDir}
	}
	if err := fn(drive.ItemRef{ID: "root", Path: "/"}, top); err != nil {
		return err
	}
	for i := range p.folders {
		its := make([]drive.Item, p.perFolder)
		for k := range its {
			name := fmt.Sprintf("file%02d", k)
			its[k] = drive.Item{ID: fmt.Sprintf("key%04d-%02d", i, k), Name: name, Path: top[i].Path + "/" + name, Type: drive.TypeFile,
				Origin: &drive.Origin{Drive: "src", ID: name}}
		}
		if err := fn(drive.ItemRef{ID: top[i].ID, Path: top[i].Path}, its); err != nil {
			return err
		}
	}
	return nil
}

// FindByOrigin makes poolDrive a mirror's destination (an OriginFinder):
// a lookup in its fixed layout, which no call changes. Every folder holds
// a copy of the source's file fileKK; the first folder's is the one found,
// as a listing of the drive in path order would find it first.
func (p *poolDrive) FindByOrigin(ctx context.Context, originDrive, originID string) (drive.Item, *drive.TrashEntry, error) {
	var k int
	if _, err := fmt.Sscanf(originID, "file%02d", &k); err == nil && originDrive == "src" && p.folders > 0 && k >= 0 && k < p.perFolder && originID == fmt.Sprintf("file%02d", k) {
		return drive.Item{ID: fmt.Sprintf("key%04d-%02d", 0, k), Name: originID, Path: "/f0000/" + originID, Type: drive.TypeFile,
			Origin: &drive.Origin{Drive: "src", ID: originID}}, nil, nil
	}
	return drive.Item{}, nil, fmt.Errorf("source item %s: %w", originID, drive.ErrNotFound)
}

// read is a file's one version, after a wait, its content's SHA-256
// that of its key.
func (p *poolDrive) read(id string, of bool) []drive.Version {
	p.mu.Lock()
	if of {
		p.ofCalls++
	} else {
		p.versionsCalls++
	}
	p.inFlight++
	p.peak = max(p.peak, p.inFlight)
	p.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(id))
	return []drive.Version{{ID: "v1", Size: 1, Current: true, Checksums: &drive.Checksums{SHA256: hex.EncodeToString(sum[:])}}}
}

func (p *poolDrive) Versions(ctx context.Context, ref drive.ItemRef) ([]drive.Version, error) {
	return p.read(ref.ID, false), nil
}

// keyPoolDrive is a poolDrive that is also a KeyVersioner.
type keyPoolDrive struct{ *poolDrive }

func (k keyPoolDrive) VersionsOf(ctx context.Context, it drive.Item) ([]drive.Version, error) {
	return k.read(it.ID, true), nil
}

// TestRebuildHashesPoolAcrossFolders (interloc T82-9d): RebuildHashes
// keeps 8 files' versions in flight however few files a folder has (a
// folder doesn't wait for the one before it to finish), never more; on a
// KeyVersioner it reads them by key (VersionsOf), never by Versions; on
// a drive that isn't one, by Versions, 8 at once too.
func TestRebuildHashesPoolAcrossFolders(t *testing.T) {
	ctx := drive.WithProfile(context.Background(), drive.Profile{System: true})
	for _, keyed := range []bool{true, false} {
		p := &poolDrive{folders: 200, perFolder: 2}
		var d drive.Drive = p
		if keyed {
			d = keyPoolDrive{p}
		}
		m := drive.NewMirror(d, "src")
		if err := m.RebuildHashes(ctx); err != nil {
			t.Fatalf("keyed %v: %v", keyed, err)
		}
		files := p.folders * p.perFolder
		if n := m.Hashes().Len(); n != files {
			t.Errorf("keyed %v: the map has %d entries, want %d", keyed, n, files)
		}
		if p.peak != 8 {
			t.Errorf("keyed %v: at most %d files' versions in flight at once, want 8", keyed, p.peak)
		}
		wantOf, wantVersions := files, 0
		if !keyed {
			wantOf, wantVersions = 0, files
		}
		if p.ofCalls != wantOf || p.versionsCalls != wantVersions {
			t.Errorf("keyed %v: %d VersionsOf and %d Versions calls, want %d and %d", keyed, p.ofCalls, p.versionsCalls, wantOf, wantVersions)
		}
	}
}
