#!/bin/bash
# Times what Explorer and Office do to an icbfs mount on the Windows VM:
# listing a folder of many files and every file's details, opening and
# reading small files, a deep path, probing names that don't exist, an
# Office-style save, copying a folder in; and the mount's peak memory.
# Once per -meta-cache value, each on a fresh filesystem.
#
#   run-bench.sh [--no-deploy] [-- extra args for run-bench.ps1]
#   e.g. run-bench.sh -- -MetaCaches 0,1s -Files 500
#
# Deploys and builds this working tree as run-winfsp-tests.sh does.
# Storage is a throwaway MinIO; results land in
# $WORK_DIR/bench-<time>.txt.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-bench.sh" "$@"
fi
source "$HERE/lib.sh"
REPO="$(cd "$HERE/../.." && pwd)"
FILTER='warning\|post-quantum\|store now\|upgraded'

deploy=1
if [ "${1:-}" = "--no-deploy" ]; then deploy=0; shift; fi
[ "${1:-}" = "--" ] && shift

IP=$(vm_ip)
[ -n "$IP" ] || { echo "no IP for $VM_NAME — is it running?" >&2; exit 1; }
sync_vm_clock "$IP"
mkdir -p "$WORK_DIR"

if [ "$deploy" = 1 ]; then
  echo "Deploying working tree to C:\\icbfs-test..." >&2
  tarball="$WORK_DIR/icbfs-src.tar"
  (cd "$REPO" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball") || exit 1
  scp_vm "$tarball" "Administrator@${IP}:C:/Windows/Temp/icbfs-src.tar" || { echo "deploy failed" >&2; exit 1; }
  out=$(ssh_vm "$IP" "if (Test-Path C:\\icbfs-test) { Remove-Item C:\\icbfs-test -Recurse -Force }; New-Item -ItemType Directory C:\\icbfs-test | Out-Null; tar -xf C:\\Windows\\Temp\\icbfs-src.tar -C C:\\icbfs-test; cd C:\\icbfs-test; go build -o icbfs-winfsp.exe ./cmd/icbfs-winfsp; if (\$LASTEXITCODE -eq 0) { go build -o icbfs-cli.exe ./cmd/icbfs-cli }; echo BUILD_EXIT=\$LASTEXITCODE" 2>&1 | grep -v -i "$FILTER")
  echo "$out" >&2
  echo "$out" | grep -q "BUILD_EXIT=0" || { echo "VM build failed" >&2; exit 1; }
fi

scp_vm "$HERE/run-bench.ps1" "Administrator@${IP}:C:/Windows/Temp/run-bench.ps1" || exit 1
result="$WORK_DIR/bench-$(date +%Y%m%d%H%M%S).txt"
echo "Running the benchmark..." >&2
ssh_vm "$IP" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\run-bench.ps1 -Endpoint $ICBFS_MINIO_VM_ENDPOINT -Bucket $ICBFS_MINIO_BUCKET -AccessKey $ICBFS_MINIO_ACCESS_KEY -SecretKey $ICBFS_MINIO_SECRET_KEY $*" 2>&1 \
  | grep -v -i "$FILTER" | tee "$result"
echo "(result: $result)" >&2
