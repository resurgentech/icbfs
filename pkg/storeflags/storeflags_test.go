package storeflags

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	hist, err := h.Open(context.Background(), nil, Default())
	if err != nil {
		t.Fatalf("no fallback: %v", err)
	}
	if err := hist.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAzureTokenCommand(t *testing.T) {
	t.Setenv("ICBFS_STORE", "azure")
	t.Setenv("ICBFS_AZURE_ACCOUNT", "acct")
	t.Setenv("ICBFS_AZURE_KEY", "")
	t.Setenv("ICBFS_AZURE_TOKEN_COMMAND", "")
	parse := func(args ...string) *Flags {
		fset := flag.NewFlagSet("t", flag.ContinueOnError)
		f := Default().(*Flags)
		f.AddFlags(fset)
		if err := fset.Parse(args); err != nil {
			t.Fatal(err)
		}
		return f
	}
	f := parse()
	if err := f.Check(); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("Check with no key and no command = %v", err)
	}
	if f.Identity() != "acct" {
		t.Errorf("identity %q", f.Identity())
	}

	f = parse("-azure-token-command", "/opt/get-token")
	if err := f.Check(); err != nil {
		t.Fatalf("Check with a token command = %v: needs only an account and a container", err)
	}
	if f.Identity() != "acct by token command" {
		t.Errorf("identity %q: the command's cache isn't the key's", f.Identity())
	}

	f = parse("-azure-token-command", "/opt/get-token", "-azure-key", "k")
	if err := f.Check(); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("Check with a key and a token command = %v, want not both", err)
	}

	t.Setenv("ICBFS_AZURE_TOKEN_COMMAND", filepath.Join(t.TempDir(), "no-such-command"))
	f = parse()
	if err := f.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Open(context.Background()); err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("Open with a command that doesn't start = %v", err)
	}
}
