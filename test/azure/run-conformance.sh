#!/bin/bash
# The object store's conformance tests (internal/objstore) against a real
# Azure Blob Storage container, through AzureStore: the same tests MinIO
# and S3 pass. (Azurite, the emulator, has no blob versioning in a release
# yet, so these need the real thing.)
#
#   test/azure/run-conformance.sh [go test flags]
#   ICBFS_TEST_AZURE_AUTH=login test/azure/run-conformance.sh [go test flags]
#
# .env.azure (gitignored, in the repository root) names the subscription,
# resource group, account (blob versioning on) and container. By default
# the store opens with the account key, fetched from the az CLI so nothing
# secret is written down. With ICBFS_TEST_AZURE_AUTH=login it opens with
# the az CLI's sign-in instead (the SDK's default credential chain), which
# needs Storage Blob Data Contributor on the container and no key at all:
# the run that proves the account's shared keys can be off. Each test
# writes under a prefix of its own; the account's lifecycle policy should
# expire what's left.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck disable=SC1091
source "$REPO/test/home.sh"
set -a
# shellcheck disable=SC1091
source "$ICBFS_TEST_HOME/.env.azure"
set +a
if [ "${ICBFS_TEST_AZURE_AUTH:-}" != "login" ]; then
  ICBFS_TEST_AZURE_KEY=$(az storage account keys list --subscription "$ICBFS_TEST_AZURE_SUBSCRIPTION" \
    -g "$ICBFS_TEST_AZURE_RESOURCE_GROUP" -n "$ICBFS_TEST_AZURE_ACCOUNT" --query '[0].value' -o tsv)
  export ICBFS_TEST_AZURE_KEY
fi
cd "$REPO"
exec go test -count=1 "$@" ./internal/objstore/azurestore
