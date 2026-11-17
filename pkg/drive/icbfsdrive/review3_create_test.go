package icbfsdrive

// Review 3 of T82-10, probe 1: create()'s handling of a lost condition
// (412) on the content write of a file it just made. Brought in by round
// 4, which changed what it probes: create() now writes a new file's
// content first, to a key no folder names (no condition: nothing is
// there), and makes the name only once it has landed. So the store
// wrapper below also takes a PutParts to a key with nothing at it (the new
// file's content), and the single-writer cases now require that a caller
// told of a failure leaves nothing a reader sees, and one told of success
// the whole file.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil/miniotest"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// r3Store wraps a store's conditional content writes over an EMPTY current
// object (what create() writes over: the empty version Create made).
//
//	mode "enforce": the first such write is held until release, then
//	  refused with a 412 if the key's etag is no longer ifMatch (what AWS
//	  does, and this MinIO does not on CompleteMultipartUpload);
//	mode "inject": refused with a 412, nothing written, `times` times;
//	mode "landed": written, then answered 412, `times` times (a lost
//	  answer whose retry, or s3store.lostRace, sees the key changed).
type r3Store struct {
	objstore.Store
	mode          string
	times         int32
	hits          atomic.Int32
	armed         atomic.Bool
	once          sync.Once
	held, release chan struct{}
}

func (s *r3Store) intercept(ctx context.Context, key, ifMatch string, parts bool, write func() (*objstore.Object, error)) (*objstore.Object, error) {
	if !s.armed.Load() {
		return write()
	}
	head, err := s.Store.Head(ctx, key)
	switch {
	case ifMatch == "" && parts && objstore.IsNotFound(err):
		// round 4: a new file's content, to its new key
	case ifMatch == "" || err != nil || head.Size != 0 || head.ETag != ifMatch:
		return write()
	}
	switch s.mode {
	case "enforce":
		if s.hits.Add(1) == 1 {
			close(s.held)
			<-s.release
			now, err := s.Store.Head(ctx, key)
			if ifMatch != "" && err == nil && now.ETag != ifMatch {
				return nil, fmt.Errorf("emulated: %w", objstore.ErrPreconditionFailed)
			}
		}
		return write()
	case "inject":
		if s.hits.Add(1) <= s.times {
			return nil, fmt.Errorf("injected: %w", objstore.ErrPreconditionFailed)
		}
		return write()
	case "landed":
		obj, err := write()
		if err == nil && s.hits.Add(1) <= s.times {
			return nil, fmt.Errorf("landed, answer lost: %w", objstore.ErrPreconditionFailed)
		}
		return obj, err
	}
	return write()
}

func (s *r3Store) Put(ctx context.Context, key string, body io.Reader, md map[string]string, ifMatch string) (*objstore.Object, error) {
	return s.intercept(ctx, key, ifMatch, false, func() (*objstore.Object, error) { return s.Store.Put(ctx, key, body, md, ifMatch) })
}

func (s *r3Store) PutParts(ctx context.Context, key string, parts []objstore.Part, md map[string]string, ifMatch string) (*objstore.Object, error) {
	return s.intercept(ctx, key, ifMatch, true, func() (*objstore.Object, error) { return s.Store.PutParts(ctx, key, parts, md, ifMatch) })
}

func r3Drive(t *testing.T, mode string, times int32) (*Drive, *r3Store) {
	st := &r3Store{Store: miniotest.Store(t), mode: mode, times: times, held: make(chan struct{}), release: make(chan struct{})}
	d := newDrive(t, st, icbfs.Settings{Name: "r3", Size: 1 << 34, RootMode: 0o755, MaxFileSize: 8 << 30}, "/")
	return d, st
}

// r3State says what a reader sees at p: absent, or its size, content kind
// and how many versions it has.
func r3State(t *testing.T, ctx context.Context, d *Drive, p string) string {
	t.Helper()
	it, err := d.Stat(ctx, drive.ByPath(p), drive.At{})
	if err != nil {
		return "absent (" + err.Error() + ")"
	}
	vs, _ := d.Versions(ctx, drive.ByPath(p))
	sizes := []int64{}
	for _, v := range vs {
		sizes = append(sizes, v.Size)
	}
	got := readAll(t, ctx, d, p)
	kind := "other"
	switch {
	case len(got) == 0:
		kind = "EMPTY"
	case got[0] == 'a' && bytes.Count(got, []byte("a")) == len(got):
		kind = "a"
	case got[0] == 'b' && bytes.Count(got, []byte("b")) == len(got):
		kind = "b"
	}
	return fmt.Sprintf("present: size %d, content %s, versions %v", it.Size, kind, sizes)
}

