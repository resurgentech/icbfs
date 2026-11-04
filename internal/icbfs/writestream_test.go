package icbfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// failAt is a body that fails after n bytes of r.
type failAt struct {
	r io.Reader
	n int64
}

var errCut = errors.New("the body was cut off")

func (f *failAt) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errCut
	}
	if int64(len(p)) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= int64(n)
	return n, err
}

// TestWriteStreamUnknownSize: a body of unknown length (UnknownSize) goes
// up in parts as it arrives, whatever its length: empty, short of a part,
// at a part's end, past one; one that fails, in its first part or a
// later one, writes nothing.
func TestWriteStreamUnknownSize(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	fsys := New(store, "unknown")
	fsys.SetCreationMaxFileSize(8 << 30)
	if err := fsys.Bootstrap(ctx, 1<<40, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	part := fsys.partSize()
	key, _, etag, err := fsys.Create(ctx, fsys.RootKey(), "f", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2*part+12345)
	for i := range data {
		data[i] = byte(i * 7 % 253)
	}
	for _, n := range []int64{1234, 0, part, 2*part + 12345, part + 1} {
		obj, err := fsys.WriteStream(ctx, key, bytes.NewReader(data[:n]), UnknownSize, etag)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		got, _, _, err := fsys.ReadFile(ctx, key)
		if err != nil || !bytes.Equal(got, data[:n]) || obj.Size != n {
			t.Fatalf("%d bytes: read %d bytes (%v), object size %d", n, len(got), err, obj.Size)
		}
		etag = obj.ETag
	}
	history, err := store.History(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int64{10, part + 10} {
		if _, err := fsys.WriteStream(ctx, key, &failAt{r: bytes.NewReader(data), n: cut}, UnknownSize, etag); !errors.Is(err, errCut) {
			t.Fatalf("cut at %d: %v, want the body's error", cut, err)
		}
	}
	if now, err := store.History(ctx, key); err != nil || len(now) != len(history) {
		t.Fatalf("history after the cut bodies: %d entries (%v), want %d", len(now), err, len(history))
	}
	if _, err := fsys.WriteStream(ctx, key, bytes.NewReader(nil), -2, etag); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("size -2: %v, want ErrInvalidArgument", err)
	}
}
