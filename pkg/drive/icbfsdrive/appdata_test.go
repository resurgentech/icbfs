package icbfsdrive

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func u32(v uint32) *uint32 { return &v }

// metadataVersions counts an item's .metadata versions.
func metadataVersions(t *testing.T, d *Drive, id string) int {
	t.Helper()
	objs, err := d.fs.Store().History(context.Background(), id+".metadata")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range objs {
		if !o.Deleted {
			n++
		}
	}
	return n
}

// TestWriteWithInfo: a write with app keys, a mode, Windows attributes,
// times and an origin makes exactly one .metadata version holding all of
// them; a bad app key is refused before anything is written.
func TestWriteWithInfo(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "info", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	born, edited := year(2020), year(2021)
	o := drive.Origin{Drive: srcDrive, ID: "doc1", Version: "3"}
	it, err := d.Write(ctx, "/locked.txt", strings.NewReader("locked"), drive.Condition{}, drive.WriteOptions{
		ModTime: edited, Created: born, Origin: &o,
		App:  map[string][]byte{"mirror.contactId": []byte("c1"), "mirror.locked": []byte("1")},
		Mode: u32(0o444), WindowsAttrs: u32(icbfs.WindowsAttrReadOnly),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := metadataVersions(t, d, it.ID); n != 2 { // Create's, then the one write
		t.Fatalf("%d .metadata versions, want 2 (create, then one write)", n)
	}
	if string(it.App["mirror.contactId"]) != "c1" || it.Mode != 0o444 || it.WindowsAttrs == nil || *it.WindowsAttrs != icbfs.WindowsAttrReadOnly ||
		!it.Modified.Equal(edited) || !it.Created.Equal(born) || it.Origin == nil || it.Origin.ID != "doc1" {
		t.Fatalf("item: %+v", it)
	}
	// As a mount sees it: mode 0444 and the ReadOnly bit.
	attr, err := d.fs.Stat(context.Background(), it.ID, icbfs.TypeFile)
	if err != nil || attr.Mode != 0o444 {
		t.Fatalf("mode %o, %v", attr.Mode, err)
	}
	if bits, ok := attr.WindowsAttributes(); !ok || bits&icbfs.WindowsAttrReadOnly == 0 {
		t.Fatalf("Windows attributes %#x, %v", bits, ok)
	}
	// A mount's own chmod still works.
	if _, err := d.fs.SetAttr(context.Background(), it.ID, icbfs.TypeFile, u32(0o644), nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if st, err := d.Stat(ctx, drive.ByID(it.ID), drive.At{}); err != nil || st.Mode != 0o644 {
		t.Fatalf("after chmod: mode %o, %v", st.Mode, err)
	}

	for _, bad := range []string{"nodot", "drive.mine", "Mirror.x", ".x", "x."} {
		_, err := d.Write(ctx, "/bad.txt", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{App: map[string][]byte{bad: []byte("v")}})
		if !errors.Is(err, drive.ErrInvalid) {
			t.Fatalf("app key %q: %v; want ErrInvalid", bad, err)
		}
	}
	if exists(t, d, "/bad.txt", time.Time{}) {
		t.Fatal("a refused write made the file")
	}
}

// TestSetInfo: on an existing item, SetInfo with changes makes one
// .metadata version and no content version; app keys alone leave ctime;
// a view at an older version's time shows that version's keys.
func TestSetInfo(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "setinfo", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	it := write(t, ctx, d, "/doc.txt", "v1", drive.Condition{})
	before, err := d.Stat(ctx, drive.ByID(it.ID), drive.At{})
	if err != nil {
		t.Fatal(err)
	}
	versions := func() int {
		v, err := d.Versions(ctx, drive.ByID(it.ID))
		if err != nil {
			t.Fatal(err)
		}
		return len(v)
	}
	meta := metadataVersions(t, d, it.ID)
	time.Sleep(1100 * time.Millisecond) // ctime has a second's resolution on the store's side

	// App keys alone: one .metadata version, ctime unmoved.
	set, err := d.SetInfo(ctx, drive.ByID(it.ID), drive.ItemInfo{App: map[string][]byte{"mirror.version.id": []byte("77")}})
	if err != nil {
		t.Fatal(err)
	}
	if string(set.App["mirror.version.id"]) != "77" || !set.Changed.Equal(before.Changed) || !set.Modified.Equal(before.Modified) {
		t.Fatalf("after app keys: changed %v -> %v, modified %v -> %v, app %q", before.Changed, set.Changed, before.Modified, set.Modified, set.App)
	}
	if n := metadataVersions(t, d, it.ID); n != meta+1 {
		t.Fatalf("%d .metadata versions, want %d", n, meta+1)
	}
	if versions() != 1 {
		t.Fatal("a content version was made")
	}
	pause := time.Now()
	time.Sleep(1100 * time.Millisecond)

	// Mode, times and more keys in one write: one version, ctime moves.
	set, err = d.SetInfo(ctx, drive.ByID(it.ID), drive.ItemInfo{
		Modified: year(2022), Created: year(2020), Mode: u32(0o444),
		App: map[string][]byte{"mirror.version.id": []byte("78"), "mirror.locked": []byte("1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if set.Mode != 0o444 || !set.Modified.Equal(year(2022)) || !set.Created.Equal(year(2020)) || string(set.App["mirror.version.id"]) != "78" || !set.Changed.After(before.Changed) {
		t.Fatalf("after info: %+v", set)
	}
	if n := metadataVersions(t, d, it.ID); n != meta+2 {
		t.Fatalf("%d .metadata versions, want %d", n, meta+2)
	}
	if versions() != 1 {
		t.Fatal("a content version was made")
	}
	// Removing a key.
	set, err = d.SetInfo(ctx, drive.ByID(it.ID), drive.ItemInfo{App: map[string][]byte{"mirror.locked": nil}})
	if err != nil || set.App["mirror.locked"] != nil {
		t.Fatalf("remove: %q, %v", set.App, err)
	}
	// The past keeps the keys as they were then.
	old, err := d.Stat(ctx, drive.ByID(it.ID), drive.At{Time: pause})
	if err != nil || string(old.App["mirror.version.id"]) != "77" || old.Mode != 0o777&^0o022 && old.Mode != before.Mode {
		t.Fatalf("as of %v: app %q, mode %o, %v", pause, old.App, old.Mode, err)
	}
}

// TestMirrorRecordsInfo: the mirror passes app keys, mode and Windows
// attributes through on create, on a new version and on the no-content
// path, each in one .metadata write; an item whose record matches is
// Unchanged and nothing is written. A directory gets its record too.
func TestMirrorRecordsInfo(t *testing.T) {
	d := newDrive(t, miniotest.Store(t), icbfs.Settings{Name: "mirrorinfo", Size: 1 << 30, RootMode: 0o777}, "")
	m := drive.NewMirror(d, srcDrive)
	ctx := system()
	src := &source{}
	withInfo := func(it drive.SourceItem, keys map[string]string, mode uint32) drive.SourceItem {
		it.App = map[string][]byte{}
		for k, v := range keys {
			it.App[k] = []byte(v)
		}
		it.Mode, it.WindowsAttrs = u32(mode), u32(0)
		if mode&0o200 == 0 {
			it.WindowsAttrs = u32(icbfs.WindowsAttrReadOnly)
		}
		return it
	}
	doc := withInfo(src.item("doc", "1", "/Docs/brief.txt", "v1", year(2021)), map[string]string{"mirror.contactId": "c1", "mirror.locked": "0"}, 0o644)
	a := apply(t, ctx, m, "create", doc, drive.Created)
	if string(a.Item.App["mirror.contactId"]) != "c1" || a.Item.Mode != 0o644 {
		t.Fatalf("created: %+v", a.Item)
	}
	if n := metadataVersions(t, d, a.Item.ID); n != 2 {
		t.Fatalf("create: %d .metadata versions, want 2", n)
	}
	apply(t, ctx, m, "same again", doc, drive.Unchanged)

	// Locked at the source, same version: one write, no content version.
	locked := withInfo(doc, map[string]string{"mirror.contactId": "c1", "mirror.locked": "1"}, 0o444)
	a = apply(t, ctx, m, "locked", locked, drive.Retimed)
	if a.Item.Mode != 0o444 || a.Item.WindowsAttrs == nil || *a.Item.WindowsAttrs != icbfs.WindowsAttrReadOnly || string(a.Item.App["mirror.locked"]) != "1" {
		t.Fatalf("locked: %+v", a.Item)
	}
	if n := metadataVersions(t, d, a.Item.ID); n != 3 {
		t.Fatalf("locked: %d .metadata versions, want 3", n)
	}
	if v, _ := d.Versions(ctx, drive.ByID(a.Item.ID)); len(v) != 1 {
		t.Fatalf("locked: %d content versions, want 1", len(v))
	}
	apply(t, ctx, m, "locked again", locked, drive.Unchanged)
	if src.opens.Load() != 1 {
		t.Fatalf("the content was downloaded %d times, want 1", src.opens.Load())
	}

	// A new version carries its record in the write.
	v2 := withInfo(src.item("doc", "2", "/Docs/brief.txt", "v2", year(2022)), map[string]string{"mirror.contactId": "c2", "mirror.locked": "0"}, 0o644)
	a = apply(t, ctx, m, "new version", v2, drive.Updated)
	if string(a.Item.App["mirror.contactId"]) != "c2" || a.Item.Mode != 0o644 || *a.Item.WindowsAttrs != 0 {
		t.Fatalf("updated: %+v", a.Item)
	}
	if n := metadataVersions(t, d, a.Item.ID); n != 4 {
		t.Fatalf("update: %d .metadata versions, want 4", n)
	}

	// A directory: made, then one write for its record and origin.
	dir := drive.SourceItem{Origin: drive.Origin{ID: "folder", Version: "1"}, Path: "/Docs/Sub", Type: drive.TypeDir, Created: year(2019),
		App: map[string][]byte{"mirror.folderId": []byte("f9")}, Mode: u32(0o555)}
	a = apply(t, ctx, m, "dir", dir, drive.Created)
	if string(a.Item.App["mirror.folderId"]) != "f9" || a.Item.Mode != 0o555 || !a.Item.Created.Equal(year(2019)) || a.Item.Origin == nil {
		t.Fatalf("dir: %+v", a.Item)
	}
	if n := metadataVersions(t, d, a.Item.ID); n != 2 {
		t.Fatalf("dir: %d .metadata versions, want 2 (mkdir, then its record)", n)
	}
	apply(t, ctx, m, "dir again", dir, drive.Unchanged)
}
