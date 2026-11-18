package icbfs

// Reviewer's test (T82-10 review, 2026-11-17, R4): how much heap
// UploadSources holds at once for an upload of N parts. Red on 153b320,
// which kept every part's Head (Object with its two checksum-state
// metadata strings) until the end; it keeps only each part's key, size and
// version, and the last part's state: held during the call at most 1.5
// times the result plus 0.25 MiB, and at 10,000 parts at most 3 MiB.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

type headStore struct {
	objstore.Store
	parts int
	md    map[string]string
	peak  func()
}

func (s *headStore) Head(_ context.Context, key string) (*objstore.Object, error) {
	if strings.HasSuffix(key, "/record") {
		return &objstore.Object{Key: key}, nil
	}
	i := strings.LastIndex(key, "/p")
	var n int
	fmt.Sscanf(key[i+2:], "%d", &n)
	if n > s.parts {
		if s.peak != nil {
			s.peak()
		}
		return nil, fmt.Errorf("head %s: %w", key, objstore.ErrNotFound)
	}
	md := make(map[string]string, len(s.md))
	for k, v := range s.md {
		md[strings.Clone(k)] = strings.Clone(v) // as the SDK hands out fresh strings
	}
	size := UploadPartSize
	return &objstore.Object{
		Key: strings.Clone(key), VersionID: fmt.Sprintf("%032x", n), ETag: fmt.Sprintf("\"%032x\"", n),
		Size: size, LastModified: time.Now(), Metadata: md,
	}, nil
}

func TestReviewUploadSourcesHeap(t *testing.T) {
	sum := NewChecksummer()
	_, _ = sum.Write(make([]byte, 12345))
	md, err := sum.state()
	if err != nil {
		t.Fatal(err)
	}
	for _, parts := range []int{52, 1000, 10000} {
		var base, at runtime.MemStats
		st := &headStore{parts: parts, md: md}
		st.peak = func() { runtime.GC(); runtime.ReadMemStats(&at) }
		f := &Filesystem{store: st, id: "0001", maxFileSize: 1 << 40}
		runtime.GC()
		runtime.ReadMemStats(&base)
		src, err := f.UploadSources(context.Background(), "0123456789abcdef0123456789abcdef", parts)
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		during := float64(int64(at.HeapAlloc)-int64(base.HeapAlloc)) / (1 << 20)
		result := float64(int64(after.HeapAlloc)-int64(base.HeapAlloc)) / (1 << 20)
		t.Logf("%5d parts: heap held during UploadSources %.2f MiB; the result alone %.2f MiB", parts, during, result)
		if during > 1.5*result+0.25 || (parts == 10000 && during > 3) {
			t.Errorf("%d parts: %.2f MiB held during the call against %.2f MiB of result: want at most 1.5 times it plus 0.25 MiB, and 3 MiB at 10,000 parts", parts, during, result)
		}
		runtime.KeepAlive(src)
	}
}
