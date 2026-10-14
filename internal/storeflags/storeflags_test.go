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
	if c.Endpoint != "http://env:9000" || c.Bucket != "icbfs" || c.Region != "us-east-1" {
		t.Fatalf("connection %+v", c)
	}
	if c.AccessKey != "env-access" || c.SecretKey != "flag-secret" {
		t.Fatalf("keys %q %q: a flag overrides the environment, the environment the default", c.AccessKey, c.SecretKey)
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
