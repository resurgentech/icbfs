package history

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// A token command takes the key's place, so the store is never opened
// with the key: the rules are storeflags', and a command that doesn't
// start fails the open.
func TestTokenCommandConfig(t *testing.T) {
	azure := func(key string) pkgicbfs.StoreConfig {
		return pkgicbfs.StoreConfig{Kind: pkgicbfs.KindAzure, Azure: pkgicbfs.Azure{Account: "acct", Container: "c", Key: key}}
	}
	missing := filepath.Join(t.TempDir(), "no-such-command")
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"key and command", Config{Store: azure("key"), Filesystem: "fs", TokenCommand: missing}, "not both"},
		{"no account", Config{Store: pkgicbfs.StoreConfig{Kind: pkgicbfs.KindAzure, Azure: pkgicbfs.Azure{Container: "c"}}, Filesystem: "fs", TokenCommand: missing}, "needs an account and a container"},
		{"command missing", Config{Store: azure(""), Filesystem: "fs", TokenCommand: missing}, "no-such-command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.CacheDir = t.TempDir()
			_, err := Open(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open: %v, want an error with %q", err, tc.want)
			}
		})
	}
}
