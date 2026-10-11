package winfspserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/resurgentech/icbfs/internal/icbfs"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/testutil"
)

// newTestFilesystem spins up a real MinIO container and a versioned
// bucket, and returns an already-bootstrapped Filesystem backed by it
// — same pattern as every other package's own copy of this helper
// (internal/icbfs, internal/fuseserver, ...). Requires Docker.
func newTestFilesystem(t *testing.T) *icbfs.Filesystem {
	t.Helper()
	ctx := context.Background()

	container, err := minio.Run(ctx, "minio/minio:latest")
	if err != nil {
		t.Fatalf("start minio container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate minio container: %v", err)
		}
	})

	endpoint, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("get minio connection string: %v", err)
	}

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("http://" + endpoint),
		UsePathStyle: true,
		Credentials: awscreds.NewStaticCredentialsProvider(
			container.Username, container.Password, "",
		),
	})

	const bucket = "icbfs-winfspserver-test"
	testutil.RetryUntilReady(t, 10*time.Second, func() error {
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		return err
	})
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		t.Fatalf("enable bucket versioning: %v", err)
	}

	store := objstore.NewS3Store(client, bucket)
	fsys := icbfs.New(store, "test")
	if err := fsys.Bootstrap(ctx, 1<<30, 0755, 0, 0, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return fsys
}

