#!/bin/bash
# Run a command against a fresh, throwaway MinIO, removed afterwards no
# matter how the command ends (success, failure, Ctrl-C).
#
#   test/minio/with-minio.sh <command> [args...]
#
# The command sees ICBFS_MINIO_ENDPOINT (from this host),
# ICBFS_MINIO_VM_ENDPOINT (from the test VMs), ICBFS_MINIO_BUCKET (empty,
# versioned), ICBFS_MINIO_ACCESS_KEY and ICBFS_MINIO_SECRET_KEY. Exits
# with the command's own status.
set -u
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

if [ $# -eq 0 ]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

trap 'minio_down' EXIT
trap 'exit 130' INT TERM
minio_up || exit 1
"$@"
