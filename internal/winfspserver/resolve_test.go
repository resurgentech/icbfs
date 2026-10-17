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
	"github.com/resurgentech/icbfs/internal/objstore/s3store"
	"github.com/resurgentech/icbfs/internal/testutil"
	"github.com/resurgentech/icbfs/internal/testutil/minioimage"
)

// newTestFilesystem starts a MinIO container with a versioned bucket and
// returns a Filesystem on it. Requires Docker.
func newTestFilesystem(t *testing.T) *icbfs.Filesystem {
	t.Helper()
	ctx := context.Background()

	container, err := minio.Run(ctx, minioimage.Image())
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

	store := s3store.New(client, bucket)
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

// TestResolvePathNestedDirectories: a file several directories deep
// resolves to the right key, and so does each directory on the way.
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

// TestResolvePathHardlinkedNamesShareUUID: two paths linked to one file
// resolve to the same key.
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

// TestResolvePathCaseInsensitiveMatchesDifferentCase: with caseInsensitive,
// a path in different case resolves to the stored entry.
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
	// fail — case folding is opt-in.
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
		// The stored case, whatever case was asked for — what Open and
		// Create report to WinFsp as the normalized name.
		if got.Path != `\MixedCase\File.TXT` {
			t.Fatalf("case-insensitive resolvePath(%q).Path = %q, want stored case", path, got.Path)
		}
	}
}

// TestResolvePathNotFound: a missing path is icbfs.ErrNotFound, which
// callers map to the right NTSTATUS.
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

// TestResolvePathThroughNonDirectoryFails: a file used as a directory
// (`\file.txt\sub`) is icbfs.ErrNotDir.
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

// TestHasIntermediateSymlink covers the check GetSecurityByName uses to
// hand a path with a symlink mid-way through it to WinFsp's own reparse
// resolver: true only when a symlink sits before the last component.
func TestHasIntermediateSymlink(t *testing.T) {
	fsys := newTestFilesystem(t)
	ctx := context.Background()
	root := fsys.RootKey()

	dir1, _, err := fsys.Mkdir(ctx, root, "1", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir 1: %v", err)
	}
	if _, _, err := fsys.Mkdir(ctx, dir1, "1.1", 0755, 0, 0); err != nil {
		t.Fatalf("mkdir 1.1: %v", err)
	}
	dir2, _, err := fsys.Mkdir(ctx, root, "2", 0755, 0, 0)
	if err != nil {
		t.Fatalf("mkdir 2: %v", err)
	}
	if _, _, err := fsys.Symlink(ctx, dir2, "l1", `..\1\.`, 0, 0); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, _, _, err := fsys.Create(ctx, root, "file.txt", 0644, 0, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{`\2\l1\1.1\x`, true},
		{`\2\l1\1.1`, true},
		{`\2\l1`, false},         // last component: not traversed through
		{`\1\1.1\x`, false},      // plain directories only
		{`\file.txt\sub`, false}, // a file mid-path is ErrNotDir, not a reparse
		{`\missing\x`, false},
		{`\`, false},
	}
	for _, c := range cases {
		if got := hasIntermediateSymlink(ctx, fsys, c.path, false); got != c.want {
			t.Errorf("hasIntermediateSymlink(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestSplitStream(t *testing.T) {
	cases := []struct {
		in, path, stream string
		ok               bool
	}{
		{`\dir\file`, `\dir\file`, ``, true},
		{`\dir\file:foo`, `\dir\file`, `foo`, true},
		{`\dir\file:foo:$DATA`, `\dir\file`, `foo`, true},
		{`\dir\file:foo:$data`, `\dir\file`, `foo`, true},
		{`\dir\file::$DATA`, `\dir\file`, ``, true},
		{`\dir1:bar`, `\dir1`, `bar`, true},
		{`\file:foo:$INDEX_ALLOCATION`, ``, ``, false},
	}
	for _, c := range cases {
		path, stream, ok := splitStream(c.in)
		if path != c.path || stream != c.stream || ok != c.ok {
			t.Errorf("splitStream(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, path, stream, ok, c.path, c.stream, c.ok)
		}
	}
}
