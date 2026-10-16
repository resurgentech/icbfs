package icbfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// acl builds an ACL in Linux's xattr encoding from (tag, perm, id)
// triples; id is ignored for the owner, group, mask and other entries.
func acl(entries ...uint32) []byte {
	var a posixACL
	for i := 0; i+2 < len(entries); i += 3 {
		a = append(a, posixACLEntry{tag: uint16(entries[i]), perm: uint16(entries[i+1]), id: entries[i+2]})
	}
	return a.encode()
}

const noID = 0xFFFFFFFF

func TestParsePosixACLChecksLikeLinux(t *testing.T) {
	good := acl(aclUserObj, 7, noID, aclUser, 6, 1000, aclGroupObj, 5, noID, aclMask, 6, noID, aclOther, 4, noID)
	if _, err := parsePosixACL(good); err != nil {
		t.Fatalf("valid ACL refused: %v", err)
	}
	for name, b := range map[string][]byte{
		"truncated":          good[:len(good)-3],
		"wrong version":      append([]byte{3, 0, 0, 0}, good[4:]...),
		"no other":           acl(aclUserObj, 7, noID, aclGroupObj, 5, noID),
		"named without mask": acl(aclUserObj, 7, noID, aclUser, 6, 1000, aclGroupObj, 5, noID, aclOther, 4, noID),
		"two owners":         acl(aclUserObj, 7, noID, aclUserObj, 7, noID, aclGroupObj, 5, noID, aclOther, 4, noID),
		"duplicate user":     acl(aclUserObj, 7, noID, aclUser, 6, 1000, aclUser, 4, 1000, aclGroupObj, 5, noID, aclMask, 6, noID, aclOther, 4, noID),
		"bad permission":     acl(aclUserObj, 8, noID, aclGroupObj, 5, noID, aclOther, 4, noID),
		"unknown tag":        acl(aclUserObj, 7, noID, 0x40, 1, noID, aclGroupObj, 5, noID, aclOther, 4, noID),
	} {
		if _, err := parsePosixACL(b); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: parse = %v, want ErrInvalidArgument", name, err)
		}
	}
	if a, err := parsePosixACL([]byte{2, 0, 0, 0}); err != nil || a != nil {
		t.Errorf("an empty ACL = %v, %v; want none", a, err)
	}
}

func TestACLModeAndInheritance(t *testing.T) {
	a, _ := parsePosixACL(acl(aclUserObj, 7, noID, aclUser, 7, 1000, aclGroupObj, 5, noID, aclMask, 6, noID, aclOther, 0, noID))
	if m := a.mode(); m != 0o760 {
		t.Fatalf("mode = %o, want 760 (the mask is the group bits)", m)
	}
	// A new file asks for 0644: owner rw, mask r (from the group bits),
	// other r; the named entry is unchanged (the mask limits it).
	got := a.limitedBy(0o644)
	if m := got.mode(); m != 0o640 {
		t.Fatalf("inherited mode = %o, want 640", m)
	}
	if got.perm(aclUser) != 7 {
		t.Fatalf("named entry changed: %v", got)
	}
}

