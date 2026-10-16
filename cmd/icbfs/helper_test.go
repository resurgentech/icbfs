package main

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestHelperFlags(t *testing.T) {
	name, dir, flags, err := helperFlags([]string{"docs", "/mnt/docs", "-o", "rw,ro,allow_other,noatime,cache_dir=/var/cache/icbfs"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mount", "-fs", "docs", "-read-only", "-allow-other", "-atime", "noatime", "-cache-dir", "/var/cache/icbfs", "/mnt/docs"}
	if name != "docs" || dir != "/mnt/docs" || !reflect.DeepEqual(flags, want) {
		t.Fatalf("got %q %q %q", name, dir, flags)
	}
	if _, _, _, err := helperFlags([]string{"docs", "/mnt", "-o", "frobnicate"}); err == nil {
		t.Fatal("an unknown option was accepted")
	}
	if _, _, _, err := helperFlags([]string{"docs"}); err == nil {
		t.Fatal("a missing mount point was accepted")
	}
	// -n and -v (mount(8)'s own) are ignored; -oOPTS works too.
	_, _, flags, err = helperFlags([]string{"-n", "docs", "-v", "/mnt", "-olocking"})
	if err != nil || !reflect.DeepEqual(flags, []string{"mount", "-fs", "docs", "-locking", "/mnt"}) {
		t.Fatalf("got %q, %v", flags, err)
	}
}

func TestLoadHelperConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "icbfs.env")
	os.WriteFile(path, []byte("# store\nexport ICBFS_ENDPOINT=http://minio:9000\nICBFS_ACCESS_KEY='from-file'\nOTHER=ignored\n"), 0o600)
	t.Setenv("ICBFS_ENDPOINT", "")
	os.Unsetenv("ICBFS_ENDPOINT")
	t.Setenv("ICBFS_ACCESS_KEY", "from-env")
	t.Setenv("OTHER", "")
	os.Unsetenv("OTHER")
	loadHelperConfig(path)
	if v := os.Getenv("ICBFS_ENDPOINT"); v != "http://minio:9000" {
		t.Fatalf("ICBFS_ENDPOINT = %q", v)
	}
	if v := os.Getenv("ICBFS_ACCESS_KEY"); v != "from-env" {
		t.Fatalf("a set variable was overridden: %q", v)
	}
	if _, set := os.LookupEnv("OTHER"); set {
		t.Fatal("a non-ICBFS variable was set")
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	if got := unescapeMountinfo(`/mnt/with\040space`); got != "/mnt/with space" {
		t.Fatalf("got %q", got)
	}
}

func TestRemountFlags(t *testing.T) {
	dir, flags, ok := remountFlags([]string{"docs", "/mnt/docs", "-o", "remount,ro,noexec"})
	if !ok || dir != "/mnt/docs" || flags != syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOEXEC {
		t.Fatalf("got %q %#x %v", dir, flags, ok)
	}
	if _, flags, ok := remountFlags([]string{"docs", "/mnt/docs", "-o", "remount,rw"}); !ok || flags != syscall.MS_REMOUNT {
		t.Fatalf("remount,rw: %#x %v", flags, ok)
	}
	if _, _, ok := remountFlags([]string{"docs", "/mnt/docs", "-o", "ro"}); ok {
		t.Fatal("a plain mount taken as a remount")
	}
}
