package azurestore_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/resurgentech/icbfs/internal/objstore"
	"github.com/resurgentech/icbfs/internal/objstore/azurestore"
	"github.com/resurgentech/icbfs/internal/objstore/storetest"
)

// TestConformance runs the store suite on a real Azure Blob Storage
// container, each test's keys under a prefix of its own:
// ICBFS_TEST_AZURE_ACCOUNT names the account (blob versioning on),
// ICBFS_TEST_AZURE_CONTAINER the container (default icbfs-test), and the
// store opens with ICBFS_TEST_AZURE_KEY, the account's key, or without
// one with ICBFS_TEST_AZURE_AUTH=login: the SDK's default credential
// chain (the az CLI's sign-in on a workstation, a service principal from
// the environment, a managed identity), whose identity needs Storage Blob
// Data Contributor on the container. Without an account it's skipped:
// Azurite, the emulator, has no blob versioning in a release yet.
// test/azure/run-conformance.sh sets them.
func TestConformance(t *testing.T) {
	account := os.Getenv("ICBFS_TEST_AZURE_ACCOUNT")
	if account == "" {
		t.Skip("no Azure account (ICBFS_TEST_AZURE_ACCOUNT); see test/azure/run-conformance.sh")
	}
	container := os.Getenv("ICBFS_TEST_AZURE_CONTAINER")
	if container == "" {
		container = "icbfs-test"
	}
	serviceURL := "https://" + account + ".blob.core.windows.net/"
	var base *azurestore.Store
	var err error
	switch auth, key := os.Getenv("ICBFS_TEST_AZURE_AUTH"), os.Getenv("ICBFS_TEST_AZURE_KEY"); {
	case auth == "login":
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			t.Fatal(err)
		}
		base, err = azurestore.NewWithCredential(serviceURL, cred, container)
	case auth == "" && key != "":
		base, err = azurestore.New(serviceURL, account, key, container)
	default:
		t.Fatalf("ICBFS_TEST_AZURE_AUTH=%q: want login, or ICBFS_TEST_AZURE_KEY set", auth)
	}
	if err != nil {
		t.Fatal(err)
	}
	run := time.Now().UTC().Format("20060102T150405.000")
	storetest.Run(t, func(t *testing.T) objstore.Store {
		return base.WithKeyPrefix(fmt.Sprintf("objstore-test/%s/%s/", run, t.Name()))
	})
}
