package icbfsdrive

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore/azurestore"
	"github.com/resurgentech/icbfs/pkg/drive"
)

// T82-10 review R6: uploads in parts on a real Azure Blob Storage account,
// the copy path (StageBlockFromURL from a staged blob's version, signed by
// a SAS with the account key, or by the sign-in's token) that MinIO cannot
// run. Skipped unless ICBFS_TEST_AZURE_ACCOUNT names an account (blob
// versioning on), as the store's conformance suite is; test/azure/
// run-uploads.sh sets the variables the same way run-conformance.sh does
// (ICBFS_TEST_AZURE_KEY, or ICBFS_TEST_AZURE_AUTH=login). Each run writes
// under a prefix of its own. ICBFS_TEST_AZURE_UPLOAD_PARTS (default 200)
// is the timed complete's part count: 5 MiB each, sent from this machine.

type azureUploads struct {
	store *azurestore.Store
	cont  *container.Client
	pfx   string
}

func azureForUploads(t *testing.T) azureUploads {
	t.Helper()
	account := os.Getenv("ICBFS_TEST_AZURE_ACCOUNT")
	if account == "" {
		t.Skip("no Azure account (ICBFS_TEST_AZURE_ACCOUNT); see test/azure/run-uploads.sh")
	}
	name := os.Getenv("ICBFS_TEST_AZURE_CONTAINER")
	if name == "" {
		name = "icbfs-test"
	}
	serviceURL := "https://" + account + ".blob.core.windows.net/"
	var base *azurestore.Store
	var cont *container.Client
	var err error
	switch auth, key := os.Getenv("ICBFS_TEST_AZURE_AUTH"), os.Getenv("ICBFS_TEST_AZURE_KEY"); {
	case auth == "login":
		cred, cerr := azidentity.NewDefaultAzureCredential(nil)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if base, err = azurestore.NewWithCredential(serviceURL, cred, name); err == nil {
			cont, err = container.NewClient(serviceURL+name, cred, nil)
		}
	case auth == "" && key != "":
		if base, err = azurestore.New(serviceURL, account, key, name); err == nil {
			var cred *azblob.SharedKeyCredential
			if cred, err = azblob.NewSharedKeyCredential(account, key); err == nil {
				cont, err = container.NewClientWithSharedKeyCredential(serviceURL+name, cred, nil)
			}
		}
	default:
		t.Fatalf("ICBFS_TEST_AZURE_AUTH=%q: want login, or ICBFS_TEST_AZURE_KEY set", auth)
	}
	if err != nil {
		t.Fatal(err)
	}
	pfx := fmt.Sprintf("uploads-test/%s/%s/", time.Now().UTC().Format("20060102T150405.000"), t.Name())
	return azureUploads{store: base.WithKeyPrefix(pfx), cont: cont, pfx: pfx}
}

// uncommitted is the uncommitted blocks on key's blob.
func (a azureUploads) uncommitted(t *testing.T, key string) []string {
	t.Helper()
	bl, err := a.cont.NewBlockBlobClient(a.pfx+key).GetBlockList(context.Background(), blockblob.BlockListTypeUncommitted, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range bl.UncommittedBlocks {
		if b.Name != nil {
			out = append(out, *b.Name)
		}
	}
	return out
}

// vmRSS is this process's resident set in bytes (Linux; 0 elsewhere): the
// Azure SDK's upload buffer is an anonymous mapping outside the Go heap.
func vmRSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}