func TestXattrNamespacesAndLimits(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "x.txt", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"user.a", "trusted.b", "security.c"} {
		if _, err := fsys.SetXattr(ctx, key, TypeFile, name, []byte(name), XattrAny); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	for _, name := range []string{"system.other", "windows.acl", "user.", "nonamespace"} {
		if _, err := fsys.SetXattr(ctx, key, TypeFile, name, []byte("v"), XattrAny); !errors.Is(err, ErrNotSupported) {
			t.Errorf("set %q = %v, want ErrNotSupported", name, err)
		}
	}
	if _, err := fsys.SetXattr(ctx, key, TypeFile, "user."+strings.Repeat("n", 251), []byte("v"), XattrAny); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("256-byte name = %v, want ErrNameTooLong", err)
	}
	if _, err := fsys.SetXattr(ctx, key, TypeFile, "user.big", make([]byte, maxXattrValue+1), XattrAny); !errors.Is(err, ErrXattrTooBig) {
		t.Errorf("oversized value = %v, want ErrXattrTooBig", err)
	}
	// A full-size value fits beside others; enough of them don't.
	for i := 0; ; i++ {
		_, err := fsys.SetXattr(ctx, key, TypeFile, fmt.Sprintf("user.fill%d", i), make([]byte, maxXattrValue), XattrAny)
		if errors.Is(err, ErrXattrNoSpace) {
			if i < 3 {
				t.Errorf("only %d full-size values fit", i)
			}
			break
		}
		if err != nil {
			t.Fatalf("full-size value %d: %v", i, err)
		}
	}
	for i := 0; i < 4; i++ {
		_, _ = fsys.RemoveXattr(ctx, key, TypeFile, fmt.Sprintf("user.fill%d", i))
	}

	if _, err := fsys.SetXattr(ctx, key, TypeFile, "user.a", []byte("again"), XattrCreate); !errors.Is(err, ErrExists) {
		t.Errorf("create over existing = %v, want ErrExists", err)
	}
	if _, err := fsys.SetXattr(ctx, key, TypeFile, "user.none", []byte("v"), XattrReplace); !errors.Is(err, ErrNoAttr) {
		t.Errorf("replace missing = %v, want ErrNoAttr", err)
	}
	if _, err := fsys.SetXattr(ctx, key, TypeFile, "user.a", []byte("replaced"), XattrReplace); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.RemoveXattr(ctx, key, TypeFile, "trusted.b"); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.RemoveXattr(ctx, key, TypeFile, "trusted.b"); !errors.Is(err, ErrNoAttr) {
		t.Errorf("remove missing = %v, want ErrNoAttr", err)
	}

	got, err := fsys.Stat(ctx, key, TypeFile)
	if err != nil {
		t.Fatal(err)
	}
	if names := XattrNames(got); !reflect.DeepEqual(names, []string{"security.c", "user.a"}) {
		t.Fatalf("names = %v", names)
	}
	if v, err := XattrValue(got, "user.a"); err != nil || string(v) != "replaced" {
		t.Fatalf("user.a = %q, %v", v, err)
	}
}

