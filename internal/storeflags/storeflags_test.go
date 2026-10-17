package storeflags

import (
	"errors"
	"flag"
	"testing"
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
