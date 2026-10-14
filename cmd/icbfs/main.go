// Command icbfs mounts an icbfs filesystem, on an S3-API store, with
// FUSE. See docs/mount.md.
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
	"github.com/resurgentech/icbfs/internal/storeflags"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "mount" {
		fmt.Fprintln(os.Stderr, "usage: icbfs mount [flags] <mountpoint>")
		os.Exit(2)
	}

	fset := flag.NewFlagSet("mount", flag.ExitOnError)
	conn := storeflags.Add(fset)
	fsName := fset.String("fs", "default", "filesystem name")
	logSocket := fset.String("log-socket", "", "mount a drive (-drive) through the icbfs-logd daemon on this Unix socket, logging reads, writes, deletes and directory listings: the server says how to reach the store and which filesystem and directory the drive is (the store flags and -fs are then not used), and whether this machine may only read it; the mount fails without the answer")
	driveID := fset.String("drive", "", "with -log-socket: the drive to mount (the server's id for it)")
	logAsync := fset.Bool("log-async", false, "with -log-socket: queue events instead of waiting for the server to log each one; never slows an operation, but drops (and counts) events while the daemon is unreachable")
	asOf := fset.String("as-of", "", "mount read-only as the filesystem was at this time: a date (2006-01-02, meaning the end of that day), a date-time (2006-01-02T15:04:05) in local time, or RFC 3339 with a zone; or a version name (icbfs-cli names). Fails if the filesystem didn't exist yet then")
	locking := fset.Bool("locking", false, "flock/fcntl locks shared with every other mount of the filesystem (docs/architecture.md's Locking section)")
	enableNotify := fset.Bool("notify", false, "MinIO change notifications (ListenBucketNotification): with -locking, a lock wait retries as soon as the lock changes")
	debug := fset.Bool("debug", false, "log every FUSE operation")
	allowOther := fset.Bool("allow-other", false, "allow users other than the one running this command (and root) to use the mount; without it the kernel refuses every other uid, whatever the file modes say. Needs user_allow_other in /etc/fuse.conf when not run as root")
	cacheDir := fset.String("cache-dir", "", "local disk directory for open files and the read cache (default: a new directory under the system temp directory)")
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
	var driveRoot string // the drive's directory in the filesystem; "" for all of it
	var driveReadOnly bool
	switch {
	case *logSocket != "" && *driveID == "":
		log.Fatal("-log-socket mounts a drive: give -drive")
	case *logSocket == "" && *driveID != "":
		log.Fatal("-drive needs -log-socket: only the server knows what a drive is")
	case *logSocket != "":
		logger = fslog.NewClient(*logSocket, *driveID, *logAsync)
		defer logger.Close(10 * time.Second)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		creds, m, err := logger.Mount(cctx)
		cancel()
		if err != nil {
			log.Fatalf("drive %q: %v", *driveID, err)
		}
		conn.Endpoint, conn.Bucket, conn.AccessKey, conn.SecretKey = creds.Endpoint, creds.Bucket, creds.AccessKey, creds.SecretKey
		if creds.Region != "" {
			conn.Region = creds.Region
		}
		*fsName, driveRoot, driveReadOnly = m.Filesystem, strings.TrimSuffix(m.Root, "/"), m.ReadOnly
	}

	if err := storeflags.Check(conn); err != nil {
		log.Fatal(err)
	}
	store := pkgicbfs.OpenS3(*conn)

	var view *objstore.AsOf
	if *asOf != "" {
		at, err := icbfs.ResolveAsOf(ctx, store, *fsName, *asOf, time.Local)
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
		minioClient, err := newMinIOClient(conn.Endpoint, conn.AccessKey, conn.SecretKey)
		if err != nil {
			log.Fatalf("set up notify backend: %v", err)
		}
		src := miniosrc.New(ctx, minioClient, conn.Bucket, fsys.Prefix())
		defer src.Close()

		// A Broadcaster, not src.Signals() directly: locking's
		// blocking-acquisition accelerator has to observe the same
		// signal stream the FUSE watch dispatch does, and Source.Signals() is a single-consumption
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
	if driveRoot != "" {
		key, err := resolveDir(ctx, fsys, driveRoot)
		if err != nil {
			log.Fatalf("drive %q: its root %s: %v", *driveID, driveRoot, err)
		}
		rootOpts = append(rootOpts, fuseserver.WithRoot(key, driveRoot))
	}
	root := fuseserver.Root(fsys, fuseSignals, rootOpts...)
	server, err := fs.Mount(mountpoint, root, &fs.Options{
		MountOptions: fuseMountOptions(*debug, *allowOther, view != nil || driveReadOnly),
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
		log.Printf("icbfs %q mounted read-only at %s as of %s (bucket %s via %s)", *fsName, mountpoint, view.At().Format(time.RFC3339Nano), conn.Bucket, conn.Endpoint)
	} else {
		log.Printf("icbfs %q mounted at %s (bucket %s via %s)", *fsName, mountpoint, conn.Bucket, conn.Endpoint)
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

// newMinIOClient builds the separate minio-go client the MinIO
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
		// default_permissions defers permission enforcement (every open/read/write, not just the
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
		// EnableLocks: without this, the kernel never
		// forwards FUSE_GETLK/SETLK/SETLKW to this driver at all — it
		// silently falls back to its own local, single-host advisory
		// lock table, which defeats the entire point of this
		// filesystem's remote, object-store-backed locking, and hides
		// any bug in it from tests.
		EnableLocks: true,
	}
}

// resolveDir is the key of directory p ("/a/b") in fsys.
func resolveDir(ctx context.Context, fsys *icbfs.Filesystem, p string) (string, error) {
	key := fsys.RootKey()
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		lookup := fsys.Lookup
		if fsys.PrimaryWindows() {
			lookup = fsys.LookupFold
		}
		e, _, err := lookup(ctx, key, part)
		if err != nil {
			return "", err
		}
		if e.Type != icbfs.TypeDir {
			return "", fmt.Errorf("%s: %w", part, icbfs.ErrNotDir)
		}
		key = e.UUID
	}
	return key, nil
}
