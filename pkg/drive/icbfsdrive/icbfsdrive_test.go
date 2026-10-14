package icbfsdrive

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

func newDrive(t *testing.T, store objstore.Store, s icbfs.Settings, root string) *Drive {
	t.Helper()
	ctx := context.Background()
	if _, err := icbfs.CreateFilesystem(ctx, store, s); err != nil && !errors.Is(err, icbfs.ErrExists) {
		t.Fatal(err)
	}
	d, err := New(ctx, Config{Store: store, Filesystem: s.Name, Root: root, ID: "drive-" + s.Name + root, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func as(uid uint32, gids ...uint32) context.Context {
	ctx := drive.WithActor(context.Background(), "user-"+string(rune('a'+uid%26)))
	return drive.WithProfile(ctx, drive.Profile{POSIX: &drive.POSIXProfile{Uid: uid, Gids: gids}})
}

func system() context.Context {
	return drive.WithProfile(drive.WithActor(context.Background(), "system"), drive.Profile{System: true})
}

func write(t *testing.T, ctx context.Context, d drive.Drive, p, content string, cond drive.Condition) drive.Item {
	t.Helper()
	it, err := d.Write(ctx, p, strings.NewReader(content), cond, drive.WriteOptions{Parents: true})
	if err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return it
}

func read(t *testing.T, ctx context.Context, d drive.Drive, ref drive.ItemRef, at drive.At) string {
	t.Helper()
	body, _, err := d.Read(ctx, ref, at, nil)
	if err != nil {
		t.Fatalf("read %+v: %v", ref, err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	return string(data)
}

// TestPOSIXDrive goes through a primary-POSIX drive's operations as an
// ordinary user: write, read, versions, conditional writes, move, delete
// and restore, as-of reads, names, purge, and the mode checks.
func TestPOSIXDrive(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "posix", Size: 1 << 30, RootMode: 0o777}, "")
	alice := as(1000, 1000)
	bob := as(1001, 1001)

	v1 := write(t, alice, d, "/docs/a.txt", "one\n", drive.Condition{})
	if v1.Type != drive.TypeFile || v1.Version == nil || v1.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("item = %+v", v1)
	}
	st, err := d.Stat(alice, drive.ByPath("/docs/a.txt"), drive.At{})
	if err != nil || st.Owner != "uid:1000" || !st.Rights.Write || st.ParentID == "" {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	time.Sleep(1100 * time.Millisecond)
	beforeTwo, _ := store.ServerTime(context.Background())
	time.Sleep(1100 * time.Millisecond)
	v2 := write(t, alice, d, "/docs/a.txt", "one\ntwo\n", drive.Condition{IfMatch: v1.Version.ID})

	// a stale If-Match: 412 with the current version
	_, err = d.Write(alice, "/docs/a.txt", strings.NewReader("x"), drive.Condition{IfMatch: v1.Version.ID}, drive.WriteOptions{})
	var pe *drive.PreconditionError
	if !errors.As(err, &pe) || pe.Current != v2.Version.ID {
		t.Fatalf("stale If-Match: %v", err)
	}
	if _, err := d.Write(alice, "/docs/a.txt", strings.NewReader("x"), drive.Condition{IfNoneMatch: true}, drive.WriteOptions{}); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("If-None-Match on an existing file: %v", err)
	}

	versions, err := d.Versions(alice, drive.ByPath("/docs/a.txt"))
	if err != nil || len(versions) != 2 || !versions[0].Current || versions[1].ID != v1.Version.ID {
		t.Fatalf("versions = %+v, %v", versions, err)
	}
	if got := read(t, alice, d, drive.ByID(v1.ID), drive.At{Version: v1.Version.ID}); got != "one\n" {
		t.Fatalf("version 1 = %q", got)
	}
	if got := read(t, alice, d, drive.ByPath("/docs/a.txt"), drive.At{Time: beforeTwo}); got != "one\n" {
		t.Fatalf("as of before two = %q", got)
	}
	diff, err := drive.Diff(alice, d, drive.ByID(v1.ID), v1.Version.ID, v2.Version.ID)
	if err != nil || !strings.Contains(diff, "+two") {
		t.Fatalf("diff = %q, %v", diff, err)
	}

	// a range
	body, _, err := d.Read(alice, drive.ByPath("/docs/a.txt"), drive.At{}, &drive.Range{Offset: 4, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(body)
	body.Close()
	if string(part) != "two" {
		t.Fatalf("range = %q", part)
	}

	// names
	time.Sleep(1100 * time.Millisecond)
	nm, err := d.CreateName(alice, "signed-off", time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	write(t, alice, d, "/docs/a.txt", "three\n", drive.Condition{})
	if got := read(t, alice, d, drive.ByPath("/docs/a.txt"), drive.At{Name: "signed-off"}); got != "one\ntwo\n" {
		t.Fatalf("as of the name = %q", got)
	}
	versions, _ = d.Versions(alice, drive.ByPath("/docs/a.txt"))
	if len(versions) != 3 || len(versions[1].Names) != 1 || versions[1].Names[0] != nm.Name {
		t.Fatalf("versions with names = %+v", versions)
	}
	if _, err := d.Write(alice, "/x", strings.NewReader("x"), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	// list
	page, err := d.List(alice, drive.ByPath("/"), drive.ListOptions{Depth: 2})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	page, _ = d.List(alice, drive.ByPath("/"), drive.ListOptions{Depth: 2, Limit: 2})
	if len(page.Items) != 2 || page.Next == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	page, _ = d.List(alice, drive.ByPath("/"), drive.ListOptions{Depth: 2, Limit: 2, Cursor: page.Next})
	if len(page.Items) != 1 {
		t.Fatalf("page 2 = %+v", page)
	}

	// move keeps the id
	moved, err := d.Move(alice, drive.ByPath("/docs/a.txt"), "/docs/b.txt", false)
	if err != nil || moved.ID != v1.ID {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	if got := read(t, alice, d, drive.ByID(v1.ID), drive.At{}); got != "three\n" {
		t.Fatalf("by id after move = %q", got)
	}

	// mode checks: bob can read but not write alice's file, nor delete it
	// from her 0755 directory.
	if got := read(t, bob, d, drive.ByPath("/docs/b.txt"), drive.At{}); got != "three\n" {
		t.Fatalf("bob reads %q", got)
	}
	if _, err := d.Write(bob, "/docs/b.txt", strings.NewReader("bob"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("bob writes: %v", err)
	}
	if err := d.Delete(bob, drive.ByPath("/docs/b.txt"), false, drive.Condition{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("bob deletes: %v", err)
	}
	if _, err := d.Stat(context.Background(), drive.ByPath("/docs/b.txt"), drive.At{}); !errors.Is(err, drive.ErrNoAccess) {
		t.Fatalf("no profile: %v", err)
	}

	// delete, then restore with history
	if err := d.Delete(alice, drive.ByPath("/docs/b.txt"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	trash, err := d.Trash(alice, drive.TrashOptions{})
	if err != nil || len(trash.Entries) != 1 || trash.Entries[0].Path != "/docs/b.txt" || trash.Entries[0].DeletedBy != drive.Actor(alice) {
		t.Fatalf("trash = %+v, %v", trash, err)
	}
	if v, err := d.Versions(alice, drive.ByID(v1.ID)); err != nil || len(v) != 3 {
		t.Fatalf("a trashed file's versions = %+v, %v", v, err)
	}
	if _, err := d.Restore(alice, trash.Entries[0].ID, drive.RestoreOptions{To: "/restored/b.txt", Parents: true}); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.Versions(alice, drive.ByPath("/restored/b.txt")); len(v) != 3 {
		t.Fatalf("restored history = %+v", v)
	}

	// a recursive delete is one batch, restored as one
	write(t, alice, d, "/tree/a/1", "1", drive.Condition{})
	write(t, alice, d, "/tree/a/2", "2", drive.Condition{})
	write(t, alice, d, "/tree/b", "b", drive.Condition{})
	if err := d.Delete(alice, drive.ByPath("/tree"), false, drive.Condition{}); !errors.Is(err, drive.ErrNotEmpty) {
		t.Fatalf("non-recursive delete of a full directory: %v", err)
	}
	if err := d.Delete(alice, drive.ByPath("/tree"), true, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	trash, _ = d.Trash(alice, drive.TrashOptions{Path: "/tree"})
	if len(trash.Entries) != 5 || trash.Entries[0].Path != "/tree" || trash.Entries[0].Batch != trash.Entries[0].ID {
		t.Fatalf("batch = %+v", trash.Entries)
	}
	if _, err := d.Restore(alice, trash.Entries[0].ID, drive.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, alice, d, drive.ByPath("/tree/a/2"), drive.At{}); got != "2" {
		t.Fatalf("restored batch = %q", got)
	}

	// purge
	if err := d.Purge(alice, drive.ByPath("/tree"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(alice, drive.ByPath("/tree"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("after purge: %v", err)
	}

	// a 0700 directory of alice's: bob can't list it
	if _, err := d.Mkdir(alice, "/private", false); err != nil {
		t.Fatal(err)
	}
	fs := d.Filesystem()
	e, _, _ := fs.Lookup(context.Background(), fs.RootKey(), "private")
	mode := uint32(0o700)
	if _, err := fs.SetAttr(context.Background(), e.UUID, icbfs.TypeDir, &mode, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.List(bob, drive.ByPath("/private"), drive.ListOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("bob lists a 0700 directory: %v", err)
	}
}

// TestSubtreeDrive: a drive on a directory sees only its subtree, its
// own trash, and names at, above or below its root — not a sibling's.
func TestSubtreeDrive(t *testing.T) {
	store := miniotest.Store(t)
	whole := newDrive(t, store, icbfs.Settings{Name: "shared", Size: 1 << 30, RootMode: 0o777}, "")
	ctx := system()
	write(t, ctx, whole, "/team-a/x", "a", drive.Condition{})
	write(t, ctx, whole, "/team-b/y", "b", drive.Condition{})
	a := newDrive(t, store, icbfs.Settings{Name: "shared"}, "/team-a")
	b := newDrive(t, store, icbfs.Settings{Name: "shared"}, "/team-b")

	if got := read(t, ctx, a, drive.ByPath("/x"), drive.At{}); got != "a" {
		t.Fatalf("a's /x = %q", got)
	}
	if _, err := a.Stat(ctx, drive.ByPath("/y"), drive.At{}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("a sees b's file: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := a.CreateName(ctx, "a-only", time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateName(ctx, "everyone", time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	names := func(d drive.Drive) string {
		ns, err := d.Names(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range ns {
			out = append(out, n.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(a); got != "a-only,everyone" {
		t.Fatalf("a's names: %s", got)
	}
	if got := names(b); got != "everyone" {
		t.Fatalf("b's names: %s", got)
	}
	if got := names(whole); got != "a-only,everyone" {
		t.Fatalf("the filesystem's names: %s", got)
	}
	if err := b.Delete(ctx, drive.ByPath("/y"), false, drive.Condition{}); err != nil {
		t.Fatal(err)
	}
	if tr, _ := a.Trash(ctx, drive.TrashOptions{}); len(tr.Entries) != 0 {
		t.Fatalf("a sees b's trash: %+v", tr)
	}
	if tr, _ := b.Trash(ctx, drive.TrashOptions{}); len(tr.Entries) != 1 || tr.Entries[0].Path != "/y" {
		t.Fatalf("b's trash: %+v", tr)
	}
	if tr, _ := whole.Trash(ctx, drive.TrashOptions{}); len(tr.Entries) != 1 || tr.Entries[0].Path != "/team-b/y" {
		t.Fatalf("the filesystem's trash: %+v", tr)
	}

	// copy between drives, with its origin
	c, err := drive.Copy(ctx, a, "drive-a", drive.ByPath("/x"), drive.At{}, b, "/copied", drive.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := b.Stat(ctx, drive.ByPath("/copied"), drive.At{})
	if st.Origin == nil || st.Origin.Drive != "drive-a" || st.Origin.Path != "/x" || c.Origin == nil {
		t.Fatalf("copy's origin = %+v", st.Origin)
	}
}

const (
	sidAlice = "S-1-5-21-1-2-3-1001"
	sidBob   = "S-1-5-21-1-2-3-1002"
	sidStaff = "S-1-5-21-1-2-3-2000"
)

func asSID(user string, groups ...string) context.Context {
	ctx := drive.WithActor(context.Background(), user)
	return drive.WithProfile(ctx, drive.Profile{Windows: &drive.WindowsProfile{User: user, Groups: groups}})
}

// TestWindowsDrive: a primary-Windows drive checks the security
// descriptors, new items inherit from their directory, and an item with
// none of its own takes its ancestor's.
func TestWindowsDrive(t *testing.T) {
	store := miniotest.Store(t)
	rootSDDL := "O:" + sidAlice + "G:" + sidAlice + "D:PAI(A;OICI;FA;;;" + sidAlice + ")(A;OICI;0x1200a9;;;" + sidStaff + ")"
	d := newDrive(t, store, icbfs.Settings{Name: "win", Size: 1 << 30, PrimaryWindows: true, RootSDDL: rootSDDL}, "")
	alice := asSID(sidAlice)
	bob := asSID(sidBob, sidStaff)
	stranger := asSID("S-1-5-21-9-9-9-500")

	write(t, alice, d, "/Reports/q3.txt", "q3", drive.Condition{})
	if got := read(t, bob, d, drive.ByPath("/reports/Q3.TXT"), drive.At{}); got != "q3" {
		t.Fatalf("bob reads (case-insensitively) %q", got)
	}
	if _, err := d.Write(bob, "/Reports/q3.txt", strings.NewReader("bob"), drive.Condition{}, drive.WriteOptions{}); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("staff (read+execute) writes: %v", err)
	}
	if _, err := d.Stat(stranger, drive.ByPath("/Reports/q3.txt"), drive.At{}); err != nil {
		t.Fatalf("a stranger stats: %v", err) // traversal and stat aren't checked; content is
	}
	if _, _, err := d.Read(stranger, drive.ByPath("/Reports/q3.txt"), drive.At{}, nil); !errors.Is(err, drive.ErrForbidden) {
		t.Fatalf("a stranger reads: %v", err)
	}
	perms, err := d.Permissions(alice, drive.ByPath("/Reports/q3.txt"))
	if err != nil || perms.Kind != "windows" || perms.Inherited != "" || perms.Owner != sidAlice || !strings.Contains(perms.SDDL, "ID") {
		t.Fatalf("a new file's permissions = %+v, %v", perms, err)
	}

	// an item with no descriptor of its own (made by a mount that set
	// none) inherits its directory's as it stands
	fs := d.Filesystem()
	rep, _, _ := fs.Lookup(context.Background(), fs.RootKey(), "Reports")
	if _, _, _, err := fs.Create(context.Background(), rep.UUID, "bare.txt", 0o644, 0, 0); err != nil {
		t.Fatal(err)
	}
	perms, _ = d.Permissions(alice, drive.ByPath("/Reports/bare.txt"))
	if perms.Inherited != "/Reports" {
		t.Fatalf("bare file's permissions = %+v", perms)
	}
	st, _ := d.Stat(bob, drive.ByPath("/Reports/bare.txt"), drive.At{})
	if !st.Rights.Read || st.Rights.Write || st.Rights.Delete {
		t.Fatalf("bob's rights on the bare file = %+v", st.Rights)
	}

	ids, err := d.Identities(alice, nil)
	if err != nil || len(ids) < 2 || ids[0].ID != sidAlice {
		t.Fatalf("identities = %+v, %v", ids, err)
	}
}

// TestDrafts: drafts live in the side store, apart from versions;
// publishing is conditional on the draft's base version.
func TestDrafts(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "docs", Size: 1 << 30, RootMode: 0o777}, "")
	side, err := OpenSideStore(context.Background(), store, "host-side")
	if err != nil {
		t.Fatal(err)
	}
	drafts := &drive.Drafts{Side: side}
	ctx := as(1000, 1000)
	it := write(t, ctx, d, "/doc.md", "base", drive.Condition{})

	if _, err := drafts.Save(ctx, "docs", it.ID, "alice", it.Version.ID, []byte("draft 1")); err != nil {
		t.Fatal(err)
	}
	if _, err := drafts.Save(ctx, "docs", it.ID, "alice", it.Version.ID, []byte("draft 2")); err != nil {
		t.Fatal(err)
	}
	if _, err := drafts.Save(ctx, "docs", it.ID, "bob", it.Version.ID, []byte("bob's")); err != nil {
		t.Fatal(err)
	}
	dr, body, err := drafts.Get(ctx, "docs", it.ID, "alice")
	if err != nil || string(body) != "draft 2" || dr.Base != it.Version.ID {
		t.Fatalf("draft = %+v %q, %v", dr, body, err)
	}
	users, _ := drafts.Users(ctx, "docs", it.ID)
	if strings.Join(users, ",") != "alice,bob" {
		t.Fatalf("users = %v", users)
	}
	if v, _ := d.Versions(ctx, drive.ByID(it.ID)); len(v) != 1 {
		t.Fatalf("drafts made versions: %+v", v)
	}

	out, err := drafts.Publish(ctx, d, "docs", it.ID, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, d, drive.ByID(it.ID), drive.At{}); got != "draft 2" {
		t.Fatalf("published = %q", got)
	}
	if _, _, err := drafts.Get(ctx, "docs", it.ID, "alice"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("published draft still there: %v", err)
	}
	// bob's draft started from the old version: conflict, unless forced
	if _, err := drafts.Publish(ctx, d, "docs", it.ID, "bob", false); !errors.Is(err, drive.ErrPrecondition) {
		t.Fatalf("stale draft published: %v", err)
	}
	if _, err := drafts.Publish(ctx, d, "docs", it.ID, "bob", true); err != nil {
		t.Fatal(err)
	}
	_ = out
	if err := drafts.DeleteAll(ctx, "docs", it.ID); err != nil {
		t.Fatal(err)
	}
}
