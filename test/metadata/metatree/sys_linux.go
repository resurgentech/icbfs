package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Linux can't set a creation time; it reads one (statx), which a copy
// sets to the moment of the copy.
const keepsBtime = false

// A symlink's access time moves when it's followed, by anything.
const keepsSymlinkAtime = false

func prepare() error { return nil }

func setMeta(p string, it Item) error {
	px := it.POSIX
	if px == nil {
		return nil
	}
	for name, v := range px.Xattrs {
		b, err := xattrValue(v)
		if err != nil {
			return err
		}
		if err := unix.Lsetxattr(p, name, b, 0); err != nil {
			return fmt.Errorf("setxattr %s: %w", name, err)
		}
	}
	if px.Uid != nil || px.Gid != nil {
		uid, gid := -1, -1
		if px.Uid != nil {
			uid = int(*px.Uid)
		}
		if px.Gid != nil {
			gid = int(*px.Gid)
		}
		if err := os.Lchown(p, uid, gid); err != nil {
			return err
		}
	}
	// After the owner (chown clears setuid and setgid), before the ACL
	// (whose mask is the group bits).
	if px.Mode != "" && it.Type != "symlink" {
		m, err := strconv.ParseUint(px.Mode, 8, 32)
		if err != nil {
			return fmt.Errorf("mode %q: %w", px.Mode, err)
		}
		if err := unix.Chmod(p, uint32(m)); err != nil {
			return err
		}
	}
	for name, text := range map[string]string{"system.posix_acl_access": px.ACL, "system.posix_acl_default": px.DefaultACL} {
		if text == "" {
			continue
		}
		b, err := encodeACL(text)
		if err != nil {
			return err
		}
		if err := unix.Lsetxattr(p, name, b, 0); err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
	}
	return nil
}

func setTimes(p string, it Item) error {
	if it.Mtime == nil && it.Atime == nil {
		return nil
	}
	ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, {Nsec: unix.UTIME_OMIT}}
	if it.Atime != nil {
		ts[0] = unix.NsecToTimespec(it.Atime.UnixNano())
	}
	if it.Mtime != nil {
		ts[1] = unix.NsecToTimespec(it.Mtime.UnixNano())
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func setSecurity(string, Item) error   { return nil }
func setAttributes(string, Item) error { return nil }

func readItem(p string) (Item, string, error) {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, p, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS|unix.STATX_BTIME, &st); err != nil {
		return Item{}, "", err
	}
	it := Item{POSIX: &POSIX{}}
	mode := uint32(st.Mode)
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		it.Type = "dir"
	case unix.S_IFLNK:
		it.Type = "symlink"
		t, err := os.Readlink(p)
		if err != nil {
			return Item{}, "", err
		}
		it.Target = t
	case unix.S_IFREG:
		it.Type = "file"
		data, err := os.ReadFile(p)
		if err != nil {
			return Item{}, "", err
		}
		it.SHA = hash(data)
		size := int64(st.Size)
		it.Size = &size
	default:
		it.Type = fmt.Sprintf("other %o", mode&unix.S_IFMT)
	}
	ts := func(t unix.StatxTimestamp) *time.Time {
		v := time.Unix(t.Sec, int64(t.Nsec)).UTC()
		return &v
	}
	it.Mtime, it.Atime = ts(st.Mtime), ts(st.Atime)
	if st.Mask&unix.STATX_BTIME != 0 {
		it.Btime = ts(st.Btime)
	}
	if it.Type != "symlink" {
		it.POSIX.Mode = fmt.Sprintf("%04o", mode&0o7777)
	}
	uid, gid := st.Uid, st.Gid
	it.POSIX.Uid, it.POSIX.Gid = &uid, &gid
	xattrs, err := listXattrs(p)
	if err != nil {
		return Item{}, "", err
	}
	for name, v := range xattrs {
		switch name {
		case "system.posix_acl_access":
			it.POSIX.ACL = decodeACL(v)
		case "system.posix_acl_default":
			it.POSIX.DefaultACL = decodeACL(v)
		default:
			if it.POSIX.Xattrs == nil {
				it.POSIX.Xattrs = map[string]string{}
			}
			it.POSIX.Xattrs[name] = showXattr(v)
		}
	}
	id := ""
	if it.Type == "file" && st.Nlink > 1 {
		id = fmt.Sprintf("%d:%d:%d", st.Dev_major, st.Dev_minor, st.Ino)
	}
	return it, id, nil
}

