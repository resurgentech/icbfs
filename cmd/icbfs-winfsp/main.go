//go:build windows

// Command icbfs-winfsp mounts an icbfs filesystem on Windows via
// WinFsp, backed by an S3-API-compatible store — the Windows
// counterpart to cmd/icbfs's FUSE mount, per ROADMAP.md's Part F.
// Functional/manual-testing entry point for task F3; not yet wired to
// any of F4-F11's mount flags (case-sensitivity, primary-mode, etc.).
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
	if err := fsys.Bootstrap(ctx, *size, 0755, 0, 0); err != nil {
		log.Fatalf("bootstrap filesystem %q: %v", *fsName, err)
	}

	// CaseSensitive(true): icbfs.Filesystem's own directory lookup is
	// already exact-match/case-sensitive (confirmed by reading it, not
	// assumed) — without telling WinFsp the same thing, the kernel
	// case-folds names internally and can hand this driver an
	// uppercased, no-longer-matching name back on later calls (found
	// via real on-VM testing: Remove-Item's underlying delete request
	// arrived as `\SUBDIR\NESTED.TXT` for an entry actually named
	// `subdir/nested.txt`, which then failed to resolve at all). A
	// real case-insensitive *mount option* is ROADMAP.md's F4, a
	// separate task; this is just making the two layers agree for
	// now, not implementing that option.
	server, err := winfsp.Mount(winfspserver.Root(fsys), mountpoint, winfsp.FileSystemName("icbfs"), winfsp.CaseSensitive(true))
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
