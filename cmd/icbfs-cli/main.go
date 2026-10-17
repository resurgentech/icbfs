// Command icbfs-cli does what isn't a filesystem operation: creating and
// managing icbfs filesystems in a bucket (fs), naming versions (names),
// and the trash (trash). Mounts never create a filesystem; this does, or
// the drive API.
//
//	icbfs-cli fs create   -fs NAME [-primary-windows] [-size N] [-max-file-size N] [-dir-block-entries N] [root options]
//	icbfs-cli fs list
//	icbfs-cli fs describe -fs NAME
//	icbfs-cli fs check    -fs NAME [-json]
//	icbfs-cli fs resize   -fs NAME -size N
//	icbfs-cli fs archive  -fs NAME
//	icbfs-cli fs prune    -fs NAME -yes
//	icbfs-cli names create DRIVE -name VERSION [-at TIME] [-filesystem-wide]
//	icbfs-cli names list   DRIVE
//	icbfs-cli names delete DRIVE -name VERSION
//	icbfs-cli trash list    DRIVE [-path PATH] [-limit N]
//	icbfs-cli trash restore DRIVE -id ENTRY [-to PATH [-parents]] [-replace]
//
// Two ways to run. With the store's connection (-endpoint, -bucket,
// -region, -access-key, -secret-key; or $ICBFS_ENDPOINT, $ICBFS_BUCKET,
// $ICBFS_ACCESS_KEY, $ICBFS_SECRET_KEY), it does the work itself; a
// DRIVE is then -fs NAME [-root DIR], a filesystem or a directory in one.
// On a machine with the daemon, -daemon SOCKET (or $ICBFS_DAEMON) sends
// the command through it to the server instead, which checks it and runs
// it as this machine's account — no store credentials on the machine; a
// DRIVE is then -drive ID, the server's, and the server knows which
// filesystem and directory that is. The fs commands name a filesystem
// either way: they administer the bucket's filesystems, drives' storage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/resurgentech/icbfs/internal/fslog"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/sidecmd"
	"github.com/resurgentech/icbfs/internal/storeflags"
	"github.com/resurgentech/icbfs/pkg/drive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

const usage = `usage: icbfs-cli fs create|list|describe|resize|archive|prune [flags]
       icbfs-cli names create|list|delete [flags]
       icbfs-cli trash list|restore [flags]
run "icbfs-cli <group> <command> -h" for a command's flags`

var commands = map[string]func([]string) error{
	"fs create": fsCreate, "fs list": fsList, "fs describe": fsDescribe, "fs check": fsCheck, "fs resize": fsResize,
	"fs archive": fsArchive, "fs prune": fsPrune,
	"names create": namesCreate, "names list": namesList, "names delete": namesDelete,
	"trash list": trashList, "trash restore": trashRestore,
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]+" "+os.Args[2]]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	c, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	opCtx = c
	if err := cmd(os.Args[3:]); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "icbfs-cli:", err)
		os.Exit(1)
	}
}

// storeFlags are every command's way to the store: directly, or through
// the daemon.
type storeFlags struct {
	conn   *pkgicbfs.StoreConfig
	daemon *string
}

func addStoreFlags(fset *flag.FlagSet) storeFlags {
	daemon := os.Getenv("ICBFS_DAEMON")
	return storeFlags{
		conn:   storeflags.Add(fset),
		daemon: fset.String("daemon", daemon, "send the command through the daemon's socket instead, to run on the server ($ICBFS_DAEMON)"),
	}
}

func (s storeFlags) store() (objstore.Store, error) { return pkgicbfs.Open(*s.conn) }

// run runs a storage command — here, or on the server through the
// daemon — and decodes its result into out (if not nil).
func (s storeFlags) run(name string, args map[string]string, out any) error {
	return s.runWith(name, args, out, nil)
}

