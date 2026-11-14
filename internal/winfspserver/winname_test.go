package winfspserver

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

// substituteSet is every code point the mapping can produce, plus the
// marker U+F000 (T164): U+F001-U+F01F, U+F020, U+F022, U+F02A, U+F02E,
// U+F03A, U+F03C, U+F03E, U+F03F, U+F05C, U+F07C. No device-name code
// points: T163 found CON, NUL.txt, com1 and LPT1-superscript open as they are.
func substituteSet() []rune {
	var set []rune
	set = append(set, 0xF000)
	for r := rune(0xF001); r <= 0xF01F; r++ {
		set = append(set, r)
	}
	return append(set, 0xF020, 0xF022, 0xF02A, 0xF02E, 0xF03A, 0xF03C, 0xF03E, 0xF03F, 0xF05C, 0xF07C)
}

func units(s string) int { return len(utf16.Encode([]rune(s))) }

func TestWinShownEachRule(t *testing.T) {
	cases := []struct{ name, stored, shown string }{
		{"plain", "plain.txt", "plain.txt"},
		{"lt", "a<b", "a\uF03Cb"},
		{"gt", "a>b", "a\uF03Eb"},
		{"colon", "a:b", "a\uF03Ab"},
		{"quote", `a"b`, "a\uF022b"},
		{"pipe", "a|b", "a\uF07Cb"},
		{"qmark", "a?b", "a\uF03Fb"},
		{"star", "a*b", "a\uF02Ab"},
		{"backslash", `a\b`, "a\uF05Cb"},
		{"tab", "a\tb", "a\uF009b"},
		{"ctl01", "a\x01b", "a\uF001b"},
		{"ctl1f", "a\x1fb", "a\uF01Fb"},
		{"trailing dot", "a.", "a\uF02E"},
		{"two trailing dots", "a..", "a.\uF02E"},
		{"trailing space", "a ", "a\uF020"},
		{"trailing dot then space", "a. ", "a.\uF020"},
		{"inner dot and space kept", "a. b", "a. b"},
		{"only a space", " ", "\uF020"},
		{"leading dot", ".a", ".a"},
		// Device names are left as they are (T163).
		{"CON", "CON", "CON"},
		{"NUL.txt", "NUL.txt", "NUL.txt"},
		{"com1", "com1", "com1"},
		{"LPT1 superscript", "LPT\u00B9", "LPT\u00B9"},
		// A stored substitute is escaped with the marker, never hidden.
		{"stored F000", "a\uF000b", "a\uF000\uF000b"},
		{"stored F03A", "a\uF03Ab", "a\uF000\uF03Ab"},
		{"stored F020 last", "a\uF020", "a\uF000\uF020"},
		// Private-use outside the set passes through.
		{"trailing F028", "a\uF028", "a\uF028"},
		{"F040", "a\uF040b", "a\uF040b"},
		{"non-BMP", "a\U0001F600b", "a\U0001F600b"},
	}
	for _, c := range cases {
		if got := winShown(c.stored); got != c.shown {
			t.Errorf("%s: winShown(%q) = %q, want %q", c.name, c.stored, got, c.shown)
		}
		back, err := winStored(c.shown)
		if err != nil || back != c.stored {
			t.Errorf("%s: winStored(%q) = %q, %v, want %q", c.name, c.shown, back, err, c.stored)
		}
	}
}

func TestWinStoredTypedInWindows(t *testing.T) {
	// A name typed in Windows with a substitute is stored with the raw
	// character; Linux and the API see it.
	got, err := winStored("a\uF03Ab")
	if err != nil || got != "a:b" {
		t.Errorf("winStored(a+F03A+b) = %q, %v, want a:b", got, err)
	}
	// A raw trailing dot or space (a \\?\ path) is stored as typed.
	for _, s := range []string{"a.", "a ", "a:b", "a<b"} {
		got, err := winStored(s)
		if err != nil || got != s {
			t.Errorf("winStored(%q) = %q, %v, want it unchanged", s, got, err)
		}
	}
}

func TestWinRoundTripEverySubstituteCodePoint(t *testing.T) {
	for _, r := range substituteSet() {
		stored := "x" + string(r) + "y"
		shown := winShown(stored)
		if shown != "x\uF000"+string(r)+"y" {
			t.Errorf("U+%04X stored: shown %q, want the marker before it", r, shown)
		}
		back, err := winStored(shown)
		if err != nil || back != stored {
			t.Errorf("U+%04X stored round trip: %q, %v", r, back, err)
		}
		// Last in the name, too.
		stored = "x" + string(r)
		back, err = winStored(winShown(stored))
		if err != nil || back != stored {
			t.Errorf("U+%04X last, round trip: %q, %v", r, back, err)
		}
	}
	// And every forbidden character, in both directions.
	forbidden := []rune{'<', '>', ':', '"', '|', '?', '*', '\\'}
	for r := rune(1); r <= 0x1F; r++ {
		forbidden = append(forbidden, r)
	}
	for _, r := range forbidden {
		stored := "x" + string(r) + "y"
		shown := winShown(stored)
		if want := "x" + string(r+0xF000) + "y"; shown != want {
			t.Errorf("%q: shown %q, want %q", stored, shown, want)
		}
		if back, err := winStored(shown); err != nil || back != stored {
			t.Errorf("%q round trip: %q, %v", stored, back, err)
		}
	}
}

