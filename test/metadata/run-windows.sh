#!/bin/bash
# The Windows copy tests on the Windows 11 test VM, as Administrator,
# against a throwaway MinIO: robocopy, xcopy, Copy-Item and a DISM image
# copy the test tree onto NTFS and onto a fresh icbfs mount, and the
# copies are compared. A few minutes. See README.md.
#
#   test/metadata/run-windows.sh
#
# ICBFS_WIN_VM picks the VM (default icbfs-win11-test). Results land in
# test/metadata/work/windows-<time>/, as run-linux.sh's do.
set -euo pipefail
META="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$META/../minio/with-minio.sh" "$META/run-windows.sh" "$@"
fi
export ICBFS_WIN_VM="${ICBFS_WIN_VM:-icbfs-win11-test}"
source "$META/../windows/lib.sh" # sets its own HERE
REPO="$(cd "$META/../.." && pwd)"
FILTER='warning\|post-quantum\|store now\|upgraded'
IP=$(vm_ip)
[ -n "$IP" ] || { echo "no IP for $VM_NAME; is it running?" >&2; exit 1; }
sync_vm_clock "$IP"
RESULTS="$META/work/windows-$(date +%Y%m%d%H%M%S)"
mkdir -p "$RESULTS" "$WORK_DIR"

echo "Deploying working tree to C:\\icbfs-test..." >&2
tarball="$WORK_DIR/icbfs-src.tar"
(cd "$REPO" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball")
scp_vm "$tarball" "Administrator@${IP}:C:/Windows/Temp/icbfs-src.tar"
ssh_vm "$IP" "Get-Process icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force; if (Test-Path C:\\icbfs-test) { Remove-Item C:\\icbfs-test -Recurse -Force }; New-Item -ItemType Directory C:\\icbfs-test | Out-Null; tar -xf C:\\Windows\\Temp\\icbfs-src.tar -C C:\\icbfs-test; cd C:\\icbfs-test; go build -o icbfs-winfsp.exe ./cmd/icbfs-winfsp; if (\$LASTEXITCODE -eq 0) { go build -o icbfs-cli.exe ./cmd/icbfs-cli }; if (\$LASTEXITCODE -eq 0) { go build -o metatree.exe ./test/metadata/metatree }; echo BUILD_EXIT=\$LASTEXITCODE" 2>&1 | grep -v -i "$FILTER" | grep -q 'BUILD_EXIT=0' || { echo "build on the VM failed" >&2; exit 1; }

# The Go tests of what keeps metadata on Windows, which need NTFS and
# WinFsp: the sync's folder side and the mount's security descriptors.
echo "Go tests on Windows (folder side, mount security)..." >&2
ssh_vm "$IP" "cd C:\\icbfs-test; go test ./internal/syncer/folderside ./internal/winfspserver -run 'NTFS|Merge|ReadWhileOpen|CreateSDDL|InheritOnly' -count=1 2>&1; echo GOTEST_EXIT=\$LASTEXITCODE" 2>&1 | grep -v -i "$FILTER" | tee "$RESULTS/go-test.txt" | grep -E '^(ok|FAIL|---)|GOTEST_EXIT'
grep -q 'GOTEST_EXIT=0' "$RESULTS/go-test.txt" || { echo "FAIL  the Go tests on Windows" >&2; exit 1; }

# The store's flags and keys reach the VM in a file, never on a command
# line; the script deletes it once read.
envfile="$WORK_DIR/metadata-store.env"
( umask 077; printf 'ICBFS_STORE_ARGS=-store s3 -endpoint %s -bucket %s\nICBFS_ACCESS_KEY=%s\nICBFS_SECRET_KEY=%s\n' \
  "$ICBFS_MINIO_VM_ENDPOINT" "$ICBFS_MINIO_BUCKET" "$ICBFS_MINIO_ACCESS_KEY" "$ICBFS_MINIO_SECRET_KEY" > "$envfile" )
scp_vm "$envfile" "Administrator@${IP}:C:/Windows/Temp/metadata-store.env"
rm -f "$envfile"

# On the logged-on desktop (auto-logon, run-in-desktop.ps1), not in this
# SSH session, as a person's copies would run; the mount must be in the
# same session as the copies. It writes exit.txt when it's done.
ssh_vm "$IP" "Remove-Item C:\\Windows\\Temp\\metadata-results -Recurse -Force -ErrorAction SilentlyContinue; powershell -NoProfile -ExecutionPolicy Bypass -File C:\\icbfs-test\\test\\windows\\run-in-desktop.ps1 -Exe C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe -Arguments '-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File C:\\icbfs-test\\test\\metadata\\copy-tests-windows.ps1 -Src C:\\icbfs-test -EnvFile C:\\Windows\\Temp\\metadata-store.env'" 2>&1 | grep -v -i "$FILTER" || true
rc=
for _ in $(seq 1 120); do
  sleep 10
  rc=$(ssh_vm "$IP" "Get-Content C:\\Windows\\Temp\\metadata-results\\exit.txt -ErrorAction SilentlyContinue" 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' || true)
  [ -n "$rc" ] && break
done
ssh_vm "$IP" "Get-Content C:\\Windows\\Temp\\metadata-results\\summary.txt -ErrorAction SilentlyContinue" 2>&1 | grep -v -i "$FILTER" | tr -d '\r'
if [ -z "$rc" ]; then
  echo "FAIL  no result after 20 minutes" >&2
  rc=2
fi
ssh_vm "$IP" "tar -cf C:\\Windows\\Temp\\metadata-results.tar -C C:\\Windows\\Temp\\metadata-results ." >/dev/null 2>&1
scp_vm "Administrator@${IP}:C:/Windows/Temp/metadata-results.tar" "$RESULTS/results.tar" && tar -C "$RESULTS" -xf "$RESULTS/results.tar" && rm "$RESULTS/results.tar"
echo "results: $RESULTS" >&2
exit "$rc"
