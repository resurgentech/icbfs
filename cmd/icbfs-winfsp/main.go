//go:build windows

// Command icbfs-winfsp mounts an icbfs filesystem on Windows via
// WinFsp, backed by an S3-API-compatible store — the Windows
// counterpart to cmd/icbfs's FUSE mount, per ROADMAP.md's Part F.
// Functional/manual-testing entry point for tasks F3-F5; not yet wired
// to F6-F11's mount flags.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	winfsp "github.com/winfsp/go-winfsp"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/winfspserver"
)

func main() {
	fset := flag.NewFlagSet("icbfs-winfsp", flag.ExitOnError)
	endpoint := fset.String("endpoint", "http://127.0.0.1:9000", "S3-API endpoint (MinIO)")
	bucket := fset.String("bucket", "icbfs", "bucket name (must have versioning enabled)")
	fsName := fset.String("fs", "default", "filesystem name")
	accessKey := fset.String("access-key", "minioadmin", "access key")
	secretKey := fset.String("secret-key", "minioadmin", "secret key")
	region := fset.String("region", "us-east-1", "region (ignored by MinIO, required by the SDK)")
	size := fset.Uint64("size", 100<<30, "declared filesystem size in bytes (only used the first time a filesystem name is created)")
	caseInsensitive := fset.Bool("case-insensitive", true, "case-insensitive name lookup, following the standard NTFS/Samba/WSL2 pattern (ARCHITECTURE.md's Windows compatibility section) — storage itself always stays case-sensitive/case-preserving regardless of this flag")
	primaryWindows := fset.Bool("primary-windows", true, "mark this filesystem primary-Windows rather than primary-POSIX (ARCHITECTURE.md's \"Windows compatibility: primary mode\" section) — only used the first time a filesystem name is created, immutable afterward, same as --size")
	if err := fset.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: icbfs-winfsp [flags] <mountpoint, e.g. J:>")
		os.Exit(2)
	}
	mountpoint := fset.Arg(0)

	ctx := context.Background()
	client := s3.New(s3.Options{
		Region:       *region,
		BaseEndpoint: aws.String(*endpoint),
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(*accessKey, *secretKey, ""),
	})
	store := objstore.NewS3Store(client, *bucket)

	fsys := icbfs.New(store, *fsName)
	if err := fsys.Bootstrap(ctx, *size, 0755, 0, 0, *primaryWindows); err != nil {
		log.Fatalf("bootstrap filesystem %q: %v", *fsName, err)
	}

	// winfsp.CaseSensitive and winfspserver.Root's own caseInsensitive
	// parameter must agree (task F4) — see Root's doc comment for why
	// these are two separate, both load-bearing settings, not one flag
	// under two names: WinFsp's CaseSensitive controls whether the
	// *kernel* folds case before ever calling this driver at all
	// (confirmed in task F3: with it left off/default, the kernel can
	// hand this driver a canonicalized, differently-cased name than
	// what's actually stored, breaking a plain Remove-Item outright),
	// while Root's parameter controls whether this driver's own
	// resolver additionally tolerates a case mismatch against
	// icbfs.Filesystem's always-case-sensitive storage.
	server, err := winfsp.Mount(
		winfspserver.Root(fsys, *caseInsensitive),
		mountpoint,
		winfsp.FileSystemName("icbfs"),
		winfsp.CaseSensitive(!*caseInsensitive),
	)
	if err != nil {
		log.Fatalf("mount %s: %v", mountpoint, err)
	}
	defer server.Unmount()

	log.Printf("icbfs %q mounted at %s (bucket %s via %s)", *fsName, mountpoint, *bucket, *endpoint)
	// winfsp.Mount's dispatcher runs its own background goroutines;
	// block here until killed, same role cmd/icbfs's server.Wait()
	// plays for the FUSE side.
	select {}
}
