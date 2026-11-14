// Command namesfixture fills a primary-POSIX icbfs filesystem with names
// Windows can't hold, through internal/icbfs (the core allows any
// POSIX-legal name there), for run-names-test.ps1 to look at through a
// Windows mount. No Linux mount is needed.
//
//	namesfixture -fs NAME [store flags]
//
// It makes two trees, A and B, each holding one file per name, and a
// folder F holding one folder per name, and in A a symlink "lnk" to the
// colon name `a:b`. The names are the ones in Names; run-names-test.ps1
// builds the same list.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/storeflags"
)

// Name is one case: an id for the table, and the stored name.
type Name struct{ ID, Name string }

// Names is every case. Kept in step with run-names-test.ps1.
func Names() []Name {
	return []Name{
		{"plain", "plain"},
		{"lt", "a<b"},
		{"gt", "a>b"},
		{"colon", "a:b"},
		{"quote", `a"b`},
		{"pipe", "a|b"},
		{"qmark", "a?b"},
		{"star", "a*b"},
		{"backslash", `a\b`},
		{"tab", "a\tb"},
		{"ctl01", "a\x01b"},
		{"trailing-dot", "a."},
		{"trailing-space", "a "},
		{"CON", "CON"},
		{"NUL.txt", "NUL.txt"},
		{"com1", "com1"},
		{"LPT1-sup", "LPT¹"},
		{"holds-F03A", "ab"},
		{"colons255", strings.Repeat(":", 255)},
		{"case", "Mix:Case"},
	}
}

func main() {
	fset := flag.NewFlagSet("namesfixture", flag.ExitOnError)
	sf := storeflags.Default()
	sf.AddFlags(fset)
	fsName := fset.String("fs", "", "the filesystem (created already, without -primary-windows)")
	list := fset.String("list", "", "print what is stored under this /-separated path, and exit")
	fset.Parse(os.Args[1:])
	if *fsName == "" {
		fmt.Fprintln(os.Stderr, "-fs is required")
		os.Exit(2)
	}
	ctx := context.Background()
	store, err := sf.Open(ctx)
	if err != nil {
		die(err)
	}
	f := icbfs.New(store, *fsName)
	if err := f.Attach(ctx); err != nil {
		die(err)
	}
	if *list != "" {
		listStored(ctx, f, *list)
		return
	}
	if f.PrimaryWindows() {
		die(fmt.Errorf("%s is primary-Windows; this needs a primary-POSIX filesystem", *fsName))
	}
	for _, tree := range []string{"A", "B"} {
		dir, _, err := f.Mkdir(ctx, f.RootKey(), tree, 0755, 0, 0)
		if err != nil {
			die(err)
		}
		fdir, _, err := f.Mkdir(ctx, dir, "F", 0755, 0, 0)
		if err != nil {
			die(err)
		}
		if tree == "A" {
			if _, _, err := f.Symlink(ctx, dir, "lnk", "a:b", 0, 0); err != nil {
				die(fmt.Errorf("symlink A/lnk: %w", err))
			}
		}
		for _, n := range Names() {
			content := []byte("file " + tree + " " + n.ID)
			if _, _, err := f.CreateWith(ctx, dir, n.Name, 0644, 0, 0, content, nil, icbfs.ItemInfo{}); err != nil {
				die(fmt.Errorf("file %s/%q: %w", tree, n.Name, err))
			}
			if _, _, err := f.Mkdir(ctx, fdir, n.Name, 0755, 0, 0); err != nil {
				die(fmt.Errorf("folder %s/F/%q: %w", tree, n.Name, err))
			}
		}
	}
	fmt.Printf("namesfixture: %d names x (file, folder) x trees A, B in %s\n", len(Names()), *fsName)
}

// listStored prints the entries of the directory at path.
func listStored(ctx context.Context, f *icbfs.Filesystem, path string) {
	key := f.RootKey()
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		e, _, err := f.Lookup(ctx, key, part)
		if err != nil {
			die(fmt.Errorf("lookup %q: %w", part, err))
		}
		key = e.UUID
	}
	entries, err := f.ReadDir(ctx, key)
	if err != nil {
		die(err)
	}
	for _, e := range entries {
		kind, target := "file", ""
		switch e.Type {
		case icbfs.TypeDir:
			kind = "dir"
		case icbfs.TypeSymlink:
			kind = "symlink"
			t, err := f.Readlink(ctx, e.UUID)
			if err != nil {
				die(err)
			}
			target = hex.EncodeToString([]byte(t))
		}
		fmt.Printf("%s\t%s\t%s\n", hex.EncodeToString([]byte(e.Name)), kind, target)
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "namesfixture:", err)
	os.Exit(1)
}
