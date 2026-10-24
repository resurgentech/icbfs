#!/bin/bash
# Which store a test runs on, for the test scripts to source. Not run
# directly. ICBFS_TEST_STORE chooses:
#
#   minio  (the default) the throwaway MinIO test/minio/with-minio.sh
#          starts; the script must already be running under it
#   s3     the real bucket .env.aws names (test/aws), with the AWS CLI's
#          credentials
#   azure  the real container .env.azure names (test/azure), with the
#          account key from the az CLI
#
# It sets STORE_ARGS, the store's flags as this host reaches it, and
# STORE_VM_ARGS as the VMs do; exports the store's secrets as the ICBFS_*
# variables icbfs-cli reads; and sets STORE_ENV to the same lines, for a
# VM's environment file. Secrets never go on a command line (ps would
# show them).

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/home.sh"

store_setup() {
  local repo="$ICBFS_TEST_HOME" region
  case "${ICBFS_TEST_STORE:-minio}" in
  azure)
    set -a
    # shellcheck disable=SC1091
    source "$repo/.env.azure"
    set +a
    STORE_ARGS="-store azure -azure-account $ICBFS_TEST_AZURE_ACCOUNT -azure-container $ICBFS_TEST_AZURE_CONTAINER"
    STORE_VM_ARGS="$STORE_ARGS"
    ICBFS_AZURE_KEY=$(az storage account keys list --subscription "$ICBFS_TEST_AZURE_SUBSCRIPTION" \
      -g "$ICBFS_TEST_AZURE_RESOURCE_GROUP" -n "$ICBFS_TEST_AZURE_ACCOUNT" --query '[0].value' -o tsv) || return 1
    export ICBFS_AZURE_KEY
    STORE_ENV="ICBFS_AZURE_KEY=$ICBFS_AZURE_KEY"
    ;;
  s3)
    set -a
    # shellcheck disable=SC1091
    source "$repo/.env.aws"
    set +a
    eval "$(aws configure export-credentials --format env)" || return 1
    region="${ICBFS_TEST_S3_REGION:-us-east-1}"
    STORE_ARGS="-store s3 -endpoint https://s3.$region.amazonaws.com -bucket $ICBFS_TEST_S3_BUCKET -region $region"
    STORE_VM_ARGS="$STORE_ARGS"
    export ICBFS_ACCESS_KEY="$AWS_ACCESS_KEY_ID" ICBFS_SECRET_KEY="$AWS_SECRET_ACCESS_KEY"
    STORE_ENV=$(printf 'ICBFS_ACCESS_KEY=%s\nICBFS_SECRET_KEY=%s' "$ICBFS_ACCESS_KEY" "$ICBFS_SECRET_KEY")
    ;;
  minio)
    : "${ICBFS_MINIO_VM_ENDPOINT:?run under test/minio/with-minio.sh}"
    STORE_ARGS="-store s3 -endpoint $ICBFS_MINIO_ENDPOINT -bucket $ICBFS_MINIO_BUCKET"
    STORE_VM_ARGS="-store s3 -endpoint $ICBFS_MINIO_VM_ENDPOINT -bucket $ICBFS_MINIO_BUCKET"
    export ICBFS_ACCESS_KEY="$ICBFS_MINIO_ACCESS_KEY" ICBFS_SECRET_KEY="$ICBFS_MINIO_SECRET_KEY"
    STORE_ENV=$(printf 'ICBFS_ACCESS_KEY=%s\nICBFS_SECRET_KEY=%s' "$ICBFS_ACCESS_KEY" "$ICBFS_SECRET_KEY")
    ;;
  *)
    echo "ICBFS_TEST_STORE=$ICBFS_TEST_STORE: minio, s3 or azure" >&2
    return 1
    ;;
  esac
}

# store_needs_minio is true when the store is the throwaway MinIO and it
# isn't running yet: the script should exec itself under with-minio.sh.
store_needs_minio() {
  [ "${ICBFS_TEST_STORE:-minio}" = minio ] && [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]
}
