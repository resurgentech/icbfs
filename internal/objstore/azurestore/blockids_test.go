package azurestore_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/azurestore"
)

// blockRules is Azure Blob Storage for one blob's blocks, as Microsoft
// documents them (Put Block's and Put Block List's remarks): a Put Block
// whose id is of another length than any uncommitted block's is 400
// InvalidBlobOrBlock; Put Block List commits the listed blocks, makes one
// version, and drops every uncommitted block it does not list; Get Block
// List answers the uncommitted ones. And Get Blob Properties of the
// version committed.
type blockRules struct {
	mu          sync.Mutex
	uncommitted map[string][]byte
	committed   [][]byte
	versions    int
	puts, lists int // Put Block and Get Block List requests
}

func (f *blockRules) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPut && q.Get("comp") == "block":
		f.puts++
		id := q.Get("blockid")
		for have := range f.uncommitted {
			if len(have) != len(id) {
				w.Header().Set("x-ms-error-code", "InvalidBlobOrBlock")
				http.Error(w, "<Error><Code>InvalidBlobOrBlock</Code></Error>", http.StatusBadRequest)
				return
			}
		}
		f.uncommitted[id] = body
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && q.Get("comp") == "blocklist":
		f.lists++
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList><CommittedBlocks/><UncommittedBlocks>`)
		for id, data := range f.uncommitted {
			b.WriteString("<Block><Name>" + id + "</Name><Size>" + itoa(len(data)) + "</Size></Block>")
		}
		b.WriteString("</UncommittedBlocks></BlockList>")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(b.String()))
	case r.Method == http.MethodPut && q.Get("comp") == "blocklist":
		var list struct {
			Latest []string `xml:"Latest"`
		}
		if err := xml.Unmarshal(body, &list); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var blob [][]byte
		for _, id := range list.Latest {
			data, ok := f.uncommitted[id]
			if !ok {
				w.Header().Set("x-ms-error-code", "InvalidBlockList")
				http.Error(w, "<Error><Code>InvalidBlockList</Code></Error>", http.StatusBadRequest)
				return
			}
			blob = append(blob, data)
		}
		f.committed = blob
		f.uncommitted = map[string][]byte{} // the unlisted ones are garbage-collected
		f.versions++
		w.Header().Set("x-ms-version-id", "2026-11-17T00:00:00.000000"+itoa(f.versions)+"Z")
		w.Header().Set("ETag", `"0x`+itoa(f.versions)+`"`)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodHead:
		w.Header().Set("Content-Length", itoa(len(bytes.Join(f.committed, nil))))
		w.Header().Set("ETag", `"0x`+itoa(f.versions)+`"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("x-ms-version-id", "2026-11-17T00:00:00.000000"+itoa(f.versions)+"Z")
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "not in the fake: "+r.Method+" "+r.URL.String(), http.StatusNotImplemented)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func newBlockRules(t *testing.T, stale ...string) (*azurestore.Store, *blockRules) {
	t.Helper()
	f := &blockRules{uncommitted: map[string][]byte{}}
	for _, raw := range stale {
		f.uncommitted[base64.StdEncoding.EncodeToString([]byte(raw))] = []byte("stale")
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	key := base64.StdEncoding.EncodeToString([]byte("not-a-real-key-not-a-real-key!!!"))
	s, err := azurestore.New(srv.URL+"/"+fakeAccount+"/", fakeAccount, key, "c")
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

// TestPutPartsOverStaleBlocks (T82-10 round 2): uncommitted blocks left on
// the blob by a write cut half way, of another id length (the SDK's
// stream upload) or of icbfs's own (a cut PutParts), do not fail a
// PutParts of uploaded parts: one version is committed, the parts' bytes
// in order, and nothing is left uncommitted. A healthy PutParts asks no
// block list.
func TestPutPartsOverStaleBlocks(t *testing.T) {
	parts := func() []objstore.Part {
		return []objstore.Part{
			{Size: 3, Body: bytes.NewReader([]byte("one"))},
			{Size: 3, Body: bytes.NewReader([]byte("two"))},
			{Size: 5, Body: bytes.NewReader([]byte("three"))},
		}
	}
	for name, tc := range map[string]struct {
		stale []string
		lists int
	}{
		"healthy":                {nil, 0},
		"another length (36)":    {[]string{"0123456789abcdef0123456789abcdef0123"}, 1},
		"another length (8)":     {[]string{"block-01", "block-02"}, 1},
		"icbfs's own (17), cut":  {[]string{"icbfs-part-000004"}, 0},
		"18 bytes: 24 in base64": {[]string{"icbfs-part-0000004"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := newBlockRules(t, tc.stale...)
			obj, err := s.PutParts(context.Background(), "k", parts(), nil, "")
			if err != nil {
				t.Fatalf("PutParts: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if got := string(bytes.Join(f.committed, nil)); got != "onetwothree" {
				t.Fatalf("committed %q", got)
			}
			if f.versions != 1 || obj.Size != 11 {
				t.Fatalf("%d versions, size %d: want 1, 11", f.versions, obj.Size)
			}
			if len(f.uncommitted) != 0 {
				t.Fatalf("%d uncommitted blocks left", len(f.uncommitted))
			}
			if f.lists != tc.lists {
				t.Fatalf("%d block lists asked, want %d", f.lists, tc.lists)
			}
		})
	}
}

// TestPutPartsStaleTooShort: stale ids too short to number the parts
// (under 5 bytes) are not matched: the error is the store's, as before.
func TestPutPartsStaleTooShort(t *testing.T) {
	s, f := newBlockRules(t, "ab")
	_, err := s.PutParts(context.Background(), "k", []objstore.Part{{Size: 3, Body: bytes.NewReader([]byte("one"))}}, nil, "")
	if err == nil {
		t.Fatal("PutParts over 2-byte stale ids succeeded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.versions != 0 || f.lists != 1 {
		t.Fatalf("%d versions, %d lists", f.versions, f.lists)
	}
}

// TestPutPartsCopiesOverStaleBlocks: the same for parts copied on the
// store's side (Put Block From URL, 8 at once): every copy fails on the
// first try, one repair is made, each is staged again under the ids found,
// and one version is committed.
func TestPutPartsCopiesOverStaleBlocks(t *testing.T) {
	s, f := newBlockRules(t, "0123456789abcdef0123456789abcdef0123")
	var parts []objstore.Part
	for i := 0; i < 20; i++ {
		parts = append(parts, objstore.Part{Size: 5 << 20, CopyKey: "src", CopyVersion: "2026-11-17T00:00:00.0000000Z"})
	}
	if _, err := s.PutParts(context.Background(), "k", parts, nil, ""); err != nil {
		t.Fatalf("PutParts of copies: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.versions != 1 || len(f.committed) != 20 || len(f.uncommitted) != 0 || f.lists != 1 {
		t.Fatalf("%d versions, %d blocks committed, %d uncommitted left, %d lists: want 1, 20, 0, 1", f.versions, len(f.committed), len(f.uncommitted), f.lists)
	}
}
