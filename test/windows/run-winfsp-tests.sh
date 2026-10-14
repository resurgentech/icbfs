#!/bin/bash
# Runs winfsp-tests (--external) against icbfs on the Windows VM.
#
#   run-winfsp-tests.sh [--no-deploy] [target] [-- extra args for run-winfsp-tests.ps1]
#
# target is icbfs-ci (default), icbfs-cs, cmemfs-ci or cmemfs-cs — see
# run-winfsp-tests.ps1. Extra args go to that script, e.g.
#   run-winfsp-tests.sh icbfs-cs -- -Only querydir_test,getfileinfo_test
#
# Copies this working tree (tracked plus untracked-but-not-ignored files)
# to C:\icbfs-test on the VM and builds icbfs-winfsp.exe there, unless
# --no-deploy. Storage is a throwaway MinIO (test/minio/with-minio.sh)
# that exists only for this run: started here, removed when the script
# exits however it exits. Results land in $WORK_DIR/winfsp-tests-<target>-<time>.txt.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-winfsp-tests.sh" "$@"
fi
source "$HERE/lib.sh"
REPO="$(cd "$HERE/../.." && pwd)"
FILTER='warning\|post-quantum\|store now\|upgraded'

deploy=1
if [ "${1:-}" = "--no-deploy" ]; then deploy=0; shift; fi
TARGET=icbfs-ci
if [ $# -gt 0 ] && [ "$1" != "--" ]; then TARGET="$1"; shift; fi
[ "${1:-}" = "--" ] && shift

IP=$(vm_ip)
[ -n "$IP" ] || { echo "no IP for $VM_NAME — is it running?" >&2; exit 1; }
scp_vm() {
  sshpass -p "$VM_PASSWORD" scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$@"
}
mkdir -p "$WORK_DIR"

if [ "$deploy" = 1 ]; then
  echo "Deploying working tree to C:\\icbfs-test..." >&2
  tarball="$WORK_DIR/icbfs-src.tar"
  (cd "$REPO" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball") || exit 1
  scp_vm "$tarball" "Administrator@${IP}:C:/Windows/Temp/icbfs-src.tar" || { echo "deploy failed" >&2; exit 1; }
  out=$(ssh_vm "$IP" "if (Test-Path C:\\icbfs-test) { Remove-Item C:\\icbfs-test -Recurse -Force }; New-Item -ItemType Directory C:\\icbfs-test | Out-Null; tar -xf C:\\Windows\\Temp\\icbfs-src.tar -C C:\\icbfs-test; cd C:\\icbfs-test; go build -o icbfs-winfsp.exe ./cmd/icbfs-winfsp; if (\$LASTEXITCODE -eq 0) { go build -o icbfs-cli.exe ./cmd/icbfs-cli }; echo BUILD_EXIT=\$LASTEXITCODE" 2>&1 | grep -v -i "$FILTER")
  echo "$out" >&2
  echo "$out" | grep -q "BUILD_EXIT=0" || { echo "VM build failed — not running tests" >&2; exit 1; }
fi

scp_vm "$HERE/run-winfsp-tests.ps1" "Administrator@${IP}:C:/Windows/Temp/run-winfsp-tests.ps1" || exit 1
result="$WORK_DIR/winfsp-tests-${TARGET}-$(date +%Y%m%d%H%M%S).txt"
echo "Running winfsp-tests against $TARGET (one fresh mount per test; ~10-20 minutes)..." >&2
ssh_vm "$IP" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\run-winfsp-tests.ps1 -Target $TARGET -Endpoint $ICBFS_MINIO_VM_ENDPOINT -Bucket $ICBFS_MINIO_BUCKET -AccessKey $ICBFS_MINIO_ACCESS_KEY -SecretKey $ICBFS_MINIO_SECRET_KEY $*" 2>&1 \
  | grep -v -i "$FILTER" > "$result"

echo "" >&2
echo "=== winfsp-tests $TARGET (full result: $result) ===" >&2
# winfsp-tests pads names with dots to a fixed column, so a long name
# gets just one.
ok=$(grep -cE '^[a-z0-9_]+\.+ OK ' "$result")
# The runner's own lines ("name ... MOUNT-FAILED") have a space before the dots.
bad=$(grep -E '^[a-z0-9_]+ ?\.+ (KO|TIMEOUT|MOUNT-FAILED)' "$result")
if [ "$ok" -eq 0 ] && [ -z "$bad" ]; then
  bad="no test passed (no results?)"
fi
echo "OK: $ok" >&2
if [ -n "$bad" ]; then
  echo "$bad"
  exit 1
fi
