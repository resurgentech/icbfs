package icbfsdrive

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// holdSized is a store that holds the first Put of a body of size n until
// release: a write whose content is slow to land.
type holdSized struct {
	objstore.Store
	n             int64
	armed         atomic.Bool
	once          sync.Once
	held, release chan struct{}
}

func (h *holdSized) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	if sz, ok := body.(interface{ Size() int64 }); ok && h.armed.Load() && sz.Size() == h.n {
		h.once.Do(func() { close(h.held); <-h.release })
	}
	return h.Store.Put(ctx, key, body, md, ifMatch)
}

func (h *holdSized) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	var total int64
	for _, p := range parts {
		total += p.Size
	}
	if h.armed.Load() && total == h.n {
		h.once.Do(func() { close(h.held); <-h.release })
	}
	return h.Store.PutParts(ctx, key, parts, md, ifMatch)
}

// TestWriteRaceKeepsTheOtherWritersFile (T82-10 round 3, review 2's B4
// "done but absent" case): two writes of one new file. A makes the file
// and is slow to send its content; B finds the file made, writes over it
// and lands first; A's content is then refused (412). A used to drop the
// file it had made, with B's content in it: the file was gone though B
// had written it. Round 3 made A leave it and write over it in turn.
// Since round 4 A writes its content first, to a key no folder names, and
// names it only once it has landed: B made the name meanwhile, so A's
// link meets it (ErrExists), drops its own content and writes over B's
// file. Either way the file is there, whole, A's or B's.
func TestWriteRaceKeepsTheOtherWritersFile(t *testing.T) {
	// A is written in parts (9 MiB at an 8 GiB max file size: 8 MiB
	// parts), so its condition is checked at CompleteMultipartUpload, as
	// in review 2's run.
	const an = 9<<20 + 777
	store := &holdSized{Store: miniotest.Store(t), n: an, held: make(chan struct{}), release: make(chan struct{})}
	d := newDrive(t, store, icbfs.Settings{Name: "race", Size: 1 << 34, RootMode: 0o755, MaxFileSize: 8 << 30}, "/")
	ctx := system()
	a, b := bytes.Repeat([]byte("a"), an), bytes.Repeat([]byte("b"), 500)
	store.armed.Store(true)
	errA := make(chan error, 1)
	go func() {
		_, err := d.WriteFrom(ctx, "/new.bin", bytes.NewReader(a), int64(len(a)), drive.Condition{}, drive.WriteOptions{})
		errA <- err
	}()
	select {
	case <-store.held:
	case <-time.After(30 * time.Second):
		t.Fatal("A's content never went to the store")
	}
	bi, err := d.WriteFrom(ctx, "/new.bin", bytes.NewReader(b), int64(len(b)), drive.Condition{}, drive.WriteOptions{})
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	t.Logf("B wrote %s version %+v", bi.ID, bi.Version)
	close(store.release)
	if err := <-errA; err != nil {
		t.Fatalf("A: %v", err)
	}
	vs, verr := d.Versions(ctx, drive.ByPath("/new.bin"))
	t.Logf("versions of /new.bin: %d (%v)", len(vs), verr)
	if verr != nil || len(vs) < 1 {
		t.Fatalf("the file has no version: %v", verr)
	}
	got := readAll(t, ctx, d, "/new.bin")
	if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
		t.Fatalf("the file holds %d bytes of neither write", len(got))
	}
}
