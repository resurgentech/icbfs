#!/bin/bash
# T82-10 review R6: uploads in parts (pkg/drive/icbfsdrive's TestAzureUpload*)
# against a real Azure Blob Storage container: the copy path MinIO cannot
# run. The same account and variables as run-conformance.sh.
#
#   test/azure/run-uploads.sh [go test flags]
#   ICBFS_TEST_AZURE_AUTH=login test/azure/run-uploads.sh [go test flags]
#   ICBFS_TEST_AZURE_UPLOAD_PARTS=200 (the timed complete's parts, 5 MiB each)
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
exec go test -count=1 -run 'TestAzure(Upload|Write)' -timeout 30m "$@" ./pkg/drive/icbfsdrive
