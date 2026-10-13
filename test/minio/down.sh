#!/bin/bash
# Remove a throwaway MinIO started by up.sh, with its data.
#
#   test/minio/down.sh <container name>     (default: $ICBFS_MINIO_CONTAINER)
#   test/minio/down.sh --all                (every icbfs-test-minio-* left over)
set -u
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
if [ "${1:-}" = "--all" ]; then
  for name in $(docker ps --format '{{.Names}}' | grep '^icbfs-test-minio-'); do
    minio_down "$name"
  done
  exit 0
fi
minio_down "${1:-}"