// runWith is run, with how to open the drive a drive command names when
// it runs here.
func (s storeFlags) runWith(name string, args map[string]string, out any, openDrive func(context.Context, map[string]string) (drive.Drive, error)) error {
	var result json.RawMessage
	var err error
	if *s.daemon != "" {
		c := fslog.NewClient(*s.daemon, "", false)
		result, err = c.Command(ctx(), name, args)
	} else {
		if err := storeflags.Check(s.conn); err != nil {
			return err
		}
		store, err := s.store()
		if err != nil {
			return err
		}
		result, err = sidecmd.Run(ctx(), sidecmd.Env{Store: store, Drive: openDrive}, sidecmd.Request{Name: name, Args: args, Actor: actor()})
	}
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(result, out)
}

func parse(fset *flag.FlagSet, args []string) error {
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fset.Args(), " "))
	}
	return nil
}

// opCtx bounds the whole command; set in main.
var opCtx context.Context = context.Background()

func ctx() context.Context { return opCtx }

func fsCreate(args []string) error {
	fset := flag.NewFlagSet("fs create", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "the new filesystem's name (required)")
	primaryWindows := fset.Bool("primary-windows", false, "primary-Windows (NTFS naming, case-insensitive order, Windows ACLs authoritative) rather than primary-POSIX; fixed at creation")
	size := fset.Uint64("size", 100<<30, "declared capacity in bytes, what df shows (changeable later with resize)")
	maxFileSize := fset.Uint64("max-file-size", icbfs.DefaultMaxFileSize, "largest file it holds, in bytes; fixed at creation")
	dirBlockEntries := fset.Uint("dir-block-entries", icbfs.DefaultDirBlockEntries, "entries a directory block holds before it splits: more favours lookups and listings, fewer the cost of each change; fixed at creation")
	rootUid := fset.Int("root-uid", max(os.Getuid(), 0), "the root folder's owner uid (default: yours; 0 on Windows, which has none)")
	rootGid := fset.Int("root-gid", max(os.Getgid(), 0), "the root folder's group gid (default: yours; 0 on Windows)")
	rootMode := fset.Uint("root-mode", 0o755, "the root folder's permission bits")
	rootSDDL := fset.String("root-sddl", "", "primary-Windows: the root folder's security descriptor, as SDDL")
	rootOwner := fset.String("root-owner", "", "primary-Windows: the SID that owns the root and has full control of everything under it (instead of -root-sddl)")
	if err := parse(fset, args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-fs is required")
	}
	if *rootUid < 0 || *rootGid < 0 {
		return errors.New("-root-uid and -root-gid must be ≥ 0")
	}
	sddl := ""
	if *primaryWindows {
		var err error
		if sddl, err = rootSecurity(*rootSDDL, *rootOwner); err != nil {
			return err
		}
	} else if *rootSDDL != "" || *rootOwner != "" {
		return errors.New("-root-sddl and -root-owner are for -primary-windows filesystems")
	}
	var info icbfs.Info
	if err := sf.run("fs create", map[string]string{
		"fs": *name, "primary-windows": strconv.FormatBool(*primaryWindows),
		"size": strconv.FormatUint(*size, 10), "max-file-size": strconv.FormatUint(*maxFileSize, 10),
		"dir-block-entries": strconv.FormatUint(uint64(*dirBlockEntries), 10),
		"root-mode":         "0" + strconv.FormatUint(uint64(*rootMode), 8), "root-uid": strconv.Itoa(*rootUid), "root-gid": strconv.Itoa(*rootGid),
		"root-sddl": sddl,
	}, &info); err != nil {
		return err
	}
	printInfos([]icbfs.Info{info}, false)
	return nil
}

// rootSecurity is a primary-Windows root's SDDL: given outright, built
// from an owner SID, or — run on Windows with neither — the running
// account's own default, as a Windows mount shows a root without one.
func rootSecurity(sddl, owner string) (string, error) {
	switch {
	case sddl != "" && owner != "":
		return "", errors.New("give -root-sddl or -root-owner, not both")
	case sddl != "":
		return sddl, nil
	case owner != "":
		if !strings.HasPrefix(owner, "S-1-") {
			return "", fmt.Errorf("-root-owner %q isn't a SID (S-1-…)", owner)
		}
		// The owner has full control, inherited by everything below, as
		// do SYSTEM and Administrators, as on an NTFS volume's root.
		return fmt.Sprintf("O:%sG:%sD:PAI(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", owner, owner, owner), nil
	}
	return defaultRootSDDL()
}

