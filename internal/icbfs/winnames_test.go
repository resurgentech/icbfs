package icbfs

import (
	"errors"
	"strings"
	"testing"
)

func TestIsWindowsReservedName(t *testing.T) {
	cases := []struct {
		name     string
		reserved bool
	}{
		{"readme.txt", false},
		{"My Report (final).docx", false},
		{"a", false},
		{"", true},
		{"con.txt", true}, // reserved base name, case-insensitive, with extension
		{"CON", true},
		{"Con", true},
		{"COM1", true},
		{"com3.log", true},
		{"LPT9", true},
		{"lpt0", false},    // LPT0 is not reserved; only LPT1-9
		{"COM0", false},    // same for COM0
		{"CONTACT", false}, // reserved base-name match is exact, not prefix
		{"a:b", true},      // reserved character
		{"a<b", true},
		{"a>b", true},
		{"a\"b", true},
		{"a/b", true},
		{"a\\b", true},
		{"a|b", true},
		{"a?b", true},
		{"a*b", true},
		{"trailing space ", true},
		{"trailing period.", true},
		{"a\x01b", true}, // control character
		{"a\x1fb", true},
		{"a\x20b", false}, // 0x20 is a plain space, not a control character
	}
	for _, c := range cases {
		if got := isWindowsReservedName(c.name); got != c.reserved {
			t.Errorf("isWindowsReservedName(%q) = %v, want %v", c.name, got, c.reserved)
		}
	}
}

func TestCheckNamePrimaryWindowsVsPosix(t *testing.T) {
	winFS := &Filesystem{primaryWindows: true}
	if err := winFS.checkName("CON"); err != ErrInvalidName {
		t.Fatalf("primary-Windows checkName(CON) = %v, want ErrInvalidName", err)
	}
	if err := winFS.checkName("readme.txt"); err != nil {
		t.Fatalf("primary-Windows checkName(readme.txt) = %v, want nil", err)
	}

	posixFS := &Filesystem{primaryWindows: false}
	if err := posixFS.checkName("CON"); err != nil {
		t.Fatalf("primary-POSIX checkName(CON) = %v, want nil", err)
	}
}

func TestCheckNameLength(t *testing.T) {
	cjk := func(n int) string { return strings.Repeat("名", n) }   // 3 UTF-8 bytes, 1 UTF-16 unit
	emoji := func(n int) string { return strings.Repeat("😀", n) } // 4 UTF-8 bytes, 2 UTF-16 units
	ascii := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name          string
		posix, window bool // whether it's accepted
	}{
		{ascii(255), true, true},
		{ascii(256), false, false},
		{cjk(85), true, true},    // 255 bytes
		{cjk(86), false, true},   // 258 bytes, 86 units
		{cjk(255), false, true},  // 765 bytes: legal on NTFS
		{cjk(256), false, false}, // 256 units
		{emoji(63) + "abc", true, true},
		{emoji(127), false, true}, // 254 units
		{emoji(128), false, false},
	}
	for _, tc := range cases {
		for _, primaryWindows := range []bool{false, true} {
			f := &Filesystem{primaryWindows: primaryWindows}
			want := tc.posix
			if primaryWindows {
				want = tc.window
			}
			err := f.checkNameLength(tc.name)
			if (err == nil) != want {
				t.Errorf("primaryWindows=%v, %d bytes / %d units: err = %v, want accepted=%v",
					primaryWindows, len(tc.name), utf16Len(tc.name), err, want)
			}
			if err != nil && !errors.Is(err, ErrNameTooLong) {
				t.Errorf("err = %v, want ErrNameTooLong", err)
			}
		}
	}
}