func TestWinOneToOne(t *testing.T) {
	// a:b and a name with a literal U+F03A are two entries with two
	// different shown names, and each is turned back to its own.
	colon, lit := "a:b", "a\uF03Ab"
	sc, sl := winShown(colon), winShown(lit)
	if sc == sl {
		t.Fatalf("a:b and the literal U+F03A name both show as %q", sc)
	}
	if sc != "a\uF03Ab" || sl != "a\uF000\uF03Ab" {
		t.Errorf("shown names %q and %q", sc, sl)
	}
	if b, err := winStored(sc); err != nil || b != colon {
		t.Errorf("winStored(%q) = %q, %v", sc, b, err)
	}
	if b, err := winStored(sl); err != nil || b != lit {
		t.Errorf("winStored(%q) = %q, %v", sl, b, err)
	}
}

func TestWinStoredRefusesBadMarker(t *testing.T) {
	for _, shown := range []string{
		"a\uF000",       // marker last
		"\uF000",        // marker alone
		"a\uF000b",      // marker, then a code point outside the set
		"a\uF000\uF028", // marker, then private-use outside the set
		"a\uF000.",      // marker, then a dot
		"a\uF000\U0001F600",
	} {
		_, err := winStored(shown)
		if !errors.Is(err, icbfs.ErrInvalidName) {
			t.Errorf("winStored(%q) = %v, want ErrInvalidName", shown, err)
		}
	}
}

func TestWinCaseFolding(t *testing.T) {
	// Mapped characters and their substitutes have no case, so the
	// translation of a name and of its upper-cased form agree: translating
	// before the folded lookup changes nothing about the fold.
	for _, stored := range []string{"Report:v2.DOCX", "a<B>c", "Trail.", "mixed CASE "} {
		shown := winShown(stored)
		up := strings.ToUpper(shown)
		backUp, err := winStored(up)
		if err != nil {
			t.Fatalf("winStored(%q): %v", up, err)
		}
		if !strings.EqualFold(backUp, stored) {
			t.Errorf("fold of %q: translated back %q is not a fold of %q", up, backUp, stored)
		}
		if strings.ToUpper(winShown(strings.ToUpper(stored))) != up {
			t.Errorf("translation does not commute with case for %q", stored)
		}
	}
}

func TestWinNameLength(t *testing.T) {
	// 255 bytes full of ':' show as 255 units, and back to 255 bytes: a
	// shown name is never longer in UTF-16 units than its stored name is
	// in bytes.
	s255 := strings.Repeat(":", 255)
	shown := winShown(s255)
	if units(shown) != 255 {
		t.Errorf("255 colons show as %d units", units(shown))
	}
	if back, err := winStored(shown); err != nil || len(back) != 255 || back != s255 {
		t.Errorf("255 colons back: len %d, %v", len(back), err)
	}
	// 256 colons come back as 256 bytes: the core refuses them as today.
	s256 := strings.Repeat("\uF03A", 256)
	if back, err := winStored(s256); err != nil || len(back) != 256 {
		t.Errorf("256 shown colons: len %d, %v", len(back), err)
	}
	// Escaped code points: three bytes stored, two units shown.
	esc := strings.Repeat("\uF03A", 85) // 255 bytes stored
	if len(esc) != 255 {
		t.Fatalf("fixture is %d bytes", len(esc))
	}
	if u := units(winShown(esc)); u != 170 || u > len(esc) {
		t.Errorf("85 escaped code points show as %d units", u)
	}
	// The bound over every kind of character.
	mixed := "a:b\uF03A\uF000 .\U0001F600\t\\" + "z."
	if units(winShown(mixed)) > len(mixed) {
		t.Errorf("%q shows longer (%d units) than stored (%d bytes)", mixed, units(winShown(mixed)), len(mixed))
	}
}

func TestWinPaths(t *testing.T) {
	// Each component alone; the separator is the only raw backslash.
	if got := winShownPath(`\a:b\c.`); got != "\\a\uF03Ab\\c\uF02E" {
		t.Errorf("winShownPath = %q", got)
	}
	got, err := winStoredPath("\\a\uF03Ab\\c\uF02E")
	if err != nil || got != `\a:b\c.` {
		t.Errorf("winStoredPath = %q, %v", got, err)
	}
	if _, err := winStoredPath("\\a\\b\uF000"); !errors.Is(err, icbfs.ErrInvalidName) {
		t.Errorf("winStoredPath of a bad marker = %v", err)
	}
	if got := winShownPath(`\`); got != `\` {
		t.Errorf("root = %q", got)
	}
	// A stored backslash in a name is not a separator once shown.
	if p, err := winStoredPath("\\a\uF05Cb"); err != nil || p != "\\a\\b" {
		t.Errorf("winStoredPath(a+F05C+b) = %q, %v", p, err)
	}
}

func TestWinSymlinkTargets(t *testing.T) {
	cases := []struct{ name, stored, shown string }{
		{"absolute with drive", `\??\M:\dir\a:b`, "\\??\\M:\\dir\\a\uF03Ab"},
		{"volume path", `\dir\a<b`, "\\dir\\a\uF03Cb"},
		{"relative", `..\a:b\c.`, "..\\a\uF03Ab\\c\uF02E"},
		{"plain", `x\y`, `x\y`},
		{"relative colon", `a:b`, "a\uF03Ab"},
		{"stored marker escaped", "\\d\\a\uF03Ab", "\\d\\a\uF000\uF03Ab"},
	}
	for _, c := range cases {
		if got := winShownTarget(c.stored); got != c.shown {
			t.Errorf("%s: winShownTarget(%q) = %q, want %q", c.name, c.stored, got, c.shown)
		}
		back, err := winStoredTarget(c.shown)
		if err != nil || back != c.stored {
			t.Errorf("%s: winStoredTarget(%q) = %q, %v, want %q", c.name, c.shown, back, err, c.stored)
		}
	}
	if _, err := winStoredTarget("\\??\\M:\\a\uF000"); !errors.Is(err, icbfs.ErrInvalidName) {
		t.Errorf("a bad marker in a target = %v", err)
	}
}
