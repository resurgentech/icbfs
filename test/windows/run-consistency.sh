#!/bin/bash
# Two mounts of one filesystem on the Windows VM, as two machines: what one
# changes, an open on the other sees at once, and the other's listings and
# cached names within -meta-cache (1s). Uses the build run-winfsp-tests.sh
# or run-bench.sh deployed (C:\icbfs-test). Storage is a throwaway MinIO.
#
#   run-consistency.sh
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-consistency.sh" "$@"
fi
source "$HERE/lib.sh"
FILTER='warning\|post-quantum\|store now\|upgraded'
IP=$(vm_ip)
[ -n "$IP" ] || { echo "no IP for $VM_NAME — is it running?" >&2; exit 1; }
sync_vm_clock "$IP"
scp_vm "$HERE/run-consistency.ps1" "Administrator@${IP}:C:/Windows/Temp/run-consistency.ps1" || exit 1
out=$(ssh_vm "$IP" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\run-consistency.ps1 -Endpoint $ICBFS_MINIO_VM_ENDPOINT -Bucket $ICBFS_MINIO_BUCKET -AccessKey $ICBFS_MINIO_ACCESS_KEY -SecretKey $ICBFS_MINIO_SECRET_KEY" 2>&1 | grep -v -i "$FILTER")
echo "$out"
echo "$out" | grep -q "RESULT fail=0"
