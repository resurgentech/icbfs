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

// TestAzureUploadStaleBlocks: the target blob already holds uncommitted
// blocks with ids of another length (a cut UploadStream leaves UUID-based
// ids; Azure requires one id length a blob): what CompleteUpload does then.
// Whatever it answers, the file is not changed by a failure, and the
// answer is logged for the report.
func TestAzureUploadStaleBlocks(t *testing.T) {
	az := azureForUploads(t)
	d := newDrive(t, az.store, icbfs.Settings{Name: "azstale", Size: 1 << 30, RootMode: 0o755}, "/")
	ctx := system()
	first := write(t, ctx, d, "/f.bin", "the first version", drive.Condition{})
	stale := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef0123")) // 36 bytes, not icbfs's 17
	if _, err := az.cont.NewBlockBlobClient(az.pfx+first.ID).StageBlock(context.Background(), stale, nopCloser{bytes.NewReader([]byte("stale"))}, nil); err != nil {
		t.Fatal(err)
	}
	const id = "upload-azure-stale1"
	if err := d.StartUpload(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	data := uploadData(1000, 3)
	if err := d.StagePart(ctx, id, 1, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	_, err := d.CompleteUpload(ctx, id, "/f.bin", 1, drive.Condition{}, drive.WriteOptions{})
	got := readAll(t, ctx, d, "/f.bin")
	t.Logf("T82-10 measured (Azure): complete over a target with stale blocks of another id length: err %v; the file is %q", err, got[:min(len(got), 20)])
	if err != nil && string(got) != "the first version" {
		t.Fatalf("a failed complete changed the file: %q", got)
	}
	if err == nil && !bytes.Equal(got, data) {
		t.Fatal("a complete that succeeded did not write its parts")
	}
	if err != nil {
		t.Logf("stale blocks left: %d", len(az.uncommitted(t, first.ID)))
	}
	_ = d.DropUpload(ctx, id)
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
