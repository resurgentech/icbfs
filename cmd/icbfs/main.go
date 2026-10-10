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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/resurgentech/icbfs/internal/fuseserver"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/notify"
	"github.com/resurgentech/icbfs/internal/notify/miniosrc"
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
	uidOverride := fset.Int("uid", -1, "owner uid for the filesystem root (only used the first time a filesystem name is created); defaults to the uid of the user running this command, same as every other FUSE filesystem's -o uid= override (sshfs, NFS, etc.)")
	gidOverride := fset.Int("gid", -1, "owner gid for the filesystem root (only used the first time a filesystem name is created); defaults to the gid of the user running this command, same as every other FUSE filesystem's -o gid= override (sshfs, NFS, etc.)")
	locking := fset.Bool("locking", false, "enable the Locking feature (flock/fcntl); off by default, per ARCHITECTURE.md's Locking section")
	enableNotify := fset.Bool("notify", false, "enable the Change notifications feature (ROADMAP.md Part E) via MinIO's ListenBucketNotification; off by default, same opt-in reasoning as --locking. Only the MinIO backend (task E4) is wired up here — Azure/AWS S3 adapters (tasks E5/E6) aren't reachable from this flag.")
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
	// The root is owned by whoever runs this mount command by default
	// — necessary now that fuseMountOptions enables default_permissions
	// (task C1): the kernel enforces this ownership for real, so a
	// hardcoded root uid 0 would lock a non-root mounting user out of
	// their own filesystem's root. --uid/--gid override that default,
	// same as every other FUSE filesystem exposes (sshfs, NFS, etc.) —
	// a real, well-known need (e.g. mounting on behalf of a different
	// user than the one running the mount command), not a hypothetical.
	rootUID, rootGID := uint32(os.Getuid()), uint32(os.Getgid())
	if *uidOverride >= 0 {
		rootUID = uint32(*uidOverride)
	}
	if *gidOverride >= 0 {
		rootGID = uint32(*gidOverride)
	}
	if err := fsys.Bootstrap(ctx, *size, 0755, rootUID, rootGID); err != nil {
		log.Fatalf("bootstrap filesystem %q: %v", *fsName, err)
	}
	fsys.EnableLocking(*locking)

	var fuseSignals <-chan notify.Signal
	if *enableNotify {
		minioClient, err := newMinIOClient(*endpoint, *accessKey, *secretKey)
		if err != nil {
			log.Fatalf("set up notify backend: %v", err)
		}
		src := miniosrc.New(ctx, minioClient, *bucket, fsys.Prefix())
		defer src.Close()

		// A Broadcaster, not src.Signals() directly: task B10 needs
		// Locking's blocking-acquisition accelerator to independently
		// observe the exact same signal stream task E3's FUSE watch
		// dispatch does, and Source.Signals() is a single-consumption
		// channel — reading it from two places would steal signals
		// from each other instead of each seeing every one.
		broadcaster := notify.NewBroadcaster(src)
		fuseSignals = broadcaster.Subscribe()
		if *locking {
			fsys.EnableChangeNotifications(broadcaster.Subscribe())
		}
	}

	root := fuseserver.Root(fsys, fuseSignals)
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

// newMinIOClient builds the separate minio-go client task E4's
// notification adapter needs — see internal/notify/miniosrc's package
// doc comment on why this can't just reuse the aws-sdk-go-v2 client
// already in use for every other S3-API call. endpoint carries a
// "http://"/"https://" scheme the way --endpoint is otherwise used
// (as aws-sdk-go-v2's BaseEndpoint); minio.New wants the bare host
// plus a separate Secure bool instead, so that's split out here.
func newMinIOClient(endpoint, accessKey, secretKey string) (*minio.Client, error) {
	secure := strings.HasPrefix(endpoint, "https://")
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	return minio.New(host, &minio.Options{
		Creds:  miniocreds.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
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
