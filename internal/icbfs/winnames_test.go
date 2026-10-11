package icbfs

import "testing"

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
