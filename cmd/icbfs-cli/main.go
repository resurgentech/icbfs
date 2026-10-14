// Command icbfs-cli does what isn't a filesystem operation: creating and
// managing icbfs filesystems in a bucket (fs), and more to come. Mounts
// never create a filesystem; this does, or the drive API.
//
//	icbfs-cli fs create   -fs NAME [-primary-windows] [-size N] [-max-file-size N] [root options]
//	icbfs-cli fs list
//	icbfs-cli fs describe -fs NAME
//	icbfs-cli fs resize   -fs NAME -size N
//	icbfs-cli fs archive  -fs NAME
//	icbfs-cli fs prune    -fs NAME -yes
//
// Every command takes the store's connection (-endpoint, -bucket,
// -region, -access-key, -secret-key; or $ICBFS_ENDPOINT, $ICBFS_BUCKET,
// $ICBFS_ACCESS_KEY, $ICBFS_SECRET_KEY).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

const usage = `usage: icbfs-cli fs create|list|describe|resize|archive|prune [flags]
run "icbfs-cli fs <command> -h" for a command's flags`

func main() {
	if len(os.Args) < 3 || os.Args[1] != "fs" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[2], os.Args[3:]
	c, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	opCtx = c
	var err error
	switch cmd {
	case "create":
		err = fsCreate(args)
	case "list":
		err = fsList(args)
	case "describe":
		err = fsDescribe(args)
	case "resize":
		err = fsResize(args)
	case "archive":
		err = fsArchive(args)
	case "prune":
		err = fsPrune(args)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "icbfs-cli:", err)
		os.Exit(1)
	}
}

// storeFlags are every command's connection to the store.
type storeFlags struct {
	endpoint, bucket, region, accessKey, secretKey *string
}

func addStoreFlags(fset *flag.FlagSet) storeFlags {
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	return storeFlags{
		endpoint:  fset.String("endpoint", env("ICBFS_ENDPOINT", "http://127.0.0.1:9000"), "S3-API endpoint ($ICBFS_ENDPOINT)"),
		bucket:    fset.String("bucket", env("ICBFS_BUCKET", "icbfs"), "bucket, with versioning enabled ($ICBFS_BUCKET)"),
		region:    fset.String("region", env("ICBFS_REGION", "us-east-1"), "region ($ICBFS_REGION)"),
		accessKey: fset.String("access-key", env("ICBFS_ACCESS_KEY", ""), "access key ($ICBFS_ACCESS_KEY)"),
		secretKey: fset.String("secret-key", env("ICBFS_SECRET_KEY", ""), "secret key ($ICBFS_SECRET_KEY)"),
	}
}

func (s storeFlags) store() objstore.Store {
	client := s3.New(s3.Options{
		Region:       *s.region,
		BaseEndpoint: aws.String(*s.endpoint),
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(*s.accessKey, *s.secretKey, ""),
	})
	return objstore.NewS3Store(client, *s.bucket)
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
	info, err := icbfs.CreateFilesystem(ctx(), sf.store(), icbfs.Settings{
		Name: *name, Size: *size, PrimaryWindows: *primaryWindows, MaxFileSize: *maxFileSize,
		RootMode: uint32(*rootMode), RootUid: uint32(*rootUid), RootGid: uint32(*rootGid), RootSDDL: sddl,
	})
	if err != nil {
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
	all, err := icbfs.ListFilesystems(ctx(), sf.store())
	if err != nil {
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
	store := sf.store()
	info, err := icbfs.DescribeFilesystem(ctx(), store, *name)
	if err != nil {
		return err
	}
	f := icbfs.New(store, *name)
	if err := f.Attach(ctx()); err != nil {
		return err
	}
	_, used, err := f.StatFS(ctx())
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(struct {
			icbfs.Info
			Used uint64
		}{info, used})
	}
	printInfos([]icbfs.Info{info}, false)
	fmt.Printf("used: %d bytes\n", used)
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
	return icbfs.Resize(ctx(), sf.store(), *name, *size)
}

func fsArchive(args []string) error {
	fset := flag.NewFlagSet("fs archive", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "filesystem (required)")
	if err := parse(fset, args); err != nil {
		return err
	}
	return icbfs.Archive(ctx(), sf.store(), *name)
}

func fsPrune(args []string) error {
	fset := flag.NewFlagSet("fs prune", flag.ExitOnError)
	sf := addStoreFlags(fset)
	name := fset.String("fs", "", "filesystem (required)")
	yes := fset.Bool("yes", false, "really delete everything in it, irreversibly")
	if err := parse(fset, args); err != nil {
		return err
	}
	store := sf.store()
	info, err := icbfs.DescribeFilesystem(ctx(), store, *name)
	if err != nil {
		return err
	}
	if !info.Archived {
		return fmt.Errorf("filesystem %q isn't archived: archive it first (fs archive), then prune", *name)
	}
	if !*yes {
		return errors.New("prune deletes every object of the filesystem irreversibly; add -yes")
	}
	return icbfs.Prune(ctx(), store, *name)
}

func printInfos(all []icbfs.Info, asJSON bool) {
	if asJSON {
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
