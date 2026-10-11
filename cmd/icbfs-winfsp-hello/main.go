//go:build windows

// Command icbfs-winfsp-hello is ROADMAP.md's Part F, task F1's "trivial
// hello world WinFsp mount" proof — not part of the real driver
// architecture (that's F2/F3's internal/winfspserver, built against
// icbfs.Filesystem via a real path-resolution layer). It exists to
// prove the chosen binding (github.com/winfsp/go-winfsp, used directly
// against its native Behaviour* interfaces — the layer the real driver
// needs, since the higher-level gofs wrapper has no SetSecurity/
// SetReparsePoint/attribute control for F7-F9) actually mounts and
// serves real file content against a real WinFsp install.
//
// Built and run on the Windows test VM (test/windows/README.md), per
// the standing build-on-the-VM workflow.
//
// Two real, non-obvious requirements found the hard way, each verified
// against the actual source rather than guessed — both are load-
// bearing for the real driver too, not quirks of this smoke test:
//
//  1. WinFsp's own fsop.c (FspFileSystemOpCreate) returns
//     STATUS_INVALID_DEVICE_REQUEST — surfaced to Win32 as "Incorrect
//     function" — for EVERY create/open unless Create (or CreateEx),
//     Open, AND Overwrite (or OverwriteEx) are all non-NULL. A
//     filesystem that only wires Open never gets a single callback
//     invoked; the mount itself still reports success, which is what
//     made this hard to see. A read-only filesystem can simply refuse
//     Create/Overwrite, but they must be wired.
//  2. The security descriptor handed back from GetSecurityByName/
//     GetSecurity must carry an Owner and Group, not just a DACL —
//     the kernel's access check on a file open fails with
//     STATUS_INVALID_SECURITY_DESCR ("The security descriptor
//     structure is invalid") otherwise. Directory listing got away
//     with a DACL-only SD; a file open did not.
//
// Presents a fixed, read-only, two-entry filesystem: the root
// directory and one file, \hello.txt, containing a static message.
// Usage: icbfs-winfsp-hello.exe <mountpoint, e.g. J:>
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	winfsp "github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"
)

const helloContent = "Hello from icbfs on WinFsp!\r\n"

// mountDuration: run under Start-Process with redirected output,
// stdin is closed, so blocking on it exits almost immediately instead
// of staying mounted; a fixed sleep is simpler and more robust for
// this throwaway smoke test than reconnecting stdin.
const mountDuration = 60 * time.Second

// Fixed file contexts: this filesystem only ever has two things, so
// there's no need for a real handle table.
const (
	rootHandle  = uintptr(1)
	helloHandle = uintptr(2)
)

// rootDirBuf is the one directory buffer this filesystem ever needs.
// WinFsp requires the buffer returned from GetOrNewDirBuffer to persist
// across calls for the same open directory (marker-based continuation
// reads back out of it) — returning a fresh zero buffer per call leaks
// the native allocation every time and breaks continuation.
var rootDirBuf winfsp.DirBuffer

type helloFS struct{}

var (
	_ winfsp.BehaviourBase              = helloFS{}
	_ winfsp.BehaviourCreate            = helloFS{}
	_ winfsp.BehaviourOverwrite         = helloFS{}
	_ winfsp.BehaviourGetFileInfo       = helloFS{}
	_ winfsp.BehaviourReadDirectory     = helloFS{}
	_ winfsp.BehaviourRead              = helloFS{}
	_ winfsp.BehaviourGetSecurityByName = helloFS{}
	_ winfsp.BehaviourGetSecurity       = helloFS{}
	_ winfsp.BehaviourGetVolumeInfo     = helloFS{}
)

// permissiveSD grants Everyone full control, owned by Builtin
// Administrators. This smoke test has nothing worth protecting and no
// uid/gid model to map yet (F8's job) — the Owner/Group are there
// because the kernel refuses an access check without them (see the
// package doc comment), not as a real ownership decision.
func permissiveSD() (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;FA;;;WD)")
}