func fsList(args []string) error {
	fset := flag.NewFlagSet("fs list", flag.ExitOnError)
	sf := addStoreFlags(fset)
	asJSON := fset.Bool("json", false, "JSON output")
	if err := parse(fset, args); err != nil {
		return err
	}
	var all []icbfs.Info
	if err := sf.run("fs list", nil, &all); err != nil {
		return err
	}
	printInfos(all, *asJSON)
	return nil
}

func fsDescribe(args []string) error {
	fset := flag.NewFlagSet("fs describe", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "filesystem (required)")
	asJSON := fset.Bool("json", false, "JSON output")
	if err := parse(fset, args); err != nil {
		return err
	}
	var d sidecmd.Described
	if err := sf.run("fs describe", map[string]string{"fs": *name}, &d); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(d)
	}
	printInfos([]icbfs.Info{d.Info}, false)
	fmt.Printf("used: %d bytes\n", d.Used)
	return nil
}

// fsCheck walks a filesystem and reports inconsistencies, changing
// nothing; it exits non-zero if it found any. Run it on a quiet
// filesystem: one changing while it's walked can show problems that
// aren't.
func fsCheck(args []string) error {
	fset := flag.NewFlagSet("fs check", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "filesystem (required)")
	asJSON := fset.Bool("json", false, "JSON output")
	if err := parse(fset, args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-fs is required")
	}
	var r icbfs.CheckReport
	if err := sf.run("fs check", map[string]string{"fs": *name}, &r); err != nil {
		return err
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
			return err
		}
	} else {
		fmt.Printf("%d directories, %d files, %d directory blocks; %d unreachable objects\n", r.Directories, r.Files, r.Blocks, r.Unreachable)
		for _, k := range r.UnreachableListed {
			fmt.Printf("  unreachable: %s\n", k)
		}
		for _, p := range r.Problems {
			fmt.Printf("%s: %s %s: %s\n", p.Kind, p.Key, p.Path, p.Detail)
		}
	}
	if !r.OK() {
		return fmt.Errorf("%s: %d problems", *name, len(r.Problems))
	}
	return nil
}

func fsResize(args []string) error {
	fset := flag.NewFlagSet("fs resize", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "filesystem (required)")
	size := fset.Uint64("size", 0, "the new declared capacity in bytes (required)")
	if err := parse(fset, args); err != nil {
		return err
	}
	if *size == 0 {
		return errors.New("-size is required")
	}
	return sf.run("fs resize", map[string]string{"fs": *name, "size": strconv.FormatUint(*size, 10)}, nil)
}

func fsArchive(args []string) error {
	fset := flag.NewFlagSet("fs archive", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "filesystem (required)")
	if err := parse(fset, args); err != nil {
		return err
	}
	return sf.run("fs archive", map[string]string{"fs": *name}, nil)
}

func fsPrune(args []string) error {
	fset := flag.NewFlagSet("fs prune", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "filesystem (required)")
	yes := fset.Bool("yes", false, "really delete everything in it, irreversibly")
	if err := parse(fset, args); err != nil {
		return err
	}
	var d sidecmd.Described
	if err := sf.run("fs describe", map[string]string{"fs": *name}, &d); err != nil {
		return err
	}
	if !d.Archived {
		return fmt.Errorf("filesystem %q isn't archived: archive it first (fs archive), then prune", *name)
	}
	if !*yes {
		return errors.New("prune deletes every object of the filesystem irreversibly; add -yes")
	}
	return sf.run("fs prune", map[string]string{"fs": *name}, nil)
}

func printInfos(all []icbfs.Info, asJSON bool) {
	if asJSON {
		if all == nil {
			all = []icbfs.Info{}
		}
		_ = json.NewEncoder(os.Stdout).Encode(all)
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tMODE\tSIZE\tMAX FILE\tARCHIVED\tCREATED")
	for _, fs := range all {
		mode := "posix"
		if fs.PrimaryWindows {
			mode = "windows"
		}
		created := ""
		if !fs.Created.IsZero() {
			created = fs.Created.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%v\t%s\n", fs.Name, fs.ID, mode, fs.Size, fs.MaxFileSize, fs.Archived, created)
	}
	w.Flush()
}
