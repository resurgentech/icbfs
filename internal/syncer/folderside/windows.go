//go:build windows

package folderside

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/resurgentech/icbfs/internal/syncer"
)

const aclsSupported = true

// probe asks the volume what it keeps: NTFS and ReFS have stable file
// ids and persistent ACLs, to 100ns; anything else (a WebDAV drive, a
// FAT stick) gets names, sizes and times to the second.
func probe(root string) syncer.Caps {
	caps := syncer.Caps{TimeResolution: time.Second}
	vol := filepath.VolumeName(root) + `\`
	if strings.HasPrefix(root, `\\`) {
		vol = root
		if !strings.HasSuffix(vol, `\`) {
			vol += `\`
		}
	}
	var flags uint32
	name := make([]uint16, windows.MAX_PATH+1)
	p, err := windows.UTF16PtrFromString(vol)
	if err != nil {
		return caps
	}
	if err := windows.GetVolumeInformation(p, nil, 0, nil, nil, &flags, &name[0], uint32(len(name))); err != nil {
		return caps
	}
	fsName := strings.ToUpper(windows.UTF16ToString(name))
	if fsName == "NTFS" || fsName == "REFS" {
		caps.IDs = true
		caps.ACLs = flags&windows.FILE_PERSISTENT_ACLS != 0
		caps.TimeResolution = 100 * time.Nanosecond
	}
	return caps
}

// fileID is the volume serial and file index: stable across renames on
// NTFS.
func fileID(p string, _ fs.FileInfo) string {
	h, err := openForInfo(p)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return ""
	}
	return fmt.Sprintf("%x:%x%08x", fi.VolumeSerialNumber, fi.FileIndexHigh, fi.FileIndexLow)
}

func openForInfo(p string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	// FILE_FLAG_BACKUP_SEMANTICS opens directories too.
	return windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

const aclInfo = windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION

func readACL(p string) string {
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, aclInfo)
	if err != nil {
		return ""
	}
	return sd.String()
}

func writeACL(p, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	owner, _, _ := sd.Owner()
	group, _, _ := sd.Group()
	dacl, _, _ := sd.DACL()
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if owner != nil {
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	if group != nil {
		info |= windows.GROUP_SECURITY_INFORMATION
	}
	if ctl, _, err := sd.Control(); err == nil && ctl&windows.SE_DACL_PROTECTED != 0 {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, info, owner, group, dacl, nil)
}

// openShared opens for reading without stopping others writing or
// deleting it (an editor holding it open).
func openShared(p string) (io.ReadCloser, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(h), p), nil
}

// replace renames from over to, replacing it.
func replace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
