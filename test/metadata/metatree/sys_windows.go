package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NTFS keeps and sets creation times.
const keepsBtime = true

// A symlink's own times are kept like any file's.
const keepsSymlinkAtime = true

// prepare enables the privileges that let an Administrator set any owner
// and get past any ACL: restore and backup.
func prepare() error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return err
	}
	defer tok.Close()
	for _, name := range []string{"SeRestorePrivilege", "SeBackupPrivilege", "SeTakeOwnershipPrivilege"} {
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
			return err
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		if err := windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil); err != nil {
			return fmt.Errorf("enable %s: %w", name, err)
		}
	}
	return nil
}

// open opens p itself (a directory, or a link rather than its target),
// past its ACL with the backup and restore privileges.
func open(p string, access uint32) (windows.Handle, error) {
	return windows.CreateFile(windows.StringToUTF16Ptr(p), access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

func setMeta(p string, it Item) error {
	w := it.Windows
	if w == nil {
		return nil
	}
	var names []string
	for name := range w.Streams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(p+":"+name, []byte(w.Streams[name]), 0o644); err != nil {
			return fmt.Errorf("stream %s: %w", name, err)
		}
	}
	return nil
}

func setTimes(p string, it Item) error {
	if it.Mtime == nil && it.Atime == nil && it.Btime == nil {
		return nil
	}
	h, err := open(p, windows.FILE_WRITE_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	ft := func(t *time.Time) *windows.Filetime {
		if t == nil {
			return nil
		}
		f := windows.NsecToFiletime(t.UnixNano())
		return &f
	}
	return windows.SetFileTime(h, ft(it.Btime), ft(it.Atime), ft(it.Mtime))
}

// setAttributes sets the R, H, S and A attributes as described.
func setAttributes(p string, it Item) error {
	w := it.Windows
	if w == nil || w.Attributes == "" {
		return nil
	}
	{
		attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(p))
		if err != nil {
			return err
		}
		attrs &^= windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM | windows.FILE_ATTRIBUTE_ARCHIVE
		attrs |= parseAttrs(w.Attributes)
		if attrs == 0 {
			attrs = windows.FILE_ATTRIBUTE_NORMAL
		}
		if err := windows.SetFileAttributes(windows.StringToUTF16Ptr(p), attrs); err != nil {
			return err
		}
	}
	return nil
}

// setSecurity sets the owner, group and DACL as described.
func setSecurity(p string, it Item) error {
	w := it.Windows
	if w == nil {
		return nil
	}
	if w.SDDL != "" {
		sd, err := windows.SecurityDescriptorFromString(w.SDDL)
		if err != nil {
			return fmt.Errorf("SDDL %q: %w", w.SDDL, err)
		}
		var info windows.SECURITY_INFORMATION
		owner, _, _ := sd.Owner()
		group, _, _ := sd.Group()
		dacl, _, _ := sd.DACL()
		if owner != nil {
			info |= windows.OWNER_SECURITY_INFORMATION
		}
		if group != nil {
			info |= windows.GROUP_SECURITY_INFORMATION
		}
		if dacl != nil {
			info |= windows.DACL_SECURITY_INFORMATION
			if ctl, _, _ := sd.Control(); ctl&windows.SE_DACL_PROTECTED != 0 {
				info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
			} else {
				info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
			}
		}
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, info, owner, group, dacl, nil); err != nil {
			return fmt.Errorf("set ACL: %w", err)
		}
	}
	return nil
}

const attrLetters = "RHSA"

var attrBits = []uint32{windows.FILE_ATTRIBUTE_READONLY, windows.FILE_ATTRIBUTE_HIDDEN, windows.FILE_ATTRIBUTE_SYSTEM, windows.FILE_ATTRIBUTE_ARCHIVE}

func parseAttrs(s string) uint32 {
	var a uint32
	for i, c := range attrLetters {
		if strings.ContainsRune(s, c) {
			a |= attrBits[i]
		}
	}
	return a
}

