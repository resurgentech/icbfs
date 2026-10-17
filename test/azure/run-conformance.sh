#!/bin/bash
# The object store's conformance tests (internal/objstore) against a real
# Azure Blob Storage container, through AzureStore: the same tests MinIO
# and S3 pass. (Azurite, the emulator, has no blob versioning in a release
# yet, so these need the real thing.)
#
#   test/azure/run-conformance.sh [go test flags]
#
# .env.azure (gitignored, in the repository root) names the subscription,
# resource group, account (blob versioning on) and container. The account
# key comes from the az CLI, so nothing secret is written down. Each test
# writes under a prefix of its own; the account's lifecycle policy should
# expire what's left.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
set -a
# shellcheck disable=SC1091
source "$REPO/.env.azure"
set +a
ICBFS_TEST_AZURE_KEY=$(az storage account keys list --subscription "$ICBFS_TEST_AZURE_SUBSCRIPTION" \
  -g "$ICBFS_TEST_AZURE_RESOURCE_GROUP" -n "$ICBFS_TEST_AZURE_ACCOUNT" --query '[0].value' -o tsv)
export ICBFS_TEST_AZURE_KEY
cd "$REPO"
exec go test -count=1 "$@" ./internal/objstore
