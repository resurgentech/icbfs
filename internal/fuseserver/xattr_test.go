package fuseserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// posixACL encodes (tag, perm, id) triples as Linux's system.posix_acl_*
// value.
func posixACL(entries ...uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 2)
	for i := 0; i+2 < len(entries); i += 3 {
		b = binary.LittleEndian.AppendUint16(b, uint16(entries[i]))
		b = binary.LittleEndian.AppendUint16(b, uint16(entries[i+1]))
		b = binary.LittleEndian.AppendUint32(b, entries[i+2])
	}
	return b
}

// aclPerm returns the permissions of the entry with tag (and, for named
// entries, id) in an encoded ACL, or -1.
func aclPerm(b []byte, tag uint16, id uint32) int {
	for off := 4; off+8 <= len(b); off += 8 {
		if binary.LittleEndian.Uint16(b[off:]) == tag && (tag != aclUser || binary.LittleEndian.Uint32(b[off+4:]) == id) {
			return int(binary.LittleEndian.Uint16(b[off+2:]))
		}
	}
	return -1
}

const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclMask     = 0x10
	aclOther    = 0x20
	noID        = 0xFFFFFFFF
)

func listXattrs(t *testing.T, path string) []string {
	t.Helper()
	n, err := syscall.Listxattr(path, nil)
	if err != nil {
		t.Fatalf("listxattr size: %v", err)
	}
	buf := make([]byte, n)
	if n, err = syscall.Listxattr(path, buf); err != nil {
		t.Fatalf("listxattr: %v", err)
	}
	var names []string
	for _, s := range strings.Split(string(buf[:n]), "\x00") {
		if s != "" {
			names = append(names, s)
		}
	}
	sort.Strings(names)
	return names
}

