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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/resurgentech/icbfs/internal/fslog"
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
	logSocket := fset.String("log-socket", "", "log reads, writes, deletes and directory listings through the icbfs-logd daemon on this Unix socket — which also supplies the store's endpoint, bucket and keys (-endpoint, -bucket, -region, -access-key and -secret-key are then ignored); the mount fails without them")
	logAsync := fset.Bool("log-async", false, "with -log-socket: queue events instead of waiting for the server to log each one; never slows an operation, but drops (and counts) events while the daemon is unreachable")
	asOf := fset.String("as-of", "", "mount read-only as the filesystem was at this time: a date (2006-01-02, meaning the end of that day), a date-time (2006-01-02T15:04:05) in local time, or RFC 3339 with a zone. Fails if the filesystem didn't exist yet then")
	locking := fset.Bool("locking", false, "enable the Locking feature (flock/fcntl); off by default, per ARCHITECTURE.md's Locking section")
	enableNotify := fset.Bool("notify", false, "enable the Change notifications feature (ROADMAP.md Part E) via MinIO's ListenBucketNotification; off by default, same opt-in reasoning as --locking. Only the MinIO backend (task E4) is wired up here — Azure/AWS S3 adapters (tasks E5/E6) aren't reachable from this flag.")
	debug := fset.Bool("debug", false, "log every FUSE operation")
	allowOther := fset.Bool("allow-other", false, "allow users other than the one running this command (and root) to access the mount; off by default, same opt-in reasoning as --locking/--notify. Without it, FUSE's own kernel-side fuse_allow_current_process check rejects every other uid outright, independent of default_permissions or file modes — a real finding from running pjdfstest's -u/-g permission tests against a root-mounted filesystem, not a hypothetical. Passing this through typically also needs user_allow_other in /etc/fuse.conf, same as any other FUSE filesystem's -o allow_other.")
	cacheDir := fset.String("cache-dir", "", "local disk directory for the file-content cache (ROADMAP.md's Part G, task G3); empty (the default) uses a fresh os.TempDir() subdirectory. Unlike --locking/--notify, this isn't optional functionality — every open file's content lives here, not in RAM, so there's always a real directory in use even when this flag is left at its default.")
	cacheSize := fset.Uint64("cache-size", 512<<20, "maximum total bytes the local disk cache (-cache-dir) may hold before evicting the least-recently-used cached file")
	if err := fset.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: icbfs mount [flags] <mountpoint>")
		os.Exit(2)
	}
	mountpoint := fset.Arg(0)

	ctx := context.Background()

	var logger *fslog.Client
	if *logSocket != "" {
		logger = fslog.NewClient(*logSocket, *fsName, *logAsync)
		defer logger.Close(10 * time.Second)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		creds, err := logger.Credentials(cctx)
		cancel()
		if err != nil {
			log.Fatalf("no credentials for %q from the logging daemon: %v", *fsName, err)
		}
		*endpoint, *bucket, *accessKey, *secretKey = creds.Endpoint, creds.Bucket, creds.AccessKey, creds.SecretKey
		if creds.Region != "" {
			*region = creds.Region
		}
	}

	client := s3.New(s3.Options{
		Region:       *region,
		BaseEndpoint: aws.String(*endpoint),
		UsePathStyle: true,
		Credentials:  awscreds.NewStaticCredentialsProvider(*accessKey, *secretKey, ""),
	})
	var store objstore.Store = objstore.NewS3Store(client, *bucket)

	var view *objstore.AsOf
	if *asOf != "" {
		at, err := icbfs.ParseAsOf(*asOf, time.Local)
		if err != nil {
			log.Fatal(err)
		}
		if *locking || *enableNotify {
			log.Fatal("-as-of mounts the past read-only; -locking and -notify don't apply")
		}
		view, err = objstore.NewAsOf(ctx, store, at, *cacheDir)
		if err != nil {
			log.Fatal(err)
		}
		defer view.Close()
		store = view
	}

	fsys := icbfs.New(store, *fsName)
	if *cacheDir != "" {
		fsys.SetCacheOptions(*cacheDir, int64(*cacheSize))
	}
	if view != nil {
		if err := fsys.BootstrapReadOnly(ctx); err != nil {
			view.Close()
			log.Fatalf("filesystem %q as of %s: %v", *fsName, view.At().Format(time.RFC3339), err)
		}
	} else if err := fsys.Attach(ctx); err != nil {
		// Mounts never create a filesystem: icbfs-cli fs create, or the
		// drive API, does.
		log.Fatalf("filesystem %q: %v", *fsName, err)
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

	var rootOpts []fuseserver.Option
	if logger != nil {
		rootOpts = append(rootOpts, fuseserver.WithLogger(logger))
	}
	root := fuseserver.Root(fsys, fuseSignals, rootOpts...)
	server, err := fs.Mount(mountpoint, root, &fs.Options{
		MountOptions: fuseMountOptions(*debug, *allowOther, view != nil),
		// Without this, go-fuse silently rewrites a real, stored "0000"
		// mode to 0644/0755 on every Getattr — found by testing chmod 000
		// and seeing stat report 644 back.
		NullPermissions: true,
	})
	if err != nil {
		if view != nil {
			view.Close()
		}
		log.Fatalf("mount %s: %v", mountpoint, err)
	}

	if view != nil {
		log.Printf("icbfs %q mounted read-only at %s as of %s (bucket %s via %s)", *fsName, mountpoint, view.At().Format(time.RFC3339Nano), *bucket, *endpoint)
	} else {
		log.Printf("icbfs %q mounted at %s (bucket %s via %s)", *fsName, mountpoint, *bucket, *endpoint)
	}
	// Ctrl+C or SIGTERM unmounts cleanly, so the deferred cleanup (an
	// as-of mount's version cache) runs.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		_ = server.Unmount()
	}()
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

func fuseMountOptions(debug, allowOther, readOnly bool) fuse.MountOptions {
	options := []string{"default_permissions"}
	if allowOther {
		options = append(options, "allow_other")
	}
	if readOnly {
		options = append(options, "ro")
	}
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
		//
		// allow_other is a separate, orthogonal kernel restriction from
		// default_permissions: without it, FUSE's fuse_allow_current_process
		// check rejects every caller except the mounting user (and root)
		// outright, before default_permissions's own mode-based check
		// ever runs — found while running pjdfstest's -u/-g permission
		// tests against a root-mounted filesystem, where it looked at
		// first like a real EACCES bug in icbfs's own permission
		// enforcement, until isolating it to this separate, well-known
		// FUSE restriction (confirmed by reproducing the same EACCES
		// with plain coreutils via sudo -u, independent of pjdfstest
		// entirely, then confirming it disappeared once allow_other was
		// added). --allow-other opts into exposing it.
		Options: options,
		// EnableLocks (task B8): without this, the kernel never
		// forwards FUSE_GETLK/SETLK/SETLKW to this driver at all — it
		// silently falls back to its own local, single-host advisory
		// lock table, which defeats the entire point of this
		// filesystem's remote, object-store-backed locking (and was
		// found, at Jared's direction, to have been masking a real
		// bug in this exact way throughout testing — see
		// ASSUMPTIONS.md's B8 entry).
		EnableLocks: true,
	}
}
