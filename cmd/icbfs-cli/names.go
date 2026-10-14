package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
)

func namesCreate(args []string) error {
	fset := flag.NewFlagSet("names create", flag.ExitOnError)
	sf := addStoreFlags(fset)
	fsName := fset.String("fs", "", "filesystem (required)")
	name := fset.String("name", "", "the version's name (required)")
	at := fset.String("at", "", "name this past moment instead of now (a date, a local date-time, or RFC 3339 — as -as-of takes)")
	scope := fset.String("scope", "", "the subtree it's for (a path; default the whole filesystem)")
	if err := parse(fset, args); err != nil {
		return err
	}
	var t time.Time
	if *at != "" {
		var err error
		if t, err = icbfs.ParseAsOf(*at, time.Local); err != nil {
			return err
		}
	}
	nv, err := icbfs.CreateName(ctx(), sf.store(), *fsName, icbfs.NamedVersion{Name: *name, Scope: *scope, CreatedBy: actor()}, t)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", nv.Name, nv.Time.Local().Format(time.RFC3339Nano))
	return nil
}

func namesList(args []string) error {
	fset := flag.NewFlagSet("names list", flag.ExitOnError)
	sf := addStoreFlags(fset)
	fsName := fset.String("fs", "", "filesystem (required)")
	asJSON := fset.Bool("json", false, "JSON output")
	if err := parse(fset, args); err != nil {
		return err
	}
	names, err := icbfs.ListNames(ctx(), sf.store(), *fsName)
	if err != nil {
		return err
	}
	if *asJSON {
		if names == nil {
			names = []icbfs.NamedVersion{}
		}
		return json.NewEncoder(os.Stdout).Encode(names)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTIME\tSCOPE\tBY")
	for _, nv := range names {
		scope := nv.Scope
		if scope == "" {
			scope = "(all)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", nv.Name, nv.Time.Local().Format(time.RFC3339), scope, nv.CreatedBy)
	}
	return w.Flush()
}

func namesDelete(args []string) error {
	fset := flag.NewFlagSet("names delete", flag.ExitOnError)
	sf := addStoreFlags(fset)
	fsName := fset.String("fs", "", "filesystem (required)")
	name := fset.String("name", "", "the version's name (required)")
	if err := parse(fset, args); err != nil {
		return err
	}
	return icbfs.DeleteName(ctx(), sf.store(), *fsName, *name)
}

func trashList(args []string) error {
	fset := flag.NewFlagSet("trash list", flag.ExitOnError)
	sf := addStoreFlags(fset)
	fsName := fset.String("fs", "", "filesystem (required)")
	path := fset.String("path", "", "only what was deleted from under this path")
	limit := fset.Int("limit", 0, "at most this many (newest first)")
	asJSON := fset.Bool("json", false, "JSON output")
	if err := parse(fset, args); err != nil {
		return err
	}
	f := icbfs.New(sf.store(), *fsName)
	if err := f.Attach(ctx()); err != nil {
		return err
	}
	entries, err := f.ListTrash(ctx(), icbfs.TrashOptions{PathPrefix: *path, Limit: *limit})
	if err != nil {
		return err
	}
	if *asJSON {
		if entries == nil {
			entries = []icbfs.TrashEntry{}
		}
		return json.NewEncoder(os.Stdout).Encode(entries)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tDELETED\tBY\tSIZE\tPATH")
	for _, e := range entries {
		p := e.Path
		if p == "" {
			p = e.Name + " (in " + e.ParentID + ")"
		}
		if e.Type == icbfs.TypeDir {
			p += "/"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", e.EntryID, e.DeletedAt.Local().Format(time.RFC3339), e.DeletedBy, e.Size, p)
	}
	return w.Flush()
}

func trashRestore(args []string) error {
	fset := flag.NewFlagSet("trash restore", flag.ExitOnError)
	sf := addStoreFlags(fset)
	fsName := fset.String("fs", "", "filesystem (required)")
	id := fset.String("id", "", "the trash entry (required; from trash list)")
	as := fset.String("as", "", "restore under this name instead (same directory)")
	replace := fset.Bool("replace", false, "move whatever has the name now to the trash first")
	if err := parse(fset, args); err != nil {
		return err
	}
	f := icbfs.New(sf.store(), *fsName)
	if err := f.Attach(ctx()); err != nil {
		return err
	}
	e, err := f.Restore(ctx(), *id, icbfs.RestoreOptions{ToName: *as, Replace: *replace})
	if err != nil {
		return err
	}
	fmt.Printf("restored %s\n", e.Name)
	return nil
}

// actor is who this command acts as, for the records it leaves.
func actor() string {
	if u := os.Getenv("USER"); u != "" {
		return "cli:" + u
	}
	return fmt.Sprintf("uid:%d", os.Getuid())
}