const r3Big = 9<<20 + 777 // past one part: PutParts

// Two writers of one new file; A's content write is refused because B wrote
// the file A made (emulated AWS 412). Old code: A drops the file holding B's
// write. New: the file stays.
func TestReview3CreateRaceTwoWriters(t *testing.T) {
	d, st := r3Drive(t, "enforce", 0)
	ctx := system()
	a, b := bytes.Repeat([]byte("a"), r3Big), bytes.Repeat([]byte("b"), 500)
	st.armed.Store(true)
	errA := make(chan error, 1)
	go func() {
		_, err := d.WriteFrom(ctx, "/new.bin", bytes.NewReader(a), int64(len(a)), drive.Condition{}, drive.WriteOptions{})
		errA <- err
	}()
	select {
	case <-st.held:
	case <-time.After(60 * time.Second):
		t.Fatal("A's content write never came")
	}
	if _, err := d.WriteFrom(ctx, "/new.bin", bytes.NewReader(b), int64(len(b)), drive.Condition{}, drive.WriteOptions{}); err != nil {
		t.Fatalf("B: %v", err)
	}
	t.Logf("after B answered: %s", r3State(t, ctx, d, "/new.bin"))
	close(st.release)
	err := <-errA
	t.Logf("A answered: %v", err)
	s := r3State(t, ctx, d, "/new.bin")
	t.Logf("after both: %s", s)
	if s[:6] == "absent" {
		t.Fatalf("B's write answered success, and the file is gone")
	}
	if err == nil && !strings.Contains(s, "content a,") {
		t.Fatalf("A answered success, and the file is not A's: %s", s)
	}
}

func r3Single(t *testing.T, mode string, times int32, stream bool) {
	d, st := r3Drive(t, mode, times)
	ctx := system()
	a := bytes.Repeat([]byte("a"), r3Big)
	st.armed.Store(true)
	var err error
	if stream {
		// a known-size body, sent as it arrives (a plain HTTP PUT with a
		// Content-Length): writeSource{r, size}
		_, err = d.Write(ctx, "/one.bin", io.MultiReader(bytes.NewReader(a)), drive.Condition{}, drive.WriteOptions{Size: int64(len(a))})
	} else {
		_, err = d.WriteFrom(ctx, "/one.bin", bytes.NewReader(a), int64(len(a)), drive.Condition{}, drive.WriteOptions{})
	}
	t.Logf("mode %s x%d stream=%v: the caller is told: %v", mode, times, stream, err)
	seen := r3State(t, ctx, d, "/one.bin")
	t.Logf("a reader then sees: %s", seen)
	switch {
	case err != nil && seen[:6] != "absent":
		t.Errorf("the caller was told %v, and a reader sees %s", err, seen)
	case err == nil && !strings.HasPrefix(seen, fmt.Sprintf("present: size %d, content a,", r3Big)):
		t.Errorf("the caller was told success, and a reader sees %s", seen)
	}
	page, _ := d.List(ctx, drive.ByPath("/"), drive.ListOptions{})
	for _, it := range page.Items {
		t.Logf("  listed: %s size %d", it.Name, it.Size)
	}
}

func TestReview3CreateInject412OnceFrom(t *testing.T)   { r3Single(t, "inject", 1, false) }
func TestReview3CreateInject412AlwaysFrom(t *testing.T) { r3Single(t, "inject", 99, false) }
func TestReview3CreateInject412OnceStream(t *testing.T) { r3Single(t, "inject", 1, true) }
func TestReview3CreateLanded412OnceFrom(t *testing.T)   { r3Single(t, "landed", 1, false) }
func TestReview3CreateLanded412OnceStream(t *testing.T) { r3Single(t, "landed", 1, true) }
