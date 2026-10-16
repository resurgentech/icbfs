package icbfsdrive

import (
	"context"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// TestChangesIncludeWritesFromElsewhere: with pkgicbfs.WatchMinIO feeding
// Notify, a file written straight through the filesystem, as a mount
// writes, shows up on the drive's change stream.
func TestChangesIncludeWritesFromElsewhere(t *testing.T) {
	store, srv := miniotest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := pkgicbfs.CreateFilesystem(ctx, store, pkgicbfs.Settings{Name: "watched", Size: 1 << 30, RootMode: 0o755}); err != nil {
		t.Fatal(err)
	}
	d, err := New(ctx, Config{Store: store, Filesystem: "watched", Root: "/", ID: "watched"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	signals, err := pkgicbfs.WatchMinIO(ctx, pkgicbfs.S3{Endpoint: srv.Endpoint, Bucket: srv.Bucket, AccessKey: srv.AccessKey, SecretKey: srv.SecretKey}, "watched")
	if err != nil {
		t.Fatal(err)
	}
	d.Notify(signals)
	changes, err := d.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the notification long poll says nothing when it's up

	// "A mount": another Filesystem on the same bucket.
	other := icbfs.New(store, "watched")
	if err := other.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := other.Create(ctx, other.RootKey(), "from-a-mount.txt", 0o644, 0, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(15 * time.Second):
		t.Fatal("no change event for a write made outside the drive")
	}
}