// TestAzureUploadInParts: three parts (5 MiB, 5 MiB, a short last one)
// through StagePart (UploadStream with the checksum state's metadata) and
// CompleteUpload (StageBlockFromURL of each, then Put Block List): the
// file's bytes and checksums are its parts'; after DropUpload nothing is
// left under the upload prefix (every version erased) and the target blob
// holds no uncommitted block. The process's RSS is logged around each
// part (the SDK's 8 MiB upload buffer is outside the Go heap).
func TestAzureUploadInParts(t *testing.T) {
	az := azureForUploads(t)
	d := newDrive(t, az.store, icbfs.Settings{Name: "azparts", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	data := uploadData(2*partSize+4321, 7)
	const id = "upload-azure-00001"
	if err := d.StartUpload(ctx, id, []byte("r")); err != nil {
		t.Fatal(err)
	}
	for off, n := 0, 1; off < len(data); off, n = off+partSize, n+1 {
		p := data[off:min(off+partSize, len(data))]
		before := vmRSS()
		if err := d.StagePart(ctx, id, n, bytes.NewReader(p), int64(len(p))); err != nil {
			t.Fatalf("part %d: %v", n, err)
		}
		t.Logf("part %d (%d bytes): process RSS %d MiB before, %d MiB after", n, len(p), before>>20, vmRSS()>>20)
	}
	it, err := d.CompleteUpload(ctx, id, "/a/big.bin", 3, drive.Condition{}, drive.WriteOptions{Parents: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, ctx, d, "/a/big.bin"); !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, uploaded %d", len(got), len(data))
	}
	if it.Checksums == nil || *it.Checksums != sumsOf(data) {
		t.Fatalf("checksums %+v, want %+v", it.Checksums, sumsOf(data))
	}
	if blocks := az.uncommitted(t, it.ID); len(blocks) != 0 {
		t.Fatalf("the target holds %d uncommitted blocks after complete", len(blocks))
	}
	if err := d.DropUpload(ctx, id); err != nil {
		t.Fatal(err)
	}
	if vs, err := az.store.HistoryUnder(context.Background(), d.Filesystem().UploadPrefix()); err != nil || len(vs) != 0 {
		t.Fatalf("after the drop: %d versions under the upload prefix (%v)", len(vs), err)
	}
}

// TestAzureUploadCompleteTime times a complete of ICBFS_TEST_AZURE_UPLOAD_PARTS
// (default 200) parts: the copies, objstore.CopyConcurrency at once, and
// the commit. The figure, per part, is what bounds the part cap on Azure.
func TestAzureUploadCompleteTime(t *testing.T) {
	az := azureForUploads(t)
	parts := 200
	if v, err := strconv.Atoi(os.Getenv("ICBFS_TEST_AZURE_UPLOAD_PARTS")); err == nil && v > 0 {
		parts = v
	}
	d := newDrive(t, az.store, icbfs.Settings{Name: "aztime", Size: 1 << 40, RootMode: 0o755}, "/")
	ctx := system()
	const id = "upload-azure-time1"
	if err := d.StartUpload(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, partSize)
	t0 := time.Now()
	for n := 1; n <= parts; n++ {
		copy(buf, uploadData(64, byte(n)))
		if err := d.StagePart(ctx, id, n, bytes.NewReader(buf), int64(len(buf))); err != nil {
			t.Fatalf("part %d: %v", n, err)
		}
	}
	staged := time.Since(t0)
	t1 := time.Now()
	it, err := d.CompleteUpload(ctx, id, "/big.bin", parts, drive.Condition{}, drive.WriteOptions{})
	took := time.Since(t1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("T82-10 measured (Azure): %d parts staged in %.1f s; complete in %.1f s (%.0f ms a part); %d bytes",
		parts, staged.Seconds(), took.Seconds(), float64(took.Milliseconds())/float64(parts), it.Size)
	t2 := time.Now()
	if err := d.DropUpload(ctx, id); err != nil {
		t.Fatal(err)
	}
	t.Logf("T82-10 measured (Azure): the drop of %d parts in %.1f s", parts, time.Since(t2).Seconds())
}

// versions is how many versions key's blob has (deletions left out).
func (a azureUploads) versions(t *testing.T, key string) int {
	t.Helper()
	h, err := a.store.History(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range h {
		if !v.Deleted {
			n++
		}
	}
	return n
}

// stale stages an uncommitted block under raw id on key's blob: what a
// write cut half way leaves.
func (a azureUploads) stale(t *testing.T, key, raw string) {
	t.Helper()
	id := base64.StdEncoding.EncodeToString([]byte(raw))
	if _, err := a.cont.NewBlockBlobClient(a.pfx+key).StageBlock(context.Background(), id, nopCloser{bytes.NewReader([]byte("stale bytes"))}, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.uncommitted(t, key); len(got) == 0 {
		t.Fatal("the stale block was not staged")
	}
}

// TestAzureUploadStaleBlocks (T82-10 round 2): the target blob already
// holds uncommitted blocks, as a write cut half way leaves them for up to a
// week: with ids of another length (the SDK's stream upload: 48
// characters against icbfs's 24), or of icbfs's own (a cut PutParts). A
// complete over it succeeds, writes exactly one new version (the
// upload's), and leaves no uncommitted block.
func TestAzureUploadStaleBlocks(t *testing.T) {
	az := azureForUploads(t)
	d := newDrive(t, az.store, icbfs.Settings{Name: "azstale", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	for i, raw := range []string{
		"0123456789abcdef0123456789abcdef0123", // 36 bytes: another length
		"icbfs-part-000007",                    // icbfs's own length
	} {
		p := fmt.Sprintf("/f%d.bin", i)
		first := write(t, ctx, d, p, "the first version", drive.Condition{})
		az.stale(t, first.ID, raw)
		before := az.versions(t, first.ID)
		id := fmt.Sprintf("upload-azure-stale%d", i)
		if err := d.StartUpload(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		data := uploadData(1000, byte(3+i))
		if err := d.StagePart(ctx, id, 1, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
		if _, err := d.CompleteUpload(ctx, id, p, 1, drive.Condition{}, drive.WriteOptions{}); err != nil {
			t.Fatalf("stale %q: complete: %v", raw, err)
		}
		if got := readAll(t, ctx, d, p); !bytes.Equal(got, data) {
			t.Fatalf("stale %q: the file is not the upload", raw)
		}
		if after := az.versions(t, first.ID); after != before+1 {
			t.Fatalf("stale %q: %d versions before the complete, %d after: want exactly one new", raw, before, after)
		}
		if left := az.uncommitted(t, first.ID); len(left) != 0 {
			t.Fatalf("stale %q: %d uncommitted blocks left", raw, len(left))
		}
		_ = d.DropUpload(ctx, id)
	}
}

// TestAzureWriteStaleBlocks (T82-10 round 2): an ordinary write of a file
// in uploaded parts (a known size over the filesystem's part size: 8 MiB
// at an 8 GiB max file size, so PutParts with bodies) over a blob holding
// uncommitted blocks of another id length succeeds, with exactly one new
// version.
func TestAzureWriteStaleBlocks(t *testing.T) {
	az := azureForUploads(t)
	d := newDrive(t, az.store, icbfs.Settings{Name: "azwrite", Size: 1 << 34, RootMode: 0o755, MaxFileSize: 8 << 30}, "/")
	ctx := system()
	data := uploadData(17<<20+5, 4)
	first, err := d.Write(ctx, "/big.bin", bytes.NewReader(data), drive.Condition{}, drive.WriteOptions{Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	az.stale(t, first.ID, "0123456789abcdef0123456789abcdef0123")
	before := az.versions(t, first.ID)
	second := uploadData(17<<20+9, 5)
	if _, err := d.Write(ctx, "/big.bin", bytes.NewReader(second), drive.Condition{}, drive.WriteOptions{Size: int64(len(second))}); err != nil {
		t.Fatalf("a write in parts over stale blocks: %v", err)
	}
	if got := readAll(t, ctx, d, "/big.bin"); !bytes.Equal(got, second) {
		t.Fatal("the file is not the second write")
	}
	if after := az.versions(t, first.ID); after != before+1 {
		t.Fatalf("%d versions before the write, %d after: want exactly one new", before, after)
	}
	if left := az.uncommitted(t, first.ID); len(left) != 0 {
		t.Fatalf("%d uncommitted blocks left", len(left))
	}
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
