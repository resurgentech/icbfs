package storeflags

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

func TestEnvironmentThenFlags(t *testing.T) {
	t.Setenv("ICBFS_ENDPOINT", "http://env:9000")
	t.Setenv("ICBFS_ACCESS_KEY", "env-access")
	t.Setenv("ICBFS_SECRET_KEY", "env-secret")
	fset := flag.NewFlagSet("t", flag.ContinueOnError)
	c := Add(fset)
	if err := fset.Parse([]string{"-secret-key", "flag-secret"}); err != nil {
		t.Fatal(err)
	}
	if c.Kind != "s3" || c.S3.Endpoint != "http://env:9000" || c.S3.Bucket != "icbfs" || c.S3.Region != "us-east-1" {
		t.Fatalf("connection %+v", c)
	}
	if c.S3.AccessKey != "env-access" || c.S3.SecretKey != "flag-secret" {
		t.Fatalf("keys %q %q: a flag overrides the environment, the environment the default", c.S3.AccessKey, c.S3.SecretKey)
	}
	if err := Check(c); err != nil {
		t.Fatal(err)
	}
}

func TestNoKeysIsAnError(t *testing.T) {
	t.Setenv("ICBFS_ACCESS_KEY", "")
	t.Setenv("ICBFS_SECRET_KEY", "")
	fset := flag.NewFlagSet("t", flag.ContinueOnError)
	c := Add(fset)
	if err := fset.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := Check(c); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("Check with no keys = %v", err)
	}
}

func TestAzure(t *testing.T) {
	t.Setenv("ICBFS_STORE", "azure")
	t.Setenv("ICBFS_AZURE_ACCOUNT", "acct")
	t.Setenv("ICBFS_AZURE_KEY", "")
	fset := flag.NewFlagSet("t", flag.ContinueOnError)
	c := Add(fset)
	if err := fset.Parse([]string{"-azure-container", "docs"}); err != nil {
		t.Fatal(err)
	}
	if c.Kind != "azure" || c.Azure.Account != "acct" || c.Azure.Container != "docs" || c.Azure.ServiceURL() != "https://acct.blob.core.windows.net/" {
		t.Fatalf("connection %+v", c)
	}
	if err := Check(c); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("Check with no key = %v", err)
	}
	c.Azure.Key = "k"
	if err := Check(c); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryFallsBackToATemporaryCache(t *testing.T) {
	// A file where the cache directory should be: the shared cache can't
	// be made there.
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &History{Dir: blocked, TTL: time.Hour}
	hist, err := h.Open(context.Background(), nil, &pkgicbfs.StoreConfig{})
	if err != nil {
		t.Fatalf("no fallback: %v", err)
	}
	if err := hist.Close(); err != nil {
		t.Fatal(err)
	}
}
