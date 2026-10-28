//go:build windows

// Package winfspcmd is icbfs-winfsp: an icbfs filesystem mounted on
// Windows via WinFsp, the Windows counterpart to cmd/icbfs's FUSE mount.
// cmd/icbfs-winfsp is Main with this module's store flags; a host with a
// store of its own (storeflags.Store) builds the same mount around it.
package winfspcmd

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	winfsp "github.com/winfsp/go-winfsp"

	"github.com/resurgentech/icbfs/internal/buildinfo"
	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/winfspserver"
	"github.com/resurgentech/icbfs/pkg/storeflags"
)

// winfspAttribution is the notice WinFsp's FLOSS exception requires in a
// program's user interface and documentation ("include the attribution...
// in user interface and documentation"), with a link. It is printed in the
// usage text and at every mount.
const winfspAttribution = "WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos\nhttps://github.com/winfsp/winfsp"

// Main runs the mount with args (the program's arguments, without its
// name) and the store flags newStore makes, until interrupted, and
// returns the exit status.
func Main(args []string, newStore func() storeflags.Store) int {
	fset := flag.NewFlagSet("icbfs-winfsp", flag.ExitOnError)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: icbfs-winfsp [flags] <mountpoint, e.g. J:>")
		fset.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, winfspAttribution)
	}
	conn := newStore()
	conn.AddFlags(fset)
	fsName := fset.String("fs", "default", "filesystem name")
	asOf := fset.String("as-of", "", "mount read-only as the filesystem was at this time: a date (2006-01-02, meaning the end of that day), a date-time (2006-01-02T15:04:05) in local time, or RFC 3339 with a zone; or a version name (icbfs-cli names). Fails if the filesystem didn't exist yet then")
	caseInsensitive := fset.Bool("case-insensitive", true, "look names up case-insensitively, as NTFS does; names are always stored as given")
	history := storeflags.AddHistory(fset)
	cacheDir := fset.String("cache-dir", "", "local disk directory for open files and the read cache (default: a new directory under the system temp directory)")
	cacheSize := fset.Uint64("cache-size", 512<<20, "maximum total bytes the local disk cache (-cache-dir) may hold before evicting the least-recently-used cached file")
	atime := fset.String("atime", "hourly", "when reads and folder listings record access times: hourly (when the recorded one is over an hour old, as NTFS does), relatime (when it's no newer than the last change, or a day old), strict (every read and listing), or noatime (never)")
	noDirAtime := fset.Bool("nodiratime", false, "don't record access times for folders")
	version := fset.Bool("version", false, "print which build this is and exit")
	metaCache := fset.Duration("meta-cache", 5*time.Second, "how long Windows trusts file and directory information, as the Linux mount's -meta-cache: other machines' changes can take this long to show in a listing or a file's details; an open still checks the store (0: trust nothing; as-of mounts default to without limit)")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if *version {
		fmt.Println(buildinfo.Version())
		return 0
	}
	if fset.NArg() != 1 {
		fset.Usage()
		return 2
	}
	if *metaCache < 0 || *metaCache > time.Duration(math.MaxUint32-1)*time.Millisecond {
		return fail("-meta-cache %v: from 0 to %v", *metaCache, time.Duration(math.MaxUint32-1)*time.Millisecond)
	}
	metaCacheSet := false
	fset.Visit(func(f *flag.Flag) { metaCacheSet = metaCacheSet || f.Name == "meta-cache" })
	mountpoint := fset.Arg(0)

	ctx := context.Background()
	if err := conn.Check(); err != nil {
		return fail("%v", err)
	}
	store, err := conn.Open(ctx)
	if err != nil {
		return fail("%v", err)
	}

	var view *objstore.AsOf
	if *asOf != "" {
		at, err := icbfs.ResolveAsOf(ctx, store, *fsName, *asOf, time.Local)
		if err != nil {
			return fail("%v", err)
		}
		hist, err := history.Open(ctx, store, conn)
		if err != nil {
			return fail("%v", err)
		}
		defer hist.Close()
		// What a purge has erased since the cache last looked goes from it.
		if live := icbfs.New(store, *fsName); live.Attach(ctx) == nil {
			if _, err := icbfs.ForgetChangedPast(ctx, live, hist); err != nil {
				return fail("history cache: %v", err)
			}
		}
		view, err = objstore.NewAsOf(ctx, hist, at)
		if err != nil {
			return fail("%v", err)
		}
		store = view
	}

	fsys := icbfs.New(store, *fsName)
	if *cacheDir != "" {
		fsys.SetCacheOptions(*cacheDir, int64(*cacheSize))
	}
	switch *atime {
	case "hourly":
		fsys.SetAtime(icbfs.AtimeHourly, !*noDirAtime)
	case "relatime":
		fsys.SetAtime(icbfs.AtimeRelative, !*noDirAtime)
	case "strict":
		fsys.SetAtime(icbfs.AtimeStrict, !*noDirAtime)
	case "noatime":
		fsys.SetAtime(icbfs.AtimeNever, false)
	default:
		return fail("-atime %q: hourly, relatime, strict or noatime", *atime)
	}
	var attrs uint32
	if view != nil {
		if err := fsys.BootstrapReadOnly(ctx); err != nil {
			return fail("filesystem %q as of %s: %v", *fsName, view.At().Format(time.RFC3339), err)
		}
		attrs = winfsp.FspFSAttributeReadOnlyVolume
	} else if err := fsys.Attach(ctx); err != nil {
		// Mounts never create a filesystem: icbfs-cli fs create, or the
		// drive API, does.
		return fail("filesystem %q: %v", *fsName, err)
	}

	// The metadata cache. WinFsp keeps no names of its own (every open
	// passes the path, which the driver resolves, through its name cache
	// below), so WinFsp's timeout covers file and directory information,
	// and with it the kernel's caching of file data. Flushing and purging that at the last handle's cleanup
	// keeps content close-to-open: what was written reaches the driver
	// before the close stores it, and the next open reads again.
	fileInfoTimeout := uint32(*metaCache / time.Millisecond)
	if view != nil && !metaCacheSet {
		fileInfoTimeout = math.MaxUint32 // nothing an as-of mount shows changes
	}
	if fileInfoTimeout != 0 {
		attrs |= winfsp.FspFSAttributeFlushAndPurgeOnCleanup
	}
	// The driver's own name cache, for the same time: WinFsp passes whole
	// paths, so without it every open walks its path from the store.
	nameCache := *metaCache
	if view != nil && !metaCacheSet {
		nameCache = 24 * time.Hour
	}

	// winfsp.CaseSensitive and winfspserver.Root's caseInsensitive must
	// agree; see Root's doc comment.
	server, err := winfsp.Mount(
		winfspserver.Root(fsys, *caseInsensitive, winfspserver.WithMetaCache(nameCache)),
		mountpoint,
		winfsp.FileSystemName("icbfs"),
		winfsp.CaseSensitive(!*caseInsensitive),
		// Storage always keeps names as given; -case-insensitive only
		// loosens lookup. go-winfsp's default (names not preserved) made
		// the kernel report normalized names: "\FILE0" for a file created
		// as "\file0" (winfsp-tests' getfileinfo_test).
		winfsp.CasePreserveNames(true),
		// A read-only volume for an as-of mount, so Windows itself
		// refuses writes before they reach the driver.
		winfsp.Attributes(attrs),
		winfsp.FileInfoTimeout(fileInfoTimeout),
	)
	if err != nil {
		return fail("mount %s: %v", mountpoint, err)
	}
	defer server.Unmount()

	if view != nil {
		log.Printf("icbfs %q mounted read-only at %s as of %s (%s)", *fsName, mountpoint, view.At().Format(time.RFC3339Nano), conn)
	} else {
		log.Printf("icbfs %q mounted at %s (%s)", *fsName, mountpoint, conn)
	}
	log.Println(winfspAttribution)
	// winfsp.Mount's dispatcher runs its own background goroutines;
	// block here until Ctrl+C (or SIGTERM), then unmount and run the
	// deferred cleanup — an as-of mount's version cache. A process
	// killed outright skips this; the next as-of mount removes its cache.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	<-sigs
	return 0
}

// fail logs why the mount can't go on, for Main's exit status.
func fail(format string, a ...any) int {
	log.Printf(format, a...)
	return 1
}