func normalize(name string) string {
	name = strings.ReplaceAll(name, "/", `\`)
	if name == "" {
		return `\`
	}
	return name
}

func (helloFS) Open(fs *winfsp.FileSystemRef, name string, createOptions, grantedAccess uint32, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	switch normalize(name) {
	case `\`:
		fillInfo(info, true, 0)
		return rootHandle, nil
	case `\hello.txt`:
		fillInfo(info, false, uint64(len(helloContent)))
		return helloHandle, nil
	default:
		return 0, os.ErrNotExist
	}
}

func (helloFS) Close(fs *winfsp.FileSystemRef, file uintptr) {}

func (helloFS) Create(fs *winfsp.FileSystemRef, name string, createOptions, grantedAccess, fileAttributes uint32, securityDescriptor *windows.SECURITY_DESCRIPTOR, allocationSize uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (uintptr, error) {
	return 0, os.ErrPermission
}

func (helloFS) Overwrite(fs *winfsp.FileSystemRef, file uintptr, attributes uint32, replaceAttributes bool, allocationSize uint64, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	return os.ErrPermission
}

func (helloFS) GetFileInfo(fs *winfsp.FileSystemRef, file uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) error {
	switch file {
	case rootHandle:
		fillInfo(info, true, 0)
		return nil
	case helloHandle:
		fillInfo(info, false, uint64(len(helloContent)))
		return nil
	default:
		return os.ErrNotExist
	}
}

func fillInfo(info *winfsp.FSP_FSCTL_FILE_INFO, isDir bool, size uint64) {
	if isDir {
		info.FileAttributes = windows.FILE_ATTRIBUTE_DIRECTORY
	} else {
		info.FileAttributes = windows.FILE_ATTRIBUTE_READONLY
		info.FileSize = size
		info.AllocationSize = size
	}
}

func (helloFS) GetSecurityByName(fs *winfsp.FileSystemRef, name string, flags winfsp.GetSecurityByNameFlags) (uint32, *windows.SECURITY_DESCRIPTOR, error) {
	var attrs uint32
	switch normalize(name) {
	case `\`:
		attrs = windows.FILE_ATTRIBUTE_DIRECTORY
	case `\hello.txt`:
		attrs = windows.FILE_ATTRIBUTE_READONLY
	default:
		return 0, nil, os.ErrNotExist
	}
	sd, err := permissiveSD()
	return attrs, sd, err
}

func (helloFS) GetSecurity(fs *winfsp.FileSystemRef, file uintptr) (*windows.SECURITY_DESCRIPTOR, error) {
	if file != rootHandle && file != helloHandle {
		return nil, os.ErrNotExist
	}
	return permissiveSD()
}

func (helloFS) GetVolumeInfo(fs *winfsp.FileSystemRef, info *winfsp.FSP_FSCTL_VOLUME_INFO) error {
	info.TotalSize = 1 << 20
	info.FreeSize = 1 << 19
	label, err := windows.UTF16FromString("icbfs-hello")
	if err != nil {
		return err
	}
	copy(info.VolumeLabel[:], label)
	info.VolumeLabelLength = uint16((len(label) - 1) * 2) // bytes, excluding the NUL UTF16FromString appends
	return nil
}

func (helloFS) GetOrNewDirBuffer(fs *winfsp.FileSystemRef, file uintptr) (*winfsp.DirBuffer, error) {
	if file != rootHandle {
		return nil, os.ErrNotExist
	}
	return &rootDirBuf, nil
}

func (helloFS) ReadDirectory(fs *winfsp.FileSystemRef, file uintptr, pattern string, fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error)) error {
	if file != rootHandle {
		return os.ErrNotExist
	}
	var info winfsp.FSP_FSCTL_FILE_INFO
	fillInfo(&info, false, uint64(len(helloContent)))
	_, err := fill("hello.txt", &info)
	return err
}

func (helloFS) Read(fs *winfsp.FileSystemRef, file uintptr, buf []byte, offset uint64) (int, error) {
	if file != helloHandle {
		return 0, os.ErrNotExist
	}
	if offset >= uint64(len(helloContent)) {
		return 0, io.EOF
	}
	return copy(buf, helloContent[offset:]), nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: icbfs-winfsp-hello.exe <mountpoint, e.g. J:>")
		os.Exit(2)
	}
	mountpoint := os.Args[1]

	fsys, err := winfsp.Mount(helloFS{}, mountpoint, winfsp.FileSystemName("icbfs-hello"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mount failed:", err)
		os.Exit(1)
	}
	defer fsys.Unmount()

	fmt.Printf("mounted at %s, staying up for %v\n", mountpoint, mountDuration)
	time.Sleep(mountDuration)
}
