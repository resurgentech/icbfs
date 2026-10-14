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
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	winfsp "github.com/winfsp/go-winfsp"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/winfspserver"
)

// winfspAttribution is the exact notice WinFsp's own FLOSS exception
// requires any FLOSS project linking against it to display in its UI
// and user-facing docs, plus a link to WinFsp's repo — verified
// against WinFsp's real License.txt, not paraphrased (ROADMAP.md's
// Part F, task F10): "include the attribution... in user interface
// and documentation." This is the one binary an end user actually
// runs to get a WinFsp-backed mount, so it's shown both in --help/
// usage output and at every successful mount, not just documented in
// a file a user might never open.
const winfspAttribution = "WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos\nhttps://github.com/winfsp/winfsp"

func main() {
	fset := flag.NewFlagSet("icbfs-winfsp", flag.ExitOnError)
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: icbfs-winfsp [flags] <mountpoint, e.g. J:>")
		fset.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, winfspAttribution)
	}
	endpoint := fset.String("endpoint", "http://127.0.0.1:9000", "S3-API endpoint (MinIO)")
	bucket := fset.String("bucket", "icbfs", "bucket name (must have versioning enabled)")
	fsName := fset.String("fs", "default", "filesystem name")
	accessKey := fset.String("access-key", "minioadmin", "access key")
	secretKey := fset.String("secret-key", "minioadmin", "secret key")
	region := fset.String("region", "us-east-1", "region (ignored by MinIO, required by the SDK)")
	asOf := fset.String("as-of", "", "mount read-only as the filesystem was at this time: a date (2006-01-02, meaning the end of that day), a date-time (2006-01-02T15:04:05) in local time, or RFC 3339 with a zone. Fails if the filesystem didn't exist yet then")
	caseInsensitive := fset.Bool("case-insensitive", true, "case-insensitive name lookup, following the standard NTFS/Samba/WSL2 pattern (ARCHITECTURE.md's Windows compatibility section) — storage itself always stays case-sensitive/case-preserving regardless of this flag")
	cacheDir := fset.String("cache-dir", "", "local disk directory for the file-content cache (ROADMAP.md's Part G, task G3); empty (the default) uses a fresh os.TempDir() subdirectory")
	cacheSize := fset.Uint64("cache-size", 512<<20, "maximum total bytes the local disk cache (-cache-dir) may hold before evicting the least-recently-used cached file")
	if err := fset.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if fset.NArg() != 1 {
		fset.Usage()
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
	var store objstore.Store = objstore.NewS3Store(client, *bucket)

	var view *objstore.AsOf
	if *asOf != "" {
		at, err := icbfs.ParseAsOf(*asOf, time.Local)
		if err != nil {
			log.Fatal(err)
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
	var attrs uint32
	if view != nil {
		if err := fsys.BootstrapReadOnly(ctx); err != nil {
			view.Close()
			log.Fatalf("filesystem %q as of %s: %v", *fsName, view.At().Format(time.RFC3339), err)
		}
		attrs = winfsp.FspFSAttributeReadOnlyVolume
	} else if err := fsys.Attach(ctx); err != nil {
		// Mounts never create a filesystem: icbfs-cli fs create, or the
		// drive API, does.
		log.Fatalf("filesystem %q: %v", *fsName, err)
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
		// icbfs's storage is always case-preserving (exactly what was
		// typed at create time is what's stored), regardless of
		// *caseInsensitive — that flag only loosens *lookup*, per
		// winfspserver.Root's own doc comment. go-winfsp defaults this
		// to false ("filesystem does not preserve cases"), which told
		// the kernel to normalize reported names instead of returning
		// the real on-disk case — confirmed via winfsp-tests'
		// getfileinfo_test (--external) reporting "\FILE0" back for a
		// file created as "\file0".
		winfsp.CasePreserveNames(true),
		// A read-only volume for an as-of mount, so Windows itself
		// refuses writes before they reach the driver.
		winfsp.Attributes(attrs),
	)
	if err != nil {
		if view != nil {
			view.Close()
		}
		log.Fatalf("mount %s: %v", mountpoint, err)
	}
	defer server.Unmount()

	if view != nil {
		log.Printf("icbfs %q mounted read-only at %s as of %s (bucket %s via %s)", *fsName, mountpoint, view.At().Format(time.RFC3339Nano), *bucket, *endpoint)
	} else {
		log.Printf("icbfs %q mounted at %s (bucket %s via %s)", *fsName, mountpoint, *bucket, *endpoint)
	}
	log.Println(winfspAttribution)
	// winfsp.Mount's dispatcher runs its own background goroutines;
	// block here until Ctrl+C (or SIGTERM), then unmount and run the
	// deferred cleanup — an as-of mount's version cache. A process
	// killed outright skips this; the next as-of mount removes its cache.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	<-sigs
}
