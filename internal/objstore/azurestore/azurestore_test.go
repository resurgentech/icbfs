package azurestore_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/azurestore"
	"github.com/resurgentech/icbfs/internal/objstore/storetest"
)

// TestConformance runs the store suite on a real Azure Blob Storage
// container, each test's keys under a prefix of its own:
// ICBFS_TEST_AZURE_ACCOUNT names the account (blob versioning on),
// ICBFS_TEST_AZURE_KEY its key, ICBFS_TEST_AZURE_CONTAINER the container
// (default icbfs-test). Without them it's skipped: Azurite, the emulator,
// has no blob versioning in a release yet. test/azure/run-conformance.sh
// sets them.
func TestConformance(t *testing.T) {
	account := os.Getenv("ICBFS_TEST_AZURE_ACCOUNT")
	if account == "" {
		t.Skip("no Azure account (ICBFS_TEST_AZURE_ACCOUNT); see test/azure/run-conformance.sh")
	}
	container := os.Getenv("ICBFS_TEST_AZURE_CONTAINER")
	if container == "" {
		container = "icbfs-test"
	}
	base, err := azurestore.New("https://"+account+".blob.core.windows.net/", account, os.Getenv("ICBFS_TEST_AZURE_KEY"), container)
	if err != nil {
		t.Fatal(err)
	}
	run := time.Now().UTC().Format("20060102T150405.000")
	storetest.Run(t, func(t *testing.T) objstore.Store {
		return base.WithKeyPrefix(fmt.Sprintf("objstore-test/%s/%s/", run, t.Name()))
	})
}
