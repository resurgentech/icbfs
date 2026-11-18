package icbfsdrive

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

var _ drive.KeyVersioner = (*Drive)(nil)

// withoutNames is vs with no version's Names.
func withoutNames(vs []drive.Version) []drive.Version {
	out := make([]drive.Version, len(vs))
	for i, v := range vs {
		v.Names = nil
		out[i] = v
	}
	return out
}

// walkFiles is every file item a walk of d gives, as ctx sees them.
func walkFiles(t *testing.T, ctx context.Context, d drive.Drive) []drive.Item {
	t.Helper()
	var files []drive.Item
	err := drive.Walk(ctx, d, drive.ByPath("/"), drive.WalkOptions{}, func(_ drive.ItemRef, its []drive.Item) error {
		for _, it := range its {
			if it.Type == drive.TypeFile {
				files = append(files, it)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestVersionsOfAsVersions (interloc T82-9d): VersionsOf gives what
// Versions gives for every file of a mirror (several versions, a version
// with no checksum recorded, stubs, another source's file, the drive's
// own), less the Names, which Versions has here; for a file deleted
// since the walk, the trash's; ErrIsDir for a folder, ErrNotFound for a
// key that names nothing.
func TestVersionsOfAsVersions(t *testing.T) {
	_, d, _ := rebuildTree(t, "versions-of")
	ctx := system()
	if _, err := d.CreateName(ctx, "checked", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	files := walkFiles(t, ctx, d)
	var named, several, unsummed, stubs int
	for _, it := range files {
		want, err := d.Versions(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})
		if err != nil {
			t.Fatalf("versions of %s: %v", it.Path, err)
		}
		got, err := d.VersionsOf(ctx, it)
		if err != nil {
			t.Fatalf("versions of %s by key: %v", it.Path, err)
		}
		if !reflect.DeepEqual(got, withoutNames(want)) {
			t.Fatalf("%s by key %+v\nwant %+v", it.Path, got, withoutNames(want))
		}
		for _, v := range want {
			if len(v.Names) > 0 {
				named++
			}
			if v.Checksums == nil || v.Checksums.SHA256 == "" {
				unsummed++
			}
		}
		if len(want) > 1 {
			several++
		}
		if a, err := d.Filesystem().Stat(ctx, it.ID, icbfs.TypeFile); err == nil && len(a.Refs) > 0 {
			stubs++
		}
	}
	if len(files) < 20 || named == 0 || several == 0 || unsummed == 0 || stubs == 0 {
		t.Fatalf("%d files, %d versions named, %d files with several versions, %d versions without a SHA-256, %d stubs: want some of each",
			len(files), named, several, unsummed, stubs)
	}

	dir, err := d.Stat(ctx, drive.ByPath("/one"), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.VersionsOf(ctx, dir); !errors.Is(err, drive.ErrIsDir) {
		t.Fatalf("a folder's versions by key: %v, want ErrIsDir", err)
	}

	// Deleted since the walk: the trash's versions, as Versions gives.
	var c drive.Item
	for _, it := range files {
		if it.Path == "/one/two/c.txt" {
			c = it
		}
	}
	if err := d.Delete(ctx, drive.ItemRef{ID: c.ID, Path: c.Path}, false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	want, err := d.Versions(ctx, drive.ItemRef{ID: c.ID, Path: c.Path})
	if err != nil || len(want) != 2 {
		t.Fatalf("a deleted file's versions: %+v %v, want its two", want, err)
	}
	if got, err := d.VersionsOf(ctx, c); err != nil || !reflect.DeepEqual(got, withoutNames(want)) {
		t.Fatalf("a deleted file's versions by key: %+v %v\nwant %+v", got, err, withoutNames(want))
	}

	gone := drive.Item{ID: "no-such-key", Path: "/nowhere.txt", Type: drive.TypeFile}
	if _, err := d.VersionsOf(ctx, gone); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a key that names nothing: %v, want ErrNotFound", err)
	}
}

// rebuildBefore is the content map RebuildHashes built before interloc
// T82-9d, built one file at a time: a walk of d, and for each of
// source's files its Versions (by path and id), each version with a
// SHA-256 added. The order versions are added in doesn't change the map
// (Add keeps the least).
func rebuildBefore(ctx context.Context, d drive.Drive, source string) (*drive.HashMap, error) {
	h := drive.NewHashMap()
	err := drive.Walk(ctx, d, drive.ByPath("/"), drive.WalkOptions{}, func(_ drive.ItemRef, its []drive.Item) error {
		for _, it := range its {
			if it.Type != drive.TypeFile || it.Origin == nil || it.Origin.Drive != source {
				continue
			}
			vs, err := d.Versions(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})
			if errors.Is(err, drive.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			for _, v := range vs {
				if v.Checksums != nil && v.Checksums.SHA256 != "" {
					h.Add(v.Checksums.SHA256, drive.HashEntry{ID: it.ID, Version: v.ID, Size: v.Size, Path: it.Path})
				}
			}
		}
		return nil
	})
	return h, err
}

// TestRebuildHashesAsBefore (interloc T82-9d): the map RebuildHashes
// builds, reading by key 8 at once across folders, is the one it built
// before, a file's Versions at a time (rebuildBefore), row for row, on a
// mirror with several versions of a file, a version with no SHA-256
// recorded, stubs, another source's file and the drive's own.
func TestRebuildHashesAsBefore(t *testing.T) {
	_, d, _ := rebuildTree(t, "rebuild-as-before")
	ctx := system()
	want, err := rebuildBefore(ctx, d, srcDrive)
	if err != nil {
		t.Fatal(err)
	}
	m := drive.NewMirror(d, srcDrive)
	if err := m.RebuildHashes(ctx); err != nil {
		t.Fatal(err)
	}
	if got, w := m.Hashes().Entries(), want.Entries(); len(w) != 13 || !reflect.DeepEqual(got, w) {
		t.Fatalf("rebuilt %+v\nbefore %+v (want 13 rows)", got, w)
	}
	gotJSON, err := m.Hashes().MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := want.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("rebuilt %s\nbefore %s", gotJSON, wantJSON)
	}
}

// TestVersionsOfRequests (interloc T82-9d): for a profile that may read
// anything, VersionsOf of a file three folders down reads only what is
// the file's: its history, its dates and its attributes (.metadata and
// a HEAD), four requests, none of them finding it by its path; Versions
// takes more.
func TestVersionsOfRequests(t *testing.T) {
	store := &requestCounter{Store: miniotest.Store(t), n: map[string]int{}}
	d := newDrive(t, store, icbfs.Settings{Name: "versions-of-requests", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	write(t, ctx, d, "/a/b/c/f.txt", "one", drive.Condition{})
	it := write(t, ctx, d, "/a/b/c/f.txt", "two", drive.Condition{})
	want, err := d.Versions(ctx, drive.ItemRef{ID: it.ID, Path: it.Path})
	if err != nil {
		t.Fatal(err)
	}
	store.take()
	got, err := d.VersionsOf(ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	n, by := store.take()
	if !reflect.DeepEqual(got, withoutNames(want)) || len(got) != 2 {
		t.Fatalf("by key %+v\nwant %+v", got, withoutNames(want))
	}
	wantBy := "\n  GET .metadata      1\n  GET .version       1\n  HEAD content       1\n  HISTORY content    1"
	if n != 4 || by != wantBy {
		t.Fatalf("VersionsOf took %d requests:%s\nwant 4:%s", n, by, wantBy)
	}
	if _, err := d.Versions(ctx, drive.ItemRef{ID: it.ID, Path: it.Path}); err != nil {
		t.Fatal(err)
	}
	vn, vby := store.take()
	t.Logf("VersionsOf: %d requests; Versions: %d:%s", n, vn, vby)
	if vn <= n {
		t.Fatalf("Versions took %d requests, no more than VersionsOf's %d", vn, n)
	}
}

// TestVersionsOfChecksProfile (interloc T82-9d): for a profile that may
// not read anything, VersionsOf checks as Versions does: refused on a
// file it may not read, the versions less the Names on one it may.
func TestVersionsOfChecksProfile(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "versions-of-perm", Size: 1 << 30, RootMode: 0o777}, "")
	alice, bob := as(1000, 1000), as(1001, 1001)
	private := uint32(0o600)
	secret, err := d.Write(alice, "/docs/secret.txt", strings.NewReader("secret"), drive.Condition{}, drive.WriteOptions{Mode: &private, Parents: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Versions(bob, drive.ItemRef{ID: secret.ID, Path: secret.Path}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("bob's Versions of alice's private file: %v, want ErrForbidden", err)
	}
	if _, err := d.VersionsOf(bob, secret); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("bob's VersionsOf alice's private file: %v, want ErrForbidden", err)
	}
	if _, err := d.VersionsOf(context.Background(), secret); !errors.Is(err, drive.ErrNoAccess) {
		t.Fatalf("VersionsOf with no profile: %v, want ErrNoAccess", err)
	}
	want, err := d.Versions(alice, drive.ItemRef{ID: secret.ID, Path: secret.Path})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.VersionsOf(alice, secret); err != nil || !reflect.DeepEqual(got, withoutNames(want)) {
		t.Fatalf("alice's VersionsOf her file: %+v %v\nwant %+v", got, err, withoutNames(want))
	}
}
