package drive

import (
	"errors"
	"testing"
)

// TestPathRule is the one rule for a path (T82-14): "/" separates, a
// backslash is a name's character, and ".", ".." or an empty name is
// refused, not tidied.
func TestPathRule(t *testing.T) {
	ok := map[string]string{
		"":                   "/",
		"/":                  "/",
		"a":                  "/a",
		"/a/b":               "/a/b",
		`/..\B\contract.txt`: `/..\B\contract.txt`, // one name
		`\`:                  `/\`,
		`/a\..\..\b`:         `/a\..\..\b`,
		"/...":               "/...",
		"/.x":                "/.x",
	}
	for in, want := range ok {
		got, err := CleanPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "/..", "/../B", "/a/../b", "/a/./b", ".", "/a//b", "//a", "/a/", "a/", `..\/../x`, `/a\/../b`} {
		if got, err := CleanPath(in); !errors.Is(err, ErrInvalidName) {
			t.Errorf("CleanPath(%q) = %q, %v; want ErrInvalidName", in, got, err)
		}
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct{ root, p, want string }{
		{"/workdirs/w1", "/", "/workdirs/w1"},
		{"/workdirs/w1", "", "/workdirs/w1"},
		{"/workdirs/w1", "/a/b", "/workdirs/w1/a/b"},
		{"/workdirs/w1", `/..\w2\mine.txt`, `/workdirs/w1/..\w2\mine.txt`},
		{"", "/a", "/a"},
		{"/", "a", "/a"},
	} {
		got, err := Within(c.root, c.p)
		if err != nil || got != c.want {
			t.Errorf("Within(%q, %q) = %q, %v; want %q", c.root, c.p, got, err, c.want)
		}
		if err == nil && !Under(c.root, got) {
			t.Errorf("Within(%q, %q) = %q is not Under its root", c.root, c.p, got)
		}
	}
	for _, c := range []struct{ root, p string }{
		{"/workdirs/w1", "/../w2/mine.txt"},
		{"/workdirs/w1", ".."},
		{"/workdirs/w1", "/a//b"},
		{"/workdirs/../x", "/a"},
	} {
		if got, err := Within(c.root, c.p); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Within(%q, %q) = %q, %v; want ErrInvalidName", c.root, c.p, got, err)
		}
	}
}

func TestCheckNewName(t *testing.T) {
	for _, n := range []string{"a.txt", "..x", "x..", "a b", "é"} {
		if err := CheckNewName(n); err != nil {
			t.Errorf("CheckNewName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", ".", "..", "a/b", `a\b`, `\`, `..\B`} {
		if err := CheckNewName(n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("CheckNewName(%q) = %v; want ErrInvalidName", n, err)
		}
	}
	// An existing name with a backslash is still a path's name.
	if err := CheckPathName(`a\b`); err != nil {
		t.Errorf("CheckPathName(a\\b) = %v", err)
	}
}

func TestUnder(t *testing.T) {
	if Under("/a", "/ab") || !Under("/a", "/a") || !Under("/a", "/a/b") || !Under("/", "/x") {
		t.Fatal("Under")
	}
}
