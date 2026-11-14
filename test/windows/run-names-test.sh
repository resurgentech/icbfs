#!/bin/bash
# What a WinFsp mount does today with names Windows can't hold, on the
# Windows VM: a primary-POSIX filesystem holding them (made through the
# core by namesfixture), listed, opened, renamed and deleted through a
# mount, and a primary-Windows one as the control. It asserts nothing about
# the outcomes; it passes when the script runs to the end and prints its
# table (RESULT done). Best on the Windows 11 VM (ICBFS_WIN_VM=icbfs-win11-test).
#
#   run-names-test.sh [--no-deploy]
#
# Deploys and builds this working tree as run-bench.sh does. Storage is a
# throwaway MinIO; the output lands in $WORK_DIR/names-<time>.txt.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-names-test.sh" "$@"
fi
source "$HERE/lib.sh"
REPO="$(cd "$HERE/../.." && pwd)"
FILTER='warning\|post-quantum\|store now\|upgraded'

deploy=1
if [ "${1:-}" = "--no-deploy" ]; then deploy=0; shift; fi

IP=$(vm_ip)
[ -n "$IP" ] || { echo "no IP for $VM_NAME — is it running?" >&2; exit 1; }
sync_vm_clock "$IP"
mkdir -p "$WORK_DIR"

if [ "$deploy" = 1 ]; then
  echo "Deploying working tree to C:\\icbfs-test..." >&2
  tarball="$WORK_DIR/icbfs-src.tar"
  (cd "$REPO" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball") || exit 1
  scp_vm "$tarball" "Administrator@${IP}:C:/Windows/Temp/icbfs-src.tar" || { echo "deploy failed" >&2; exit 1; }
  out=$(ssh_vm "$IP" "if (Test-Path C:\\icbfs-test) { Remove-Item C:\\icbfs-test -Recurse -Force }; New-Item -ItemType Directory C:\\icbfs-test | Out-Null; tar -xf C:\\Windows\\Temp\\icbfs-src.tar -C C:\\icbfs-test; cd C:\\icbfs-test; go build -o icbfs-winfsp.exe ./cmd/icbfs-winfsp; if (\$LASTEXITCODE -eq 0) { go build -o icbfs-cli.exe ./cmd/icbfs-cli }; if (\$LASTEXITCODE -eq 0) { go build -o namesfixture.exe ./test/windows/namesfixture }; echo BUILD_EXIT=\$LASTEXITCODE" 2>&1 | grep -v -i "$FILTER")
  echo "$out" >&2
  echo "$out" | grep -q "BUILD_EXIT=0" || { echo "VM build failed" >&2; exit 1; }
fi

scp_vm "$HERE/run-names-test.ps1" "Administrator@${IP}:C:/Windows/Temp/run-names-test.ps1" || exit 1
result="$WORK_DIR/names-$(date +%Y%m%d%H%M%S).txt"
echo "Running the names test..." >&2
ssh_vm "$IP" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\run-names-test.ps1 -Endpoint $ICBFS_MINIO_VM_ENDPOINT -Bucket $ICBFS_MINIO_BUCKET -AccessKey $ICBFS_MINIO_ACCESS_KEY -SecretKey $ICBFS_MINIO_SECRET_KEY" 2>&1 \
  | grep -v -i "$FILTER" | tee "$result"
echo "(result: $result)" >&2
grep -q "^RESULT done" "$result"
