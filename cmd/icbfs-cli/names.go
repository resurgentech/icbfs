package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/pkg/drive"
	"github.com/resurgentech/icbfs/pkg/drive/icbfsdrive"
)

// driveFlags say which drive a names or trash command acts on: through
// the daemon, host's drive id; directly, a filesystem and optionally
// a directory in it.
type driveFlags struct {
	sf           storeFlags
	id, fs, root *string
}

func addDriveFlags(fset *flag.FlagSet) driveFlags {
	return driveFlags{
		sf:   addStoreFlags(fset),
		id:   fset.String("drive", "", "with -daemon: the drive (host's id)"),
		fs:   fset.String("fs", "", "without -daemon: the filesystem"),
		root: fset.String("root", "", "without -daemon: a directory in the filesystem that is the drive (default its root)"),
	}
}

// run runs a drive command, naming the drive as the way it runs needs.
func (df driveFlags) run(name string, args map[string]string, out any) error {
	if *df.sf.daemon != "" {
		if *df.id == "" {
			return errors.New("through the daemon, give -drive (-fs and -root are for running with store credentials)")
		}
		if *df.fs != "" || *df.root != "" {
			return errors.New("through the daemon, the drive says which filesystem and directory: drop -fs and -root")
		}
		args["drive"] = *df.id
	} else {
		if *df.fs == "" {
			return errors.New("give -fs (or, through the daemon, -daemon and -drive)")
		}
		if *df.id != "" {
			return errors.New("-drive is for -daemon: only the server knows what a drive is")
		}
	}
	return df.sf.runWith(name, args, out, func(ctx context.Context, _ map[string]string) (drive.Drive, error) {
		return icbfsdrive.New(ctx, icbfsdrive.Config{Store: df.sf.store(), Filesystem: *df.fs, Root: *df.root})
	})
}

func namesCreate(args []string) error {
	fset := flag.NewFlagSet("names create", flag.ExitOnError)
	df := addDriveFlags(fset)
	name := fset.String("name", "", "the version's name (required)")
	at := fset.String("at", "", "name this past moment instead of now (a date, a local date-time, or RFC 3339 — as -as-of takes)")
	fsWide := fset.Bool("filesystem-wide", false, "for the whole filesystem the drive is on, not just the drive")
	if err := parse(fset, args); err != nil {
		return err
	}
	a := map[string]string{"name": *name, "filesystem-wide": strconv.FormatBool(*fsWide)}
	if *at != "" {
		t, err := icbfs.ParseAsOf(*at, time.Local)
		if err != nil {
			return err
		}
		a["at"] = t.Format(time.RFC3339Nano)
	}
	var n drive.Name
	if err := df.run("names create", a, &n); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", n.Name, n.Time.Local().Format(time.RFC3339Nano))
	return nil
}

func namesList(args []string) error {
	fset := flag.NewFlagSet("names list", flag.ExitOnError)
	df := addDriveFlags(fset)
	asJSON := fset.Bool("json", false, "JSON output")
	if err := parse(fset, args); err != nil {
		return err
	}
	names := []drive.Name{}
	if err := df.run("names list", map[string]string{}, &names); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(names)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTIME\tFOR\tBY")
	for _, n := range names {
		scope := n.Scope
		if n.FilesystemWide {
			scope = "(the whole filesystem)"
		} else if scope == "" || scope == "/" {
			scope = "(the drive)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.Name, n.Time.Local().Format(time.RFC3339), scope, n.CreatedBy)
	}
	return w.Flush()
}

func namesDelete(args []string) error {
	fset := flag.NewFlagSet("names delete", flag.ExitOnError)
	df := addDriveFlags(fset)
	name := fset.String("name", "", "the version's name (required)")
	if err := parse(fset, args); err != nil {
		return err
	}
	return df.run("names delete", map[string]string{"name": *name}, nil)
}

func trashList(args []string) error {
	fset := flag.NewFlagSet("trash list", flag.ExitOnError)
	df := addDriveFlags(fset)
	path := fset.String("path", "", "only what was deleted from under this directory (in the drive)")
	limit := fset.Int("limit", 0, "at most this many (newest first)")
	asJSON := fset.Bool("json", false, "JSON output")
	if err := parse(fset, args); err != nil {
		return err
	}
	var page drive.TrashPage
	if err := df.run("trash list", map[string]string{"path": *path, "limit": strconv.Itoa(*limit)}, &page); err != nil {
		return err
	}
	if *asJSON {
		if page.Entries == nil {
			page.Entries = []drive.TrashEntry{}
		}
		return json.NewEncoder(os.Stdout).Encode(page.Entries)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tDELETED\tBY\tSIZE\tPATH")
	for _, e := range page.Entries {
		p := e.Path
		if p == "" {
			p = e.Name + " (where unknown)"
		}
		if e.Type == drive.TypeDir {
			p += "/"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", e.ID, e.DeletedAt.Local().Format(time.RFC3339), e.DeletedBy, e.Size, p)
	}
	return w.Flush()
}

func trashRestore(args []string) error {
	fset := flag.NewFlagSet("trash restore", flag.ExitOnError)
	df := addDriveFlags(fset)
	id := fset.String("id", "", "the trash entry (required; from trash list)")
	to := fset.String("to", "", "restore to this path in the drive instead of where it was")
	parents := fset.Bool("parents", false, "with -to: make missing directories")
	replace := fset.Bool("replace", false, "move whatever is at the path now to the trash first")
	if err := parse(fset, args); err != nil {
		return err
	}
	var it drive.Item
	if err := df.run("trash restore", map[string]string{"id": *id, "to": *to,
		"parents": strconv.FormatBool(*parents), "replace": strconv.FormatBool(*replace)}, &it); err != nil {
		return err
	}
	fmt.Printf("restored %s\n", it.Path)
	return nil
}

// actor is who this command acts as, for the records it leaves, when it
// runs here (through the daemon, the daemon says who).
func actor() string {
	if u := os.Getenv("USER"); u != "" {
		return "cli:" + u
	}
	return fmt.Sprintf("uid:%d", os.Getuid())
}