func TestPosixACLKeepsModeInStep(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "acl.txt", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// An extended ACL: stored, and the mode's group bits are its mask.
	ext := acl(aclUserObj, 6, noID, aclUser, 7, 1000, aclGroupObj, 4, noID, aclMask, 7, noID, aclOther, 0, noID)
	attr, err := fsys.SetXattr(ctx, key, TypeFile, xattrACLAccess, ext, XattrAny)
	if err != nil {
		t.Fatal(err)
	}
	if attr.Mode&0o777 != 0o670 || !bytes.Equal(attr.PosixACLAccess, ext) {
		t.Fatalf("after setting the ACL: mode %o, ACL %v", attr.Mode, attr.PosixACLAccess)
	}
	// chmod moves the owner, mask and other entries with it.
	if attr, err = fsys.SetAttr(ctx, key, TypeFile, ptrU32(0o640), nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := parsePosixACL(attr.PosixACLAccess)
	if a.perm(aclUserObj) != 6 || a.perm(aclMask) != 4 || a.perm(aclOther) != 0 || a.perm(aclUser) != 7 || a.perm(aclGroupObj) != 4 {
		t.Fatalf("ACL after chmod 640: %v", a)
	}
	// An ACL that says no more than a mode is just the mode.
	plain := acl(aclUserObj, 7, noID, aclGroupObj, 5, noID, aclOther, 1, noID)
	if attr, err = fsys.SetXattr(ctx, key, TypeFile, xattrACLAccess, plain, XattrAny); err != nil {
		t.Fatal(err)
	}
	if attr.Mode&0o777 != 0o751 || attr.PosixACLAccess != nil {
		t.Fatalf("a mode-only ACL: mode %o, ACL %v", attr.Mode, attr.PosixACLAccess)
	}
	if _, err := fsys.SetXattr(ctx, key, TypeFile, xattrACLDefault, plain, XattrAny); !errors.Is(err, ErrACLNotDir) {
		t.Fatalf("default ACL on a file = %v, want ErrACLNotDir", err)
	}
}

func TestDefaultACLIsInherited(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnablePosixACLs(true)
	ctx := context.Background()
	dir, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "shared", 0o755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	def := acl(aclUserObj, 7, noID, aclUser, 7, 1000, aclGroupObj, 7, noID, aclMask, 7, noID, aclOther, 5, noID)
	if _, err := fsys.SetXattr(ctx, dir, TypeDir, xattrACLDefault, def, XattrAny); err != nil {
		t.Fatal(err)
	}

	_, file, _, err := fsys.Create(ctx, dir, "f.txt", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if file.Mode&0o777 != 0o644 {
		t.Fatalf("inherited file mode = %o, want 644", file.Mode)
	}
	a, err := parsePosixACL(file.PosixACLAccess)
	if err != nil || a == nil || a.perm(aclUser) != 7 || a.perm(aclMask) != 4 {
		t.Fatalf("inherited access ACL = %v, %v", a, err)
	}
	if file.PosixACLDefault != nil {
		t.Fatal("a file got a default ACL")
	}

	_, sub, err := fsys.Mkdir(ctx, dir, "sub", 0o750, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sub.PosixACLDefault, def) || sub.Mode&0o777 != 0o750 {
		t.Fatalf("subdirectory: mode %o, default ACL %v", sub.Mode, sub.PosixACLDefault)
	}

	// Without EnablePosixACLs nothing is inherited.
	fsys.EnablePosixACLs(false)
	_, plain, _, err := fsys.Create(ctx, dir, "plain.txt", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plain.PosixACLAccess != nil {
		t.Fatal("inherited with ACLs off")
	}
}

func TestPosixACLsRefusedOnPrimaryWindows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	fsys := New(store, "win")
	if err := fsys.Bootstrap(ctx, 1<<30, 0o755, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "f", 0o644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	plain := acl(aclUserObj, 7, noID, aclGroupObj, 5, noID, aclOther, 1, noID)
	if _, err := fsys.SetXattr(ctx, key, TypeFile, xattrACLAccess, plain, XattrAny); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("POSIX ACL on primary-Windows = %v, want ErrNotSupported", err)
	}
	if _, err := fsys.SetXattr(ctx, key, TypeFile, "user.ok", []byte("v"), XattrAny); err != nil {
		t.Fatalf("user xattr on primary-Windows: %v", err)
	}
}

func ptrU32(v uint32) *uint32 { return &v }

// TestSetgidDirectoryInheritance: in a setgid directory, new items take
// the directory's group, and new directories its setgid bit too
// (xfstests' generic/314).
func TestSetgidDirectoryInheritance(t *testing.T) {
	fsys, _ := newTestFilesystem(t)
	fsys.EnablePosixACLs(true)
	ctx := context.Background()
	dir, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "shared", 0o2775, 1000, 4242)
	if err != nil {
		t.Fatal(err)
	}
	sub, subAttr, err := fsys.Mkdir(ctx, dir, "sub", 0o755, 1001, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if subAttr.Gid != 4242 || subAttr.Mode&0o2000 == 0 {
		t.Fatalf("subdirectory: gid %d mode %o, want gid 4242 and setgid", subAttr.Gid, subAttr.Mode)
	}
	_, fileAttr, _, err := fsys.Create(ctx, sub, "f", 0o644, 1001, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if fileAttr.Gid != 4242 || fileAttr.Mode&0o2000 != 0 {
		t.Fatalf("file: gid %d mode %o, want gid 4242 and no setgid", fileAttr.Gid, fileAttr.Mode)
	}
	_, plain, err := fsys.Mkdir(ctx, fsys.RootKey(), "plain", 0o755, 1001, 1001)
	if err != nil || plain.Gid != 1001 || plain.Mode&0o2000 != 0 {
		t.Fatalf("outside: gid %d mode %o (%v), want the creator's group", plain.Gid, plain.Mode, err)
	}
}