func listXattrs(p string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(p, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		return nil, err
	}
	buf := make([]byte, size)
	if size, err = unix.Llistxattr(p, buf); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		n, err := unix.Lgetxattr(p, name, nil)
		if err != nil {
			return nil, fmt.Errorf("getxattr %s: %w", name, err)
		}
		v := make([]byte, n)
		if n, err = unix.Lgetxattr(p, name, v); err != nil {
			return nil, fmt.Errorf("getxattr %s: %w", name, err)
		}
		out[name] = v[:n]
	}
	return out, nil
}

func checkPlatform(want, got Item) []string {
	w, g := want.POSIX, got.POSIX
	if w == nil {
		return nil
	}
	var diffs []string
	add := func(field, want, got string) {
		diffs = append(diffs, fmt.Sprintf("%s: want %s, got %s", field, want, got))
	}
	if w.Mode != "" && want.Type != "symlink" && w.Mode != g.Mode {
		add("mode", w.Mode, g.Mode)
	}
	if w.Uid != nil && *w.Uid != *g.Uid {
		add("uid", fmt.Sprint(*w.Uid), fmt.Sprint(*g.Uid))
	}
	if w.Gid != nil && *w.Gid != *g.Gid {
		add("gid", fmt.Sprint(*w.Gid), fmt.Sprint(*g.Gid))
	}
	wx, err := normXattrs(w.Xattrs)
	if err != nil {
		return append(diffs, err.Error())
	}
	var names []string
	for k := range wx {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if gv := g.Xattrs[k]; gv != wx[k] {
			add("xattr "+k, wx[k], orNone(gv))
		}
	}
	if w.ACL != "" && normACL(w.ACL) != g.ACL {
		add("acl", normACL(w.ACL), orNone(g.ACL))
	}
	if w.DefaultACL != "" && normACL(w.DefaultACL) != g.DefaultACL {
		add("defaultAcl", normACL(w.DefaultACL), orNone(g.DefaultACL))
	}
	return diffs
}

// POSIX ACLs as Linux keeps them in system.posix_acl_*: a version, then
// entries of a tag, permissions and an id.
const aclVersion = 2

var aclTags = []struct {
	tag  uint16
	name string
	id   bool
}{{1, "u", false}, {2, "u", true}, {4, "g", false}, {8, "g", true}, {0x10, "m", false}, {0x20, "o", false}}

func encodeACL(text string) ([]byte, error) {
	b := binary.LittleEndian.AppendUint32(nil, aclVersion)
	for _, e := range strings.Split(text, ",") {
		parts := strings.Split(e, ":")
		if len(parts) != 3 || len(parts[2]) != 3 {
			return nil, fmt.Errorf("ACL entry %q: want u:ID:rwx", e)
		}
		var tag uint16
		id := uint32(0xffffffff)
		for _, t := range aclTags {
			if t.name == parts[0] && t.id == (parts[1] != "") {
				tag = t.tag
			}
		}
		if tag == 0 {
			return nil, fmt.Errorf("ACL entry %q: unknown tag", e)
		}
		if parts[1] != "" {
			n, err := strconv.ParseUint(parts[1], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("ACL entry %q: %w", e, err)
			}
			id = uint32(n)
		}
		var perm uint16
		for i, c := range "rwx" {
			if parts[2][i] == byte(c) {
				perm |= 4 >> i
			}
		}
		b = binary.LittleEndian.AppendUint16(b, tag)
		b = binary.LittleEndian.AppendUint16(b, perm)
		b = binary.LittleEndian.AppendUint32(b, id)
	}
	return b, nil
}

func decodeACL(b []byte) string {
	if len(b) < 4 || binary.LittleEndian.Uint32(b) != aclVersion {
		return "base64:" + showXattr(b)
	}
	var out []string
	for b = b[4:]; len(b) >= 8; b = b[8:] {
		tag, perm, id := binary.LittleEndian.Uint16(b), binary.LittleEndian.Uint16(b[2:]), binary.LittleEndian.Uint32(b[4:])
		name, withID := "?", false
		for _, t := range aclTags {
			if t.tag == tag {
				name, withID = t.name, t.id
			}
		}
		who := ""
		if withID {
			who = strconv.FormatUint(uint64(id), 10)
		}
		rwx := []byte("---")
		for i, c := range "rwx" {
			if perm&(4>>i) != 0 {
				rwx[i] = byte(c)
			}
		}
		out = append(out, name+":"+who+":"+string(rwx))
	}
	return strings.Join(out, ",")
}

// normACL is a description's ACL as a dump shows it (Linux keeps the
// entries in tag order).
func normACL(text string) string {
	b, err := encodeACL(text)
	if err != nil {
		return "invalid: " + err.Error()
	}
	return decodeACL(b)
}
