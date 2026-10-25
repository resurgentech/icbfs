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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

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
	if filepath.Base(os.Args[0]) == helperName {
		os.Exit(mountHelper(os.Args[1:]))
	}
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
	locking := fset.Bool("locking", true, "flock/fcntl locks shared with every other mount of the filesystem (docs/architecture.md's Locking section); -locking=false refuses them (ENOSYS) and saves a round trip per flush")
	enableNotify := fset.Bool("notify", false, "MinIO change notifications (ListenBucketNotification): a lock wait retries as soon as the lock changes")
	readOnly := fset.Bool("read-only", false, "mount read-only")
	devices := fset.Bool("dev", false, "honour device nodes (as root; a FUSE mount is nodev unless asked: anyone who can write the filesystem could plant one)")
	setuid := fset.Bool("suid", false, "honour setuid and setgid bits on executables (as root; nosuid unless asked, for the same reason)")
	noExec := fset.Bool("noexec", false, "don't allow executing files from the mount")
	atime := fset.String("atime", "relatime", "when reads record access times: relatime (when the recorded one is no newer than the last change, or a day old), strict (every read), or noatime (never)")
	noDirAtime := fset.Bool("nodiratime", false, "don't record access times for directories")
	debug := fset.Bool("debug", false, "log every FUSE operation")
	allowOther := fset.Bool("allow-other", false, "allow users other than the one running this command (and root) to use the mount; without it the kernel refuses every other uid, whatever the file modes say. Needs user_allow_other in /etc/fuse.conf when not run as root")
	history := storeflags.AddHistory(fset)
	cacheDir := fset.String("cache-dir", "", "local disk directory for open files and the read cache (default: a new directory under the system temp directory)")
	cacheSize := fset.Uint64("cache-size", 512<<20, "maximum total bytes the local disk cache (-cache-dir) may hold before evicting the least-recently-used cached file")
	metaCache := fset.Duration("meta-cache", 5*time.Second, "how long the kernel trusts names and attributes, as NFS's attribute cache: other machines' renames, creates and chmods can take this long to show here; opens and locks still check the store (0: trust nothing; as-of mounts default to a day)")
	if err := fset.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: icbfs mount [flags] <mountpoint>")
		os.Exit(2)
	}
	if *metaCache < 0 {
		fmt.Fprintln(os.Stderr, "icbfs mount: -meta-cache can't be negative")
		os.Exit(2)
	}
	metaCacheSet := false
	fset.Visit(func(f *flag.Flag) { metaCacheSet = metaCacheSet || f.Name == "meta-cache" })
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
		*conn = creds
		*fsName, driveRoot, driveReadOnly = m.Filesystem, strings.TrimSuffix(m.Root, "/"), m.ReadOnly
	}

	if err := storeflags.Check(conn); err != nil {
		log.Fatal(err)
	}
	store, err := pkgicbfs.Open(*conn)
	if err != nil {
		log.Fatal(err)
	}

	var view *objstore.AsOf
	if *asOf != "" {
		at, err := icbfs.ResolveAsOf(ctx, store, *fsName, *asOf, time.Local)
		if err != nil {
			log.Fatal(err)
		}
		if *enableNotify {
			log.Fatal("-as-of mounts the past read-only; -notify doesn't apply")
		}
		*locking = false // nothing can be written, so there's nothing to lock
		hist, err := history.Open(ctx, store, conn)
		if err != nil {
			log.Fatal(err)
		}
		defer hist.Close()
		// What a purge has erased since the cache last looked goes from it.
		if live := icbfs.New(store, *fsName); live.Attach(ctx) == nil {
			if _, err := icbfs.ForgetChangedPast(ctx, live, hist); err != nil {
				log.Fatalf("history cache: %v", err)
			}
		}
		view, err = objstore.NewAsOf(ctx, hist, at)
		if err != nil {
			log.Fatal(err)
		}
		store = view
	}

	fsys := icbfs.New(store, *fsName)
	if *cacheDir != "" {
		fsys.SetCacheOptions(*cacheDir, int64(*cacheSize))
	}
	if view != nil {
		if err := fsys.BootstrapReadOnly(ctx); err != nil {
			log.Fatalf("filesystem %q as of %s: %v", *fsName, view.At().Format(time.RFC3339), err)
		}
	} else if err := fsys.Attach(ctx); err != nil {
		// Mounts never create a filesystem: icbfs-cli fs create, or the
		// drive API, does.
		log.Fatalf("filesystem %q: %v", *fsName, err)
	}
	fsys.EnableLocking(*locking)
	switch *atime {
	case "relatime":
		fsys.SetAtime(icbfs.AtimeRelative, !*noDirAtime)
	case "strict":
		fsys.SetAtime(icbfs.AtimeStrict, !*noDirAtime)
	case "noatime":
		fsys.SetAtime(icbfs.AtimeNever, false)
	default:
		log.Fatalf("-atime %q: relatime, strict or noatime", *atime)
	}
	if *readOnly || driveReadOnly {
		fsys.SetAtime(icbfs.AtimeNever, false) // a read-only mount records nothing
	}
	// POSIX ACLs on primary-POSIX filesystems; on primary-Windows ones
	// the security descriptor is the permission.
	acls := !fsys.PrimaryWindows()
	fsys.EnablePosixACLs(acls)

	var fuseSignals <-chan notify.Signal
	if *enableNotify {
		if conn.Kind == pkgicbfs.KindAzure {
			log.Fatal("-notify needs MinIO's bucket notifications: not on Azure Blob Storage")
		}
		minioClient, err := pkgicbfs.NewMinIOClient(conn.S3.Endpoint, conn.S3.AccessKey, conn.S3.SecretKey)
		if err != nil {
			log.Fatalf("set up notify backend: %v", err)
		}
		src := miniosrc.New(ctx, minioClient, conn.S3.Bucket, fsys.Prefix())
		defer src.Close()

		// A Broadcaster, not src.Signals() directly: the FUSE watch
		// dispatch and locking's acquire accelerator both need every
		// signal, and a Source's channel has one reader.
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
	// Nothing an as-of mount shows can change, so its names and
	// attributes can be trusted as long as it's mounted, and an open has
	// nothing to revalidate.
	cacheFor := *metaCache
	if view != nil {
		if !metaCacheSet {
			cacheFor = 24 * time.Hour
		}
	} else {
		rootOpts = append(rootOpts, fuseserver.WithMetaCache(cacheFor))
	}
	root := fuseserver.Root(fsys, fuseSignals, rootOpts...)
	source := *fsName
	if *driveID != "" {
		source = *driveID
	}
	mountOpts := &fs.Options{
		MountOptions: fuseMountOptions(source, *debug, *allowOther, view != nil || driveReadOnly || *readOnly, acls, kernelFlags{dev: *devices, suid: *setuid, noexec: *noExec}),
		// Without it, go-fuse reports a stored mode 0000 as 0644 or
		// 0755.
		NullPermissions: true,
	}
	fuseserver.MetaCacheTimeouts(mountOpts, cacheFor)
	server, err := fuseserver.Mount(mountpoint, root, mountOpts)
	if err != nil {
		log.Fatalf("mount %s: %v", mountpoint, err)
	}

	if view != nil {
		log.Printf("icbfs %q mounted read-only at %s as of %s (%s)", *fsName, mountpoint, view.At().Format(time.RFC3339Nano), conn)
	} else {
		log.Printf("icbfs %q mounted at %s (%s)", *fsName, mountpoint, conn)
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

// fuseMountOptions: source is what /proc/mounts shows as the mount's
// source (the filesystem's name, or the drive's), and the type is
// fuse.icbfs.
// kernelFlags are mount flags the kernel applies: device nodes, setuid
// bits, and executing files.
type kernelFlags struct{ dev, suid, noexec bool }

func fuseMountOptions(source string, debug, allowOther, readOnly, acls bool, kf kernelFlags) fuse.MountOptions {
	options := []string{"default_permissions"}
	if kf.dev {
		options = append(options, "dev")
	}
	if kf.suid {
		options = append(options, "suid")
	}
	if kf.noexec {
		options = append(options, "noexec")
	}
	if allowOther {
		options = append(options, "allow_other")
	}
	if readOnly {
		options = append(options, "ro")
	}
	return fuse.MountOptions{
		FsName: source,
		Name:   "icbfs",
		Debug:  debug,
		// default_permissions has the kernel check permissions on every
		// open, read and write, with root's bypass and supplementary
		// groups, rather than this driver doing it.
		//
		// allow_other is a separate restriction: without it the kernel
		// refuses every caller but the mounting user and root before any
		// mode check. -allow-other turns it on.
		Options: options,
		// EnableLocks: without it the kernel never sends lock requests to
		// this driver, and uses its own local lock table instead, which
		// other machines can't see.
		EnableLocks: true,
		// EnableAcl: the kernel enforces POSIX ACLs, kept as the
		// system.posix_acl_* xattrs.
		EnableAcl: acls,
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
