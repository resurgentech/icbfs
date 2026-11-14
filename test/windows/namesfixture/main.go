// Command namesfixture fills a primary-POSIX icbfs filesystem with names
// Windows can't hold, through internal/icbfs (the core allows any
// POSIX-legal name there), for run-names-test.ps1 to look at through a
// Windows mount. No Linux mount is needed.
//
//	namesfixture -fs NAME [store flags]
//
// It makes two trees, A and B, each holding one file per name, and a
// folder F holding one folder per name. The names are the ones in
// Names; run-names-test.ps1 builds the same list.
package main

import (
	"context"
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
	}
}

func main() {
	fset := flag.NewFlagSet("namesfixture", flag.ExitOnError)
	sf := storeflags.Default()
	sf.AddFlags(fset)
	fsName := fset.String("fs", "", "the filesystem (created already, without -primary-windows)")
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

func die(err error) {
	fmt.Fprintln(os.Stderr, "namesfixture:", err)
	os.Exit(1)
}