// TestResolvePathRoot covers both spellings of the root WinFsp path
// (`\` and the empty string) resolving to the filesystem's own root
// key, not treated as a missing/invalid segment.
func TestResolvePathRoot(t *testing.T) {
	fsys := newTestFilesystem(t)
	ctx := context.Background()

	for _, path := range []string{`\`, ``} {
		got, err := resolvePath(ctx, fsys, path, false)
		if err != nil {
			t.Fatalf("resolvePath(%q): %v", path, err)
		}
		if got.Key != fsys.RootKey() {
			t.Fatalf("resolvePath(%q).Key = %q, want root key %q", path, got.Key, fsys.RootKey())
		}
		if got.Type != icbfs.TypeDir {
			t.Fatalf("resolvePath(%q).Type = %v, want TypeDir", path, got.Type)
		}
	}
}

// TestResolvePathNestedDirectories covers F2's "Done when" nested-
// directory case: a file several directories deep resolves to the
// right key, and every intermediate directory along the way also
// resolves correctly on its own.
func TestResolvePathNestedDirectories(t *testing.T) {
	fsys := newTestFilesystem(t)
	ctx := context.Background()

	dir1UUID, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "dir1", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir dir1: %v", err)
	}
	dir2UUID, _, err := fsys.Mkdir(ctx, dir1UUID, "dir2", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir dir1/dir2: %v", err)
	}
	fileUUID, _, _, err := fsys.Create(ctx, dir2UUID, "file.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create dir1/dir2/file.txt: %v", err)
	}

	cases := []struct {
		path    string
		wantKey string
		wantTyp icbfs.EntryType
	}{
		{`\dir1`, dir1UUID, icbfs.TypeDir},
		{`\dir1\dir2`, dir2UUID, icbfs.TypeDir},
		{`\dir1\dir2\file.txt`, fileUUID, icbfs.TypeFile},
	}
	for _, c := range cases {
		got, err := resolvePath(ctx, fsys, c.path, false)
		if err != nil {
			t.Fatalf("resolvePath(%q): %v", c.path, err)
		}
		if got.Key != c.wantKey {
			t.Errorf("resolvePath(%q).Key = %q, want %q", c.path, got.Key, c.wantKey)
		}
		if got.Type != c.wantTyp {
			t.Errorf("resolvePath(%q).Type = %v, want %v", c.path, got.Type, c.wantTyp)
		}
	}
}

// TestResolvePathHardlinkedNamesShareUUID covers F2's other explicit
// "Done when" case: two different paths linked to the same file (via
// icbfs.Filesystem.Link, the real hardlink primitive the FUSE driver
// already uses) resolve to the identical UUID — proving this layer
// doesn't accidentally treat a hardlink's second name as a distinct
// entity.
func TestResolvePathHardlinkedNamesShareUUID(t *testing.T) {
	fsys := newTestFilesystem(t)
	ctx := context.Background()

	dirUUID, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "dir", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir dir: %v", err)
	}
	fileUUID, _, _, err := fsys.Create(ctx, fsys.RootKey(), "original.txt", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create original.txt: %v", err)
	}
	if _, err := fsys.Link(ctx, dirUUID, "linked.txt", fileUUID, icbfs.TypeFile); err != nil {
		t.Fatalf("link dir/linked.txt -> original.txt: %v", err)
	}

	original, err := resolvePath(ctx, fsys, `\original.txt`, false)
	if err != nil {
		t.Fatalf("resolvePath(original.txt): %v", err)
	}
	linked, err := resolvePath(ctx, fsys, `\dir\linked.txt`, false)
	if err != nil {
		t.Fatalf("resolvePath(dir/linked.txt): %v", err)
	}
	if original.Key != fileUUID || linked.Key != fileUUID {
		t.Fatalf("original.Key = %q, linked.Key = %q, want both = %q", original.Key, linked.Key, fileUUID)
	}
	if original.Key != linked.Key {
		t.Fatalf("hardlinked paths resolved to different keys: %q vs %q", original.Key, linked.Key)
	}
}

// TestResolvePathCaseInsensitiveMatchesDifferentCase covers task F4:
// with caseInsensitive=true, a path using different case than what
// was actually stored still resolves to the same entry — the
// WinFsp-mount-flag-driven behavior ARCHITECTURE.md's Windows
// compatibility section calls for (storage itself stays case-
// sensitive/preserving; only lookup folds case, and only when this
// flag is set).
func TestResolvePathCaseInsensitiveMatchesDifferentCase(t *testing.T) {
	fsys := newTestFilesystem(t)
	ctx := context.Background()

	dirUUID, _, err := fsys.Mkdir(ctx, fsys.RootKey(), "MixedCase", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir MixedCase: %v", err)
	}
	fileUUID, _, _, err := fsys.Create(ctx, dirUUID, "File.TXT", 0644, 0, 0)
	if err != nil {
		t.Fatalf("create File.TXT: %v", err)
	}

	// caseInsensitive=false: a differently-cased request must still
	// fail, exactly as it did before this task existed — case folding
	// is opt-in, never the default.
	if _, err := resolvePath(ctx, fsys, `\mixedcase\file.txt`, false); !errors.Is(err, icbfs.ErrNotFound) {
		t.Fatalf("case-sensitive resolvePath(wrong case) = %v, want ErrNotFound", err)
	}

	for _, path := range []string{`\MixedCase\File.TXT`, `\mixedcase\file.txt`, `\MIXEDCASE\FILE.TXT`} {
		got, err := resolvePath(ctx, fsys, path, true)
		if err != nil {
			t.Fatalf("case-insensitive resolvePath(%q): %v", path, err)
		}
		if got.Key != fileUUID {
			t.Fatalf("case-insensitive resolvePath(%q).Key = %q, want %q", path, got.Key, fileUUID)
		}
	}
}

// TestResolvePathNotFound confirms a genuinely missing path surfaces
// icbfs.ErrNotFound, not some other generic error — callers (F3's real
// WinFsp callback wiring) need to distinguish this to report the right
// NTSTATUS back to the kernel.
func TestResolvePathNotFound(t *testing.T) {
	fsys := newTestFilesystem(t)
	ctx := context.Background()

	if _, err := resolvePath(ctx, fsys, `\does-not-exist.txt`, false); !errors.Is(err, icbfs.ErrNotFound) {
		t.Fatalf("resolvePath(missing) = %v, want ErrNotFound", err)
	}
	if _, err := resolvePath(ctx, fsys, `\does\not\exist\either`, false); !errors.Is(err, icbfs.ErrNotFound) {
		t.Fatalf("resolvePath(missing, nested) = %v, want ErrNotFound", err)
	}
}

// TestResolvePathThroughNonDirectoryFails confirms treating a plain
// file as an intermediate path component (e.g. `\file.txt\sub`, what
// a real client sends if it mistakenly assumes a file is a directory)
// fails with icbfs.ErrNotDir, not a confusing not-found or a silent
// wrong answer.
func TestResolvePathThroughNonDirectoryFails(t *testing.T) {
	fsys := newTestFilesystem(t)
	ctx := context.Background()

	if _, _, _, err := fsys.Create(ctx, fsys.RootKey(), "file.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create file.txt: %v", err)
	}

	if _, err := resolvePath(ctx, fsys, `\file.txt\sub`, false); !errors.Is(err, icbfs.ErrNotDir) {
		t.Fatalf("resolvePath(through a file) = %v, want ErrNotDir", err)
	}
}
