#!/bin/bash
# Start a throwaway MinIO by hand and print the variables a run needs.
# Prefer with-minio.sh, which also guarantees teardown; if you use this,
# remove it with down.sh when you're done:
#
#   eval "$(test/minio/up.sh)"
#   ...
#   test/minio/down.sh "$ICBFS_MINIO_CONTAINER"
set -u
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
minio_up || exit 1
for v in ICBFS_MINIO_CONTAINER ICBFS_MINIO_ENDPOINT ICBFS_MINIO_VM_ENDPOINT \
         ICBFS_MINIO_BUCKET ICBFS_MINIO_ACCESS_KEY ICBFS_MINIO_SECRET_KEY; do
  printf 'export %s=%q\n' "$v" "${!v}"
done