func TestMountXattrs(t *testing.T) {
	mnt, fsys := mountTestFSWithFsys(t)
	path := filepath.Join(mnt, "tagged.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Setxattr(path, "user.color", []byte("blue"), 0); err != nil {
		t.Fatalf("setxattr: %v", err)
	}
	if err := syscall.Setxattr(path, "user.shape", []byte("round"), 0); err != nil {
		t.Fatal(err)
	}
	// The size query, then the value.
	n, err := syscall.Getxattr(path, "user.color", nil)
	if err != nil || n != 4 {
		t.Fatalf("size query = %d, %v", n, err)
	}
	buf := make([]byte, 16)
	if n, err = syscall.Getxattr(path, "user.color", buf); err != nil || string(buf[:n]) != "blue" {
		t.Fatalf("getxattr = %q, %v", buf[:n], err)
	}
	if _, err := syscall.Getxattr(path, "user.color", make([]byte, 2)); !errors.Is(err, syscall.ERANGE) {
		t.Fatalf("small buffer = %v, want ERANGE", err)
	}
	if _, err := syscall.Getxattr(path, "user.none", buf); !errors.Is(err, syscall.ENODATA) {
		t.Fatalf("missing = %v, want ENODATA", err)
	}

	const xattrCreate, xattrReplace = 1, 2
	if err := syscall.Setxattr(path, "user.color", []byte("red"), xattrCreate); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("XATTR_CREATE over existing = %v, want EEXIST", err)
	}
	if err := syscall.Setxattr(path, "user.none", []byte("v"), xattrReplace); !errors.Is(err, syscall.ENODATA) {
		t.Fatalf("XATTR_REPLACE of missing = %v, want ENODATA", err)
	}

	// icbfs's own state is never an xattr.
	key := lookupKey(t, fsys, "tagged.txt")
	ctx := context.Background()
	if _, err := fsys.SetAppData(ctx, key, icbfs.TypeFile, icbfs.AppSyncHash, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.SetWindowsAttributes(ctx, key, icbfs.TypeFile, icbfs.WindowsAttrHidden); err != nil {
		t.Fatal(err)
	}
	if names := listXattrs(t, path); strings.Join(names, ",") != "user.color,user.shape" {
		t.Fatalf("listxattr = %v, want only the user's two", names)
	}
	for _, name := range []string{"windows.attributes", "sync.sha256", "user.sync.sha256"} {
		if _, err := syscall.Getxattr(path, name, buf); err == nil {
			t.Fatalf("internal state visible as %q", name)
		}
	}

	if err := syscall.Removexattr(path, "user.shape"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Removexattr(path, "user.shape"); !errors.Is(err, syscall.ENODATA) {
		t.Fatalf("remove missing = %v, want ENODATA", err)
	}
	if names := listXattrs(t, path); strings.Join(names, ",") != "user.color" {
		t.Fatalf("after remove = %v", names)
	}

	// A directory has xattrs too.
	dir := filepath.Join(mnt, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(dir, "user.dirtag", []byte("1"), 0); err != nil {
		t.Fatalf("directory setxattr: %v", err)
	}
	if names := listXattrs(t, dir); strings.Join(names, ",") != "user.dirtag" {
		t.Fatalf("directory list = %v", names)
	}
}

// TestMountPosixACLGrantsAccess: the kernel enforces an ACL entry the mode
// alone doesn't allow. The file belongs to another uid with mode 000; a
// named-user entry for this test's uid lets it read.
func TestMountPosixACLGrantsAccess(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	mnt, fsys := mountTestFSWithFsys(t)
	ctx := context.Background()
	key, _, _, err := fsys.Create(ctx, fsys.RootKey(), "locked.txt", 0, 4242, 4242)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fsys.WriteFile(ctx, key, []byte("secret"), ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mnt, "locked.txt")
	if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read before the ACL = %v, want permission denied", err)
	}

	me := uint32(os.Getuid())
	grant := posixACL(aclUserObj, 0, noID, aclUser, 4, me, aclGroupObj, 0, noID, aclMask, 4, noID, aclOther, 0, noID)
	if _, err := fsys.SetXattr(ctx, key, icbfs.TypeFile, "system.posix_acl_access", grant, icbfs.XattrAny); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "secret" {
		t.Fatalf("read with the ACL = %q, %v", data, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write (the ACL grants read only) = %v, want permission denied", err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o040 {
		t.Fatalf("mode = %v, %v; want 040 (the mask as the group bits)", st.Mode().Perm(), err)
	}
}

// TestMountDefaultACLInheritedAndChmod: a directory's default ACL is set
// through the mount, a file created in it inherits an access ACL, and
// chmod changes that ACL's mask.
func TestMountDefaultACLInheritedAndChmod(t *testing.T) {
	mnt, _ := mountTestFSWithFsys(t)
	dir := filepath.Join(mnt, "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	def := posixACL(aclUserObj, 7, noID, aclUser, 6, 31337, aclGroupObj, 5, noID, aclMask, 7, noID, aclOther, 5, noID)
	if err := syscall.Setxattr(dir, "system.posix_acl_default", def, 0); err != nil {
		t.Fatalf("set default ACL: %v", err)
	}

	file := filepath.Join(dir, "new.txt")
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0o664)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	buf := make([]byte, 256)
	n, err := syscall.Getxattr(file, "system.posix_acl_access", buf)
	if err != nil {
		t.Fatalf("inherited access ACL: %v", err)
	}
	got := buf[:n]
	if aclPerm(got, aclUser, 31337) != 6 {
		t.Fatalf("named entry = %d, want 6", aclPerm(got, aclUser, 31337))
	}
	// The kernel applies the umask before icbfs sees the mode (go-fuse
	// passes no umask, so DONT_MASK can't be used), so the mask is the
	// create mode's group bits after the umask, where ext4 would ignore
	// the umask under a default ACL.
	umask := syscall.Umask(0o22)
	syscall.Umask(umask)
	if want := int(0o664&^umask>>3) & 7; aclPerm(got, aclMask, 0) != want {
		t.Fatalf("mask = %d, want %d", aclPerm(got, aclMask, 0), want)
	}

	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err = syscall.Getxattr(file, "system.posix_acl_access", buf); err != nil {
		t.Fatal(err)
	}
	if m := aclPerm(buf[:n], aclMask, 0); m != 0 {
		t.Fatalf("mask after chmod 600 = %d, want 0", m)
	}
	// A subdirectory inherits the default ACL itself.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if n, err = syscall.Getxattr(sub, "system.posix_acl_default", buf); err != nil || !bytes.Equal(buf[:n], def) {
		t.Fatalf("subdirectory's default ACL = %v, %v", buf[:n], err)
	}
}

func lookupKey(t *testing.T, fsys *icbfs.Filesystem, name string) string {
	t.Helper()
	e, _, err := fsys.Lookup(context.Background(), fsys.RootKey(), name)
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	return e.UUID
}
