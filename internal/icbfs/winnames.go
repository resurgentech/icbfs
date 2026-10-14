package icbfs

import "strings"

// windowsReservedChars are the characters Win32 forbids in a single
// path component, per Microsoft's own documented naming rules —
// docs/architecture.md's "Windows compatibility: primary mode" section.
// `/` and `\` are included for
// completeness even though neither can reach this check in practice
// (every caller here operates on a single already-split path
// component, never a multi-segment string).
const windowsReservedChars = `<>:"/\|?*`

// windowsReservedBaseNames are reserved regardless of extension — a
// real Windows filesystem refuses "COM1.txt" exactly as it refuses
// "COM1", matched case-insensitively.
var windowsReservedBaseNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isWindowsReservedName reports whether name would be refused outright
// by a real Windows filesystem: a reserved character, a control
// character, a trailing space or period, or a reserved base name
// (with or without an extension). Pure string logic, no Windows
// dependency at all — deliberately portable and testable without a
// VM, same reasoning internal/winfspserver/resolve.go's own doc
// comment gives for keeping path-resolution logic off the Windows-only
// build tag.
func isWindowsReservedName(name string) bool {
	if name == "" {
		return true
	}
	for _, c := range name {
		if c < 0x20 || strings.ContainsRune(windowsReservedChars, c) {
			return true
		}
	}
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return true
	}
	base := name
	if idx := strings.IndexByte(name, '.'); idx >= 0 {
		base = name[:idx]
	}
	return windowsReservedBaseNames[strings.ToUpper(base)]
}

// checkName refuses a reserved name outright on a primary-Windows
// filesystem — docs/architecture.md's "Reserved names/characters" rule,
// enforced here at the core Filesystem layer (not just the WinFsp
// driver) so it applies regardless of which access layer is writing:
// primary mode is a property of the filesystem itself, not of
// whichever driver happens to be mounting it right now. A
// primary-POSIX filesystem never rejects anything here: any
// POSIX-legal name is allowed, Windows-reserved or not.
//
// It also enforces the name length limit; see checkNameLength.
func (f *Filesystem) checkName(name string) error {
	if err := f.checkNameLength(name); err != nil {
		return err
	}
	if f.primaryWindows && isWindowsReservedName(name) {
		return ErrInvalidName
	}
	return nil
}

// maxNameLen is the longest name a directory entry can have: 255 bytes
// on a primary-POSIX filesystem, as on ext4, XFS, Btrfs, F2FS, tmpfs and
// ZFS, and 255 UTF-16 code units on a primary-Windows one, as on NTFS
// (and Linux's own NTFS and exFAT drivers). Every name within the POSIX
// limit is within the Windows one too, since no UTF-16 code unit takes
// less than one UTF-8 byte.
const maxNameLen = 255

// checkNameLength refuses a name longer than maxNameLen in this
// filesystem's unit. Like checkName, it's a property of the filesystem,
// whichever access layer is asking — and it applies to lookups too, so a
// too-long name is "name too long" (ENAMETOOLONG), not "not found".
func (f *Filesystem) checkNameLength(name string) error {
	n := len(name)
	if f.primaryWindows {
		n = utf16Len(name)
	}
	if n > maxNameLen {
		return ErrNameTooLong
	}
	return nil
}

// utf16Len is name's length in UTF-16 code units: two for a character
// outside the Basic Multilingual Plane, one for anything else (including
// a byte that isn't valid UTF-8, which Windows can't produce anyway).
func utf16Len(name string) int {
	n := 0
	for _, r := range name {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}
