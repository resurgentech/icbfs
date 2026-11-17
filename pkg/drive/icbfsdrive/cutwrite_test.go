package icbfsdrive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// cutStore holds every PutParts of n bytes until its context ends, then
// fails as a cut copy does: a write cut in the middle of its content.
type cutStore struct {
	objstore.Store
	n       int64
	entered chan struct{}
}

func (s *cutStore) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	var total int64
	for _, p := range parts {
		total += p.Size
	}
	if total == s.n {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	return s.Store.PutParts(ctx, key, parts, md, ifMatch)
}

// TestCutWriteLeavesNoName (T82-10 round 4, review 3's N1): a write cut
// in the middle of its content's copy (a stop cancels its context) leaves
// no name for a new file, and an existing file as it was, its previous
// version current. Each way a large body goes: content the caller holds
// (WriteFrom: an upload's assembly takes this path's create too), a body
// of known length sent as it arrives (a plain PUT), and one of unknown
// length spooled to a file.
func TestCutWriteLeavesNoName(t *testing.T) {
	const n = 9<<20 + 777 // past one 8 MiB part: PutParts
	body := bytes.Repeat([]byte("c"), n)
	ways := map[string]func(ctx context.Context, d *Drive, p string) error{
		"from": func(ctx context.Context, d *Drive, p string) error {
			_, err := d.WriteFrom(ctx, p, bytes.NewReader(body), n, drive.Condition{}, drive.WriteOptions{})
			return err
		},
		"known": func(ctx context.Context, d *Drive, p string) error {
			_, err := d.Write(ctx, p, io.MultiReader(bytes.NewReader(body)), drive.Condition{}, drive.WriteOptions{Size: n})
			return err
		},
		"spooled": func(ctx context.Context, d *Drive, p string) error {
			_, err := d.Write(ctx, p, io.MultiReader(bytes.NewReader(body)), drive.Condition{}, drive.WriteOptions{})
			return err
		},
	}
	for way, write := range ways {
		for _, existing := range []bool{false, true} {
			name := way + "/new"
			if existing {
				name = way + "/existing"
			}
			t.Run(name, func(t *testing.T) {
				store := &cutStore{Store: miniotest.Store(t), n: n, entered: make(chan struct{}, 1)}
				d := newDrive(t, store, icbfs.Settings{Name: "cut", Size: 1 << 34, RootMode: 0o755, MaxFileSize: 8 << 30}, "/")
				sys := system()
				before := []byte("the version before")
				if existing {
					if _, err := d.WriteFrom(sys, "/f.bin", bytes.NewReader(before), int64(len(before)), drive.Condition{}, drive.WriteOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancelCause(sys)
				errc := make(chan error, 1)
				go func() { errc <- write(ctx, d, "/f.bin") }()
				select {
				case <-store.entered:
				case <-time.After(60 * time.Second):
					t.Fatal("the content's copy never began")
				}
				cancel(errors.New("stopped by admin"))
				var err error
				select {
				case err = <-errc:
				case <-time.After(30 * time.Second):
					t.Fatal("the cut write never returned")
				}
				if err == nil {
					t.Fatal("the cut write answered success")
				}
				it, serr := d.Stat(sys, drive.ByPath("/f.bin"), drive.At{})
				page, lerr := d.List(sys, drive.ByPath("/"), drive.ListOptions{})
				if lerr != nil {
					t.Fatal(lerr)
				}
				if !existing {
					if serr == nil {
						t.Fatalf("after the cut a reader finds /f.bin, %d bytes (%v)", it.Size, err)
					}
					if len(page.Items) != 0 {
						t.Fatalf("after the cut the folder lists %+v", page.Items)
					}
					return
				}
				if serr != nil {
					t.Fatalf("the existing file is gone after the cut: %v", serr)
				}
				if got := readAll(t, sys, d, "/f.bin"); !bytes.Equal(got, before) {
					t.Fatalf("the existing file holds %d bytes after the cut, want its previous version's %d", len(got), len(before))
				}
				vs, err := d.Versions(sys, drive.ByPath("/f.bin"))
				if err != nil || len(vs) != 1 {
					t.Fatalf("versions after the cut: %d (%v), want the one before", len(vs), err)
				}
			})
		}
	}
}
