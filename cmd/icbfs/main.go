// Command icbfs mounts an icbfs filesystem backed by an S3-API-compatible
// store (MinIO today; Azure Blob is a separate backend behind the same
// objstore.Store interface, not yet wired up here).
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

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/resurgentech/icbfs/internal/fuseserver"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "mount" {
		fmt.Fprintln(os.Stderr, "usage: icbfs mount [flags] <mountpoint>")
		os.Exit(2)
	}

	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	endpoint := fset.String("endpoint", "http://127.0.0.1:9000", "S3-API endpoint (MinIO)")
	bucket := fset.String("bucket", "icbfs", "bucket name (must have versioning enabled)")
	fsName := fset.String("fs", "default", "filesystem name (root block key: root/<fs>)")
	accessKey := fset.String("access-key", "minioadmin", "access key")
	secretKey := fset.String("secret-key", "minioadmin", "secret key")
	region := fset.String("region", "us-east-1", "region (ignored by MinIO, required by the SDK)")
	size := fset.Uint64("size", 100<<30, "declared filesystem size in bytes, for df (only used the first time a filesystem name is created)")
	locking := fset.Bool("locking", false, "enable the Locking feature (flock/fcntl); off by default, per ARCHITECTURE.md's Locking section")
	debug := fset.Bool("debug", false, "log every FUSE operation")
	if err := fset.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: icbfs mount [flags] <mountpoint>")
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
	// The root is owned by whoever runs this mount command, not a
	// hardcoded uid/gid — necessary now that fuseMountOptions enables
	// default_permissions (task C1): the kernel enforces this
	// ownership for real, so a hardcoded root uid 0 would lock a
	// non-root mounting user out of their own filesystem's root.
	if err := fsys.Bootstrap(ctx, *size, 0755, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
		log.Fatalf("bootstrap filesystem %q: %v", *fsName, err)
	}
	fsys.EnableLocking(*locking)

	root := fuseserver.Root(fsys)
	server, err := fs.Mount(mountpoint, root, &fs.Options{
		MountOptions: fuseMountOptions(*debug),
		// Without this, go-fuse silently rewrites a real, stored "0000"
		// mode to 0644/0755 on every Getattr — found by testing chmod 000
		// and seeing stat report 644 back.
		NullPermissions: true,
	})
	if err != nil {
		log.Fatalf("mount %s: %v", mountpoint, err)
	}

	log.Printf("icbfs %q mounted at %s (bucket %s via %s)", *fsName, mountpoint, *bucket, *endpoint)
	server.Wait()
}

func fuseMountOptions(debug bool) fuse.MountOptions {
	return fuse.MountOptions{
		FsName: "icbfs",
		Name:   "icbfs",
		Debug:  debug,
		// Task C1 (ROADMAP.md's Permission enforcement part): defers
		// permission enforcement (every open/read/write, not just the
		// access(2)-triggered checks go-fuse's own default Access()
		// fallback handles) to the kernel, which already correctly
		// handles root bypass and full supplementary group
		// membership — preferred over implementing NodeAccesser or
		// manual checks ourselves.
		Options: []string{"default_permissions"},
	}
}
