#!/bin/bash
# icbfs-sync on the Windows VM: its Go tests (the engine, the folder side
# on NTFS), then run-sync-test.ps1 end to end against a throwaway MinIO.
#
#   run-sync-test.sh [--no-deploy]
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-sync-test.sh" "$@"
fi
source "$HERE/lib.sh"
REPO="$(cd "$HERE/../.." && pwd)"
FILTER='warning\|post-quantum\|store now\|upgraded'

IP=$(vm_ip)
[ -n "$IP" ] || { echo "no IP for $VM_NAME — is it running?" >&2; exit 1; }
scp_vm() {
  sshpass -p "$VM_PASSWORD" scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$@"
}
mkdir -p "$WORK_DIR"

if [ "${1:-}" != "--no-deploy" ]; then
  echo "Deploying working tree to C:\\icbfs-test..." >&2
  tarball="$WORK_DIR/icbfs-src.tar"
  (cd "$REPO" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball") || exit 1
  scp_vm "$tarball" "Administrator@${IP}:C:/Windows/Temp/icbfs-src.tar" || exit 1
  out=$(ssh_vm "$IP" "Get-Process icbfs-winfsp,icbfs-sync -ErrorAction SilentlyContinue | Stop-Process -Force; if (Test-Path C:\\icbfs-test) { Remove-Item C:\\icbfs-test -Recurse -Force }; New-Item -ItemType Directory C:\\icbfs-test | Out-Null; tar -xf C:\\Windows\\Temp\\icbfs-src.tar -C C:\\icbfs-test; cd C:\\icbfs-test; go build -o icbfs-winfsp.exe ./cmd/icbfs-winfsp; if (\$LASTEXITCODE -eq 0) { go build -o icbfs-cli.exe ./cmd/icbfs-cli }; if (\$LASTEXITCODE -eq 0) { go build -ldflags -H=windowsgui -o icbfs-sync.exe ./cmd/icbfs-sync }; echo BUILD_EXIT=\$LASTEXITCODE" 2>&1 | grep -v -i "$FILTER")
  echo "$out" >&2
  echo "$out" | grep -q "BUILD_EXIT=0" || { echo "VM build failed" >&2; exit 1; }
fi

fail=0
echo "Go tests on Windows..." >&2
ssh_vm "$IP" "cd C:\\icbfs-test; go test -count=1 ./internal/syncer ./internal/syncer/folderside 2>&1" 2>&1 | grep -v -i "$FILTER" | tee "$WORK_DIR/sync-gotest.txt"
grep -aq "^FAIL" "$WORK_DIR/sync-gotest.txt" && fail=1

scp_vm "$HERE/run-sync-test.ps1" "Administrator@${IP}:C:/Windows/Temp/run-sync-test.ps1" || exit 1
echo "End to end..." >&2
ssh_vm "$IP" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\run-sync-test.ps1 -Endpoint $ICBFS_MINIO_VM_ENDPOINT -Bucket $ICBFS_MINIO_BUCKET -AccessKey $ICBFS_MINIO_ACCESS_KEY -SecretKey $ICBFS_MINIO_SECRET_KEY; echo PS_EXIT=\$LASTEXITCODE" 2>&1 \
  | grep -v -i "$FILTER" | tee "$WORK_DIR/sync-e2e.txt"
grep -aq "PS_EXIT=0" "$WORK_DIR/sync-e2e.txt" || fail=1
exit $fail