func showAttrs(a uint32) string {
	var b strings.Builder
	for i, c := range attrLetters {
		if a&attrBits[i] != 0 {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}

func readItem(p string) (Item, string, error) {
	h, err := open(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return Item{}, "", err
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return Item{}, "", err
	}
	it := Item{Windows: &Windows{Attributes: showAttrs(fi.FileAttributes)}}
	isDir := fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	switch {
	case fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		t, err := os.Readlink(p)
		if err != nil {
			return Item{}, "", err
		}
		it.Type, it.Target = "symlink", filepath.ToSlash(t)
	case isDir:
		it.Type = "dir"
	default:
		it.Type = "file"
		data, err := os.ReadFile(p)
		if err != nil {
			return Item{}, "", err
		}
		it.SHA = hash(data)
		size := int64(fi.FileSizeHigh)<<32 | int64(fi.FileSizeLow)
		it.Size = &size
	}
	t := func(f windows.Filetime) *time.Time {
		v := time.Unix(0, f.Nanoseconds()).UTC()
		return &v
	}
	it.Btime, it.Atime, it.Mtime = t(fi.CreationTime), t(fi.LastAccessTime), t(fi.LastWriteTime)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err == nil {
		it.Windows.SDDL = sd.String()
	} else {
		it.Windows.SDDL = "unreadable: " + err.Error()
	}
	if it.Type != "symlink" {
		if it.Windows.Streams, err = streams(p); err != nil {
			return Item{}, "", err
		}
	}
	id := ""
	if it.Type == "file" && fi.NumberOfLinks > 1 {
		id = fmt.Sprintf("%x:%x:%x", fi.VolumeSerialNumber, fi.FileIndexHigh, fi.FileIndexLow)
	}
	return it, id, nil
}

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	findFirstStreamW = kernel32.NewProc("FindFirstStreamW")
	findNextStreamW  = kernel32.NewProc("FindNextStreamW")
)

// win32FindStreamData is WIN32_FIND_STREAM_DATA.
type win32FindStreamData struct {
	StreamSize int64
	StreamName [windows.MAX_PATH + 36]uint16
}

// streams are p's alternate data streams, by name, each its content's
// SHA-256.
func streams(p string) (map[string]string, error) {
	var d win32FindStreamData
	h, _, err := findFirstStreamW.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(p))), 0, uintptr(unsafe.Pointer(&d)), 0)
	if windows.Handle(h) == windows.InvalidHandle {
		if err == windows.ERROR_HANDLE_EOF {
			return nil, nil
		}
		return nil, nil // a volume without streams (or a link): none
	}
	defer windows.FindClose(windows.Handle(h))
	out := map[string]string{}
	for {
		name := windows.UTF16ToString(d.StreamName[:])
		// ":name:$DATA"; "::$DATA" is the file's content.
		if n := strings.TrimSuffix(strings.TrimPrefix(name, ":"), ":$DATA"); n != "" {
			data, err := os.ReadFile(p + ":" + n)
			if err != nil {
				return nil, fmt.Errorf("stream %s: %w", n, err)
			}
			out[n] = hash(data)
		}
		r, _, _ := findNextStreamW.Call(h, uintptr(unsafe.Pointer(&d)))
		if r == 0 {
			break
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func checkPlatform(want, got Item) []string {
	w, g := want.Windows, got.Windows
	if w == nil {
		return nil
	}
	var diffs []string
	add := func(field, want, got string) {
		diffs = append(diffs, fmt.Sprintf("%s: want %s, got %s", field, want, got))
	}
	if w.Attributes != "" && showAttrs(parseAttrs(w.Attributes)) != g.Attributes {
		add("attributes", showAttrs(parseAttrs(w.Attributes)), g.Attributes)
	}
	if w.SDDL != "" {
		if sd, err := windows.SecurityDescriptorFromString(w.SDDL); err == nil && sd.String() != g.SDDL {
			add("sddl", sd.String(), g.SDDL)
		}
	}
	var names []string
	for k := range w.Streams {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if want := hash([]byte(w.Streams[k])); g.Streams[k] != want {
			add("stream "+k, want, orNone(g.Streams[k]))
		}
	}
	return diffs
}
