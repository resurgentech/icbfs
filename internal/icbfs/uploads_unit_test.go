package icbfs

import (
	"bytes"
	"errors"
	"testing"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// TestUploadsFitTheStores (T82-10): a staged part of UploadPartSize is one
// part of the file on S3 (UploadPartCopy's 5 MiB minimum, 10,000 parts)
// and on Azure (one block each, 50,000 at most), whatever the
// filesystem's own part size; a store with a larger minimum refuses.
func TestUploadsFitTheStores(t *testing.T) {
	s3 := objstore.PartLimits{Min: 5 << 20, Max: 5 << 30, Count: 10000}
	azure := objstore.PartLimits{Min: 1, Max: 4000 << 20, Count: 50000}
	for name, lim := range map[string]objstore.PartLimits{"s3": s3, "azure": azure} {
		if err := uploadsFit(lim); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := uploadsFit(objstore.PartLimits{Min: 8 << 20, Max: 5 << 30, Count: 10000}); !errors.Is(err, ErrUploadsUnsupported) {
		t.Errorf("a store with an 8 MiB minimum: %v, want ErrUploadsUnsupported", err)
	}
	if err := uploadsFit(objstore.PartLimits{Min: 1, Max: 5 << 30, Count: 1000}); !errors.Is(err, ErrUploadsUnsupported) {
		t.Errorf("a store of 1,000 parts: %v, want ErrUploadsUnsupported", err)
	}
}

// TestUploadChecksumState: the checksums made a part at a time, the state
// carried in each part's metadata, are the whole content's.
func TestUploadChecksumState(t *testing.T) {
	data := bytes.Repeat([]byte("icbfs-upload "), 100000)
	want, err := ChecksumsOf(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var md map[string]string
	const step = 333333
	for off := 0; off < len(data); off += step {
		h := NewChecksummer()
		if md != nil {
			if err := h.restore(md); err != nil {
				t.Fatal(err)
			}
		}
		h.Write(data[off:min(off+step, len(data))])
		if md, err = h.state(); err != nil {
			t.Fatal(err)
		}
		for k, v := range md {
			if len(k)+len(v) > 300 {
				t.Errorf("metadata %s is %d bytes", k, len(v))
			}
		}
	}
	h := NewChecksummer()
	if err := h.restore(md); err != nil {
		t.Fatal(err)
	}
	if got := h.Sum(); !bytes.Equal(got.MD5, want.MD5) || !bytes.Equal(got.SHA256, want.SHA256) {
		t.Fatalf("chained checksums %x %x, want %x %x", got.MD5, got.SHA256, want.MD5, want.SHA256)
	}
	if err := NewChecksummer().restore(map[string]string{}); !errors.Is(err, ErrUploadParts) {
		t.Fatalf("no state: %v", err)
	}
}

func TestCheckUploadID(t *testing.T) {
	for _, ok := range []string{"0123456789abcdef", "u-2f9c0d1e-aaaa-bbbb"} {
		if err := CheckUploadID(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "short", "UPPERCASE0123456", "with/slash012345", "../../0123456789"} {
		if err := CheckUploadID(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}
