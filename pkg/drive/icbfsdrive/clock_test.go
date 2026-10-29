package icbfsdrive

import (
	"context"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// skewedClock is a store whose clock runs off from the real one by by,
// as a store's clock may run off from the caller's.
type skewedClock struct {
	objstore.Store
	by time.Duration
}

func (s skewedClock) ServerTime(ctx context.Context) (time.Time, error) {
	t, err := s.Store.ServerTime(ctx)
	return t.Add(s.by), err
}

// TestStoreTime: the drive's clock is the store's (its Date header),
// whole seconds, and not this program's.
func TestStoreTime(t *testing.T) {
	store := miniotest.Store(t)
	d := newDrive(t, store, icbfs.Settings{Name: "clock", Size: 1 << 30, RootMode: 0o755}, "")
	var c drive.Clock = d
	ctx := context.Background()
	got, err := c.StoreTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	header, err := store.ServerTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nanosecond() != 0 {
		t.Fatalf("StoreTime %v isn't whole seconds", got)
	}
	if diff := header.Sub(got); diff < -time.Second || diff > time.Second {
		t.Fatalf("StoreTime %v; the store's Date header %v", got, header)
	}

	// A store whose clock is hours off this program's: the drive follows
	// the store.
	const skew = 3 * time.Hour
	sd := newDrive(t, skewedClock{Store: store, by: skew}, icbfs.Settings{Name: "clock", Size: 1 << 30, RootMode: 0o755}, "")
	got, err = sd.StoreTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if off := got.Sub(time.Now()); off < skew-time.Minute || off > skew+time.Minute {
		t.Fatalf("StoreTime %v is %v from this program's clock; the store's runs %v ahead", got, off, skew)
	}
}
